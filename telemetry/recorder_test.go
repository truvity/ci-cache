package telemetry

import (
	"sync"
	"testing"
	"time"
)

// countingHook stands in for the exporter's synchronous instruments.
//
// The real one forwards to an OTel histogram; what matters for the test is
// that it sees exactly the events the Recorder stored, because that is the
// property the package promises -- one set of numbers, observed twice, rather
// than two sets incremented in parallel.
type countingHook struct {
	mu       sync.Mutex
	observed map[opKey]int64
	total    map[opKey]time.Duration
	gcRuns   int64
	gcTotal  time.Duration
}

func newCountingHook() *countingHook {
	return &countingHook{observed: map[opKey]int64{}, total: map[opKey]time.Duration{}}
}

func (h *countingHook) Observe(frontend, tier, op string, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := opKey{frontend, tier, op}
	h.observed[k]++
	h.total[k] += d
}

func (h *countingHook) GCDuration(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gcRuns++
	h.gcTotal += d
}

// TestSnapshotAgreesWithTheHookAfterAThousandOperations is the "same numbers"
// assertion.
//
// The exporter reads Snapshot for everything countable and the hook for the
// two histograms; if those two ever disagree, a dashboard and `ci-cache
// stats` report different caches and there is no way to tell which is right.
// A thousand operations across eight goroutines, because the disagreement
// this guards against is a lost increment under contention.
func TestSnapshotAgreesWithTheHookAfterAThousandOperations(t *testing.T) {
	t.Parallel()

	const (
		goroutines = 8
		perG       = 125 // 8 * 125 = 1000
	)

	hook := newCountingHook()
	rec := newRecorder(hook)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range perG {
				// A deterministic mix: every goroutine runs the same script,
				// so the totals are arithmetic rather than a guess.
				switch (g + i) % 4 {
				case 0:
					rec.Get("go/build", "disk", OutcomeHit)
					rec.Bytes("go/build", "disk", DirectionRead, 10)
				case 1:
					rec.Get("go/build", "disk", OutcomeMiss)
				case 2:
					rec.Put("go/build", "bucket", OutcomeMiss)
					rec.Bytes("go/build", "bucket", DirectionWritten, 100)
				default:
					rec.Get("nix", "bucket", OutcomeError)
				}
				rec.Observe("go/build", "disk", OpGet, time.Millisecond)
			}
		}(g)
	}
	wg.Wait()

	const total = goroutines * perG
	snap := rec.Snapshot()

	var gets, puts, errs, bytesRead, bytesWritten int64
	for _, f := range snap.Frontends {
		for _, ti := range f.Tiers {
			gets += ti.Gets
			puts += ti.Puts
			errs += ti.Errors
			bytesRead += ti.BytesRead
			bytesWritten += ti.BytesWritten
		}
	}

	// Three of the four branches are reads and one is a write.
	if want := int64(total / 4 * 3); gets != want {
		t.Errorf("gets = %d, want %d", gets, want)
	}
	if want := int64(total / 4); puts != want {
		t.Errorf("puts = %d, want %d", puts, want)
	}
	if want := int64(total / 4); errs != want {
		t.Errorf("errors = %d, want %d", errs, want)
	}
	if want := int64(total / 4 * 10); bytesRead != want {
		t.Errorf("bytesRead = %d, want %d", bytesRead, want)
	}
	if want := int64(total / 4 * 100); bytesWritten != want {
		t.Errorf("bytesWritten = %d, want %d", bytesWritten, want)
	}

	// The latency the snapshot kept and the latency the exporter saw are the
	// same events; a hook that missed one would export a histogram that
	// disagrees with the totals beside it.
	var ops OpSnapshot
	for _, f := range snap.Frontends {
		for _, o := range f.Ops {
			if f.Frontend == "go/build" && o.Tier == "disk" && o.Op == OpGet {
				ops = o
			}
		}
	}
	if ops.Count != total {
		t.Errorf("snapshot observed %d get latencies, want %d", ops.Count, total)
	}
	hook.mu.Lock()
	k := opKey{"go/build", "disk", OpGet}
	hookCount, hookTotal := hook.observed[k], hook.total[k]
	hook.mu.Unlock()
	if hookCount != ops.Count {
		t.Errorf("hook saw %d get latencies, snapshot kept %d", hookCount, ops.Count)
	}
	if hookTotal != ops.Total {
		t.Errorf("hook summed %v, snapshot summed %v", hookTotal, ops.Total)
	}
}

// TestSnapshotIsSorted is what makes two snapshots comparable.
func TestSnapshotIsSorted(t *testing.T) {
	t.Parallel()

	rec := NewRecorder()
	for _, f := range []string{"nix", "go/build", "maven"} {
		for _, ti := range []string{"remote", "bucket", "disk"} {
			rec.Get(f, ti, OutcomeHit)
			rec.Observe(f, ti, OpPut, time.Millisecond)
			rec.Observe(f, ti, OpGet, time.Millisecond)
		}
	}

	snap := rec.Snapshot()
	var frontends []string
	for _, f := range snap.Frontends {
		frontends = append(frontends, f.Frontend)
		var tiers []string
		for _, ti := range f.Tiers {
			tiers = append(tiers, ti.Tier)
		}
		if got, want := tiers, []string{"bucket", "disk", "remote"}; !equal(got, want) {
			t.Errorf("%s tiers = %v, want %v", f.Frontend, got, want)
		}
		var ops []string
		for _, o := range f.Ops {
			ops = append(ops, o.Tier+"/"+o.Op)
		}
		want := []string{"bucket/get", "bucket/put", "disk/get", "disk/put", "remote/get", "remote/put"}
		if !equal(ops, want) {
			t.Errorf("%s ops = %v, want %v", f.Frontend, ops, want)
		}
	}
	if want := []string{"go/build", "maven", "nix"}; !equal(frontends, want) {
		t.Errorf("frontends = %v, want %v", frontends, want)
	}
}

// TestGCIsProcessWide checks the one counter that belongs to the volume
// rather than to a front-end.
func TestGCIsProcessWide(t *testing.T) {
	t.Parallel()

	hook := newCountingHook()
	rec := newRecorder(hook)
	rec.GC(1024, 3*time.Millisecond)
	rec.GC(2048, 5*time.Millisecond)

	got := rec.Snapshot().GC
	if got.Runs != 2 || got.EvictedBytes != 3072 || got.Total != 8*time.Millisecond {
		t.Errorf("gc = %+v, want {Runs:2 EvictedBytes:3072 Total:8ms}", got)
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.gcRuns != 2 || hook.gcTotal != 8*time.Millisecond {
		t.Errorf("hook gc = %d runs / %v, want 2 / 8ms", hook.gcRuns, hook.gcTotal)
	}
}

// TestDroppedIsNotAnError is the distinction an alert depends on.
func TestDroppedIsNotAnError(t *testing.T) {
	t.Parallel()

	rec := NewRecorder()
	rec.Put("nix", "bucket", OutcomeDropped)
	rec.Put("nix", "bucket", OutcomeError)

	ti := rec.Snapshot().Frontends[0].Tiers[0]
	if ti.Dropped != 1 {
		t.Errorf("dropped = %d, want 1", ti.Dropped)
	}
	if ti.Errors != 1 {
		t.Errorf("errors = %d, want 1 (a dropped put is not a failure)", ti.Errors)
	}
	if ti.Puts != 2 {
		t.Errorf("puts = %d, want 2", ti.Puts)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
