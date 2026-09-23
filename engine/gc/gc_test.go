package gc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/disk"
	"github.com/truvity/ci-cache/engine/tier"
)

// --- helpers ----------------------------------------------------------

func newDisk(t *testing.T) *disk.Disk {
	t.Helper()
	// fsync off and no index flushing: this package's tests are about which
	// objects survive, and paying a sync per object would make a four
	// hundred object case slower than the whole suite.
	d, err := disk.New(t.TempDir(), disk.WithFsync(false), disk.WithIndexFlush(0))
	if err != nil {
		t.Fatalf("disk.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func mustPut(t *testing.T, d *disk.Disk, key string, body []byte) {
	t.Helper()
	if err := d.Put(context.Background(), key, bytes.NewReader(body), tier.Meta{Size: int64(len(body))}); err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
}

func present(t *testing.T, d *disk.Disk, key string) bool {
	t.Helper()
	_, err := d.Stat(context.Background(), key)
	switch {
	case err == nil:
		return true
	case errors.Is(err, tier.ErrNotFound):
		return false
	default:
		t.Fatalf("Stat(%s): %v", key, err)
		return false
	}
}

// unitOf puts one object and reports what the volume spent on it.
//
// The header's size is the disk tier's business and is not exported, so the
// budgets here are expressed in whole objects and measured rather than
// assumed. A test that hard-coded the overhead would start failing the day
// the header gained a field, which is not a failure worth having.
func unitOf(t *testing.T, d *disk.Disk, key string, body []byte) int64 {
	t.Helper()
	mustPut(t, d, key, body)
	u := d.Used()
	if u <= 0 {
		t.Fatalf("Used after one Put: got %d", u)
	}
	return u
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// --- eviction -----------------------------------------------------------

// TestCollectEvictsToLowAndKeepsWhatWasReadLast is the proof that the
// collector is LRU and not FIFO.
//
// The keys it expects to survive are the ten OLDEST writes on the volume.
// Under FIFO they would be the first ten to go; they live only because they
// were the last ten things READ, which is the whole distinction the index
// exists to make.
func TestCollectEvictsToLowAndKeepsWhatWasReadLast(t *testing.T) {
	t.Parallel()
	const (
		objects     = 400
		budgetUnits = 200
		hot         = 10
	)
	d := newDisk(t)
	body := bytes.Repeat([]byte{0x5a}, 1024)
	key := func(i int) string { return fmt.Sprintf("go/build/action/%03d", i) }

	unit := unitOf(t, d, key(0), body)
	budget := int64(budgetUnits) * unit
	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: budget})

	for i := 1; i < objects; i++ {
		mustPut(t, d, key(i), body)
	}
	if got, want := d.Used(), int64(objects)*unit; got != want {
		t.Fatalf("Used before collecting: got %d, want %d", got, want)
	}

	// Read the ten oldest. Nothing has been evicted yet -- no space manager
	// is wired to this tier -- so this is purely a statement about recency.
	for i := range hot {
		rc, _, err := d.Get(context.Background(), key(i))
		if err != nil {
			t.Fatalf("Get(%s): %v", key(i), err)
		}
		_ = rc.Close()
	}

	g.Collect()

	low := pct(budget, g.low())
	if got := d.Used(); got > low {
		t.Fatalf("Used after a pass: got %d, want at most the low watermark %d", got, low)
	}
	if got, want := g.Runs(), int64(1); got != want {
		t.Errorf("Runs: got %d, want %d", got, want)
	}
	if want := int64(objects)*unit - d.Used(); g.EvictedBytes() != want {
		t.Errorf("EvictedBytes: got %d, want %d", g.EvictedBytes(), want)
	}

	for i := range hot {
		if !present(t, d, key(i)) {
			t.Errorf("%s was evicted: the collector is ordering by write, not by use", key(i))
		}
	}
	// The middle of the volume -- old writes that nothing read -- is what
	// should have gone.
	for _, i := range []int{hot + 5, 100, 200} {
		if present(t, d, key(i)) {
			t.Errorf("%s survived, but nothing has touched it since it was written", key(i))
		}
	}
	// The newest writes stay: they are recent by both measures.
	for _, i := range []int{objects - 1, objects - 50} {
		if !present(t, d, key(i)) {
			t.Errorf("%s was evicted, though it is one of the newest objects on the volume", key(i))
		}
	}
}

func TestCollectDoesNothingBelowTheHighWatermark(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	body := bytes.Repeat([]byte{1}, 256)
	unit := unitOf(t, d, "go/build/000", body)
	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: 100 * unit})

	for i := 1; i < 50; i++ {
		mustPut(t, d, fmt.Sprintf("go/build/%03d", i), body)
	}
	before := d.Used()
	g.Collect()
	if d.Used() != before {
		t.Errorf("a cache at half its budget was evicted anyway: %d -> %d", before, d.Used())
	}
	if g.EvictedBytes() != 0 {
		t.Errorf("EvictedBytes: got %d, want 0", g.EvictedBytes())
	}
	if g.Runs() != 1 {
		t.Errorf("Runs: got %d, want 1 -- a pass that finds nothing is still a pass", g.Runs())
	}
}

// TestSubBudgetEvictsTheOverrunningFrontendFirst holds the rule that one
// front-end cannot spend another's share.
//
// The volume as a whole is well inside its high watermark here, so nothing
// would be evicted at all but for the cap. Without it, a Go build cache that
// grew without bound would eventually push the volume over its watermark and
// take every Nix NAR with it.
func TestSubBudgetEvictsTheOverrunningFrontendFirst(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	body := bytes.Repeat([]byte{2}, 512)
	unit := unitOf(t, d, "go/build/000", body)
	budget := 200 * unit

	for i := 1; i < 40; i++ {
		mustPut(t, d, fmt.Sprintf("go/build/%03d", i), body)
	}
	for i := range 40 {
		mustPut(t, d, fmt.Sprintf("nix/nar/%03d", i), body)
	}

	g := New(d, config.GC{
		Floor:       10,
		High:        95,
		Low:         85,
		BudgetBytes: budget,
		Budgets:     map[string]int{"go": 10}, // 10% of 200 units = 20 units
	})
	if used := d.Used(); used > pct(budget, g.high()) {
		t.Fatalf("the test is not testing the cap: %d bytes is already over the high watermark", used)
	}

	g.Collect()

	goEntries, goBytes := d.UsedBy("go")
	if want := pct(budget, 10); goBytes > want {
		t.Errorf("go holds %d bytes (%d entries) after the pass, over its cap of %d", goBytes, goEntries, want)
	}
	if nixEntries, _ := d.UsedBy("nix"); nixEntries != 40 {
		t.Errorf("nix lost %d entries to another front-end's overrun", 40-nixEntries)
	}
	if g.EvictedBytes() == 0 {
		t.Error("nothing was evicted, though go was at twice its cap")
	}
}

func TestSubBudgetIgnoresNonsensePercentages(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	body := bytes.Repeat([]byte{3}, 128)
	unit := unitOf(t, d, "go/build/000", body)
	for i := 1; i < 20; i++ {
		mustPut(t, d, fmt.Sprintf("go/build/%03d", i), body)
	}
	g := New(d, config.GC{
		Floor: 10, High: 95, Low: 85, BudgetBytes: 1000 * unit,
		Budgets: map[string]int{"go": 0, "nix": -5, "maven": 400},
	})
	before := d.Used()
	g.Collect()
	if d.Used() != before {
		t.Errorf("a cap of 0 or a negative one was read as a real limit: %d -> %d", before, d.Used())
	}
}

// --- admission ----------------------------------------------------------

func TestAdmitRefusesAnObjectLargerThanTheBudget(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	body := bytes.Repeat([]byte{4}, 512)
	unit := unitOf(t, d, "go/build/000", body)

	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: 10 * unit})
	d.SetSpaceManager(g)

	big := bytes.Repeat([]byte{5}, int(20*unit))
	err := d.Put(context.Background(), "gradle/build/enormous", bytes.NewReader(big), tier.Meta{Size: int64(len(big))})
	if !errors.Is(err, tier.ErrNoSpace) {
		t.Fatalf("Put of an object larger than the whole budget: got %v, want ErrNoSpace", err)
	}
	// Refusing must not have emptied the cache for an object that could
	// never have fitted anyway.
	if !present(t, d, "go/build/000") {
		t.Error("the cache was evicted to make room for something that cannot fit")
	}
}

func TestAdmitMakesRoomForAnObjectThatFits(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	body := bytes.Repeat([]byte{6}, 512)
	unit := unitOf(t, d, "go/build/000", body)

	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: 10 * unit})
	d.SetSpaceManager(g)

	for i := 1; i < 30; i++ {
		mustPut(t, d, fmt.Sprintf("go/build/%03d", i), body)
	}
	if used, budget := d.Used(), g.Budget(); used > budget {
		t.Fatalf("the volume went past its budget: %d > %d", used, budget)
	}
	if !present(t, d, "go/build/029") {
		t.Error("the object that triggered the eviction was not stored")
	}
	if g.EvictedBytes() == 0 {
		t.Error("thirty objects into a ten-object budget and nothing was evicted")
	}
}

// --- budget -------------------------------------------------------------

func TestBudgetAppliesTheFloorToTheMeasuredVolume(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cfg   config.GC
		total int64
		want  int64
	}{
		{"floor of 10 percent", config.GC{Floor: 10, High: 95, Low: 85}, 1000, 900},
		{"no floor", config.GC{Floor: 0, High: 95, Low: 85}, 1000, 1000},
		{"an explicit budget wins over statfs", config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: 512}, 1000, 512},
		{"a wholly empty config is not a zero floor", config.GC{}, 1000, 900},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDisk(t)
			g := New(d, tc.cfg, WithCapacity(func(string) (int64, error) { return tc.total, nil }))
			if got := g.Budget(); got != tc.want {
				t.Errorf("Budget: got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBudgetSurvivesAnUnreadableFilesystem holds the rule that a monitoring
// failure must not become a cache outage.
func TestBudgetSurvivesAnUnreadableFilesystem(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	g := New(d, config.GC{Floor: 10, High: 95, Low: 85},
		WithCapacity(func(string) (int64, error) { return 0, errors.New("statfs: no") }))
	if got := g.Budget(); got != fallbackBudget {
		t.Fatalf("Budget with no measurement: got %d, want the fallback %d", got, fallbackBudget)
	}
	if err := g.Admit(1 << 20); err != nil {
		t.Errorf("Admit with an unmeasurable filesystem: got %v, want nil", err)
	}
}

func TestBudgetIsReMeasured(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	var total atomic.Int64
	total.Store(1000)
	g := New(d, config.GC{Floor: 10, High: 95, Low: 85},
		WithCapacity(func(string) (int64, error) { return total.Load(), nil }),
		WithBudgetRefresh(time.Millisecond),
		WithInterval(time.Hour),
		WithNegativeInterval(time.Hour))
	if got := g.Budget(); got != 900 {
		t.Fatalf("initial Budget: got %d, want 900", got)
	}

	g.Start(context.Background())
	defer g.Stop()
	// The volume was resized under the process, which is the case this
	// exists for: nothing else tells the server that its PVC grew.
	total.Store(2000)
	waitFor(t, "the budget to follow the volume", func() bool { return g.Budget() == 1800 })
}

func TestStatfsMeasuresTheRealVolume(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	total, err := fsCapacity(d.Dir())
	if err != nil {
		t.Fatalf("fsCapacity(%s): %v", d.Dir(), err)
	}
	if total <= 0 {
		t.Fatalf("fsCapacity: got %d, want a positive size", total)
	}
	g := New(d, config.GC{Floor: 10, High: 95, Low: 85})
	if got, want := g.Budget(), pct(total, 90); got != want {
		t.Errorf("Budget from statfs: got %d, want %d", got, want)
	}
}

// --- the background loop ------------------------------------------------

func TestNotifyNeverBlocks(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: 1 << 30})
	// No Start, so nothing is draining the channel. A Notify that blocked
	// here would be a Put that blocked in production.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			g.Notify()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blocked: a Put would have blocked with it")
	}
}

func TestBackgroundLoopKeepsTheVolumeInsideItsBudget(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	body := bytes.Repeat([]byte{7}, 1024)
	unit := unitOf(t, d, "go/build/000", body)
	budget := 50 * unit

	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: budget},
		WithInterval(5*time.Millisecond), WithNegativeInterval(time.Hour), WithBudgetRefresh(time.Hour))
	d.SetSpaceManager(g)
	g.Start(context.Background())
	defer g.Stop()

	for i := 1; i < 400; i++ {
		mustPut(t, d, fmt.Sprintf("go/build/%03d", i), body)
		// The invariant a writer depends on: the volume never goes past its
		// budget, and no Put ever had to wait for a background pass to make
		// that true.
		if used := d.Used(); used > budget {
			t.Fatalf("after put %d: used %d, budget %d", i, used, budget)
		}
	}
	waitFor(t, "the background loop to settle under the high watermark", func() bool {
		return d.Used() <= pct(budget, g.high())
	})
	if g.Runs() == 0 {
		t.Error("the collector never ran")
	}
}

func TestNegativeEntriesExpireOnTheTicker(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	for i := range 20 {
		d.PutNegative(fmt.Sprintf("maven/central/absent-%02d", i), 10*time.Millisecond)
	}
	d.PutNegative("maven/central/long-lived", time.Hour)
	if got, want := d.Negatives(), int64(21); got != want {
		t.Fatalf("Negatives: got %d, want %d", got, want)
	}

	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: 1 << 30},
		WithNegativeInterval(time.Millisecond), WithInterval(time.Hour), WithBudgetRefresh(time.Hour))
	g.Start(context.Background())
	defer g.Stop()

	// Nothing asks about these keys again, so only the ticker can reclaim
	// them. Without it a burst of one-shot lookups is remembered for the
	// life of the process.
	waitFor(t, "expired negative entries to be swept", func() bool { return d.Negatives() == 1 })
	if !d.GetNegative("maven/central/long-lived") {
		t.Error("the sweep took a negative entry that had not expired")
	}
}

func TestStopIsIdempotentAndStopsTheLoop(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: 1 << 30}, WithInterval(time.Millisecond))
	g.Start(context.Background())
	g.Stop()
	g.Stop()
	runs := g.Runs()
	time.Sleep(20 * time.Millisecond)
	if g.Runs() != runs {
		t.Error("the collector is still running after Stop")
	}
}

func TestStartRespectsContextCancellation(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	ctx, cancel := context.WithCancel(context.Background())
	g := New(d, config.GC{Floor: 10, High: 95, Low: 85, BudgetBytes: 1 << 30}, WithInterval(time.Millisecond))
	g.Start(ctx)
	cancel()
	// Stop must return rather than hang: the goroutine has already left
	// through the ctx.Done arm, and the WaitGroup is what proves it.
	done := make(chan struct{})
	go func() { defer close(done); g.Stop() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung after the context was cancelled")
	}
}

// --- watermarks ---------------------------------------------------------

func TestWatermarksFallBackOnNonsense(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	cases := []struct {
		name              string
		cfg               config.GC
		wantHigh, wantLow int
	}{
		{"as configured", config.GC{Floor: 10, High: 90, Low: 70}, 90, 70},
		{"nothing said", config.GC{}, defaultHigh, defaultLow},
		// Only the nonsensical value is replaced. A low of 80 is perfectly
		// usable once high has fallen back to 95, and throwing it away too
		// would silently ignore the one thing the operator got right.
		{"low above high", config.GC{High: 50, Low: 80}, defaultHigh, 80},
		{"high over 100", config.GC{High: 150, Low: 80}, defaultHigh, 80},
		{"both nonsense", config.GC{High: 50, Low: 200}, defaultHigh, defaultLow},
		{"low at zero", config.GC{High: 90, Low: 0}, 90, defaultLow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := New(d, tc.cfg, WithCapacity(func(string) (int64, error) { return 1 << 30, nil }))
			if got := g.high(); got != tc.wantHigh {
				t.Errorf("high: got %d, want %d", got, tc.wantHigh)
			}
			if got := g.low(); got != tc.wantLow {
				t.Errorf("low: got %d, want %d", got, tc.wantLow)
			}
			if g.low() >= g.high() {
				t.Errorf("low %d is not below high %d: eviction would never terminate", g.low(), g.high())
			}
		})
	}
}

func TestPctDoesNotOverflow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v       int64
		percent int
		want    int64
	}{
		{1000, 90, 900},
		{0, 90, 0},
		{fallbackBudget, 90, (fallbackBudget / 100) * 90},
		{1 << 40, 85, (1 << 40) * 85 / 100},
	}
	for _, tc := range cases {
		if got := pct(tc.v, tc.percent); got != tc.want {
			t.Errorf("pct(%d, %d): got %d, want %d", tc.v, tc.percent, got, tc.want)
		}
		if got := pct(tc.v, tc.percent); got < 0 {
			t.Errorf("pct(%d, %d) overflowed to %d", tc.v, tc.percent, got)
		}
	}
}
