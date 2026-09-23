package bench

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truvity/ci-cache/engine/tier"
)

// A benchmark whose corpus moves between runs compares two things at once
// and attributes the difference to whichever one you were thinking about.
func TestCorpusIsDeterministic(t *testing.T) {
	a := NewCorpus(500, 7, "")
	b := NewCorpus(500, 7, "")

	if len(a.Objects) != len(b.Objects) || a.Bytes() != b.Bytes() {
		t.Fatalf("same seed gave %d objects / %d bytes and %d objects / %d bytes",
			len(a.Objects), a.Bytes(), len(b.Objects), b.Bytes())
	}

	for i := range a.Objects {
		if a.Objects[i] != b.Objects[i] {
			t.Fatalf("object %d differs: %+v vs %+v", i, a.Objects[i], b.Objects[i])
		}
	}

	if c := NewCorpus(500, 8, ""); c.Bytes() == a.Bytes() {
		t.Error("a different seed produced the same corpus; the seed is not reaching the generator")
	}
}

// The workload's shape is the measured one, and it is not a knob. Two
// requests per lookup, one tiny and one large, because that is what a Go
// build does and a uniform corpus would measure neither half.
func TestCorpusHasTheMeasuredShape(t *testing.T) {
	c := NewCorpus(1000, 1, "")

	if got, want := len(c.Objects), 2*len(c.Lookups); got != want {
		t.Fatalf("%d objects for %d lookups, want %d", got, len(c.Lookups), want)
	}

	var records, outputs int

	for _, o := range c.Objects {
		if o.Record {
			records++

			if o.Size != ActionRecordSize {
				t.Fatalf("action record is %d bytes, want %d: the record is a fixed format", o.Size, ActionRecordSize)
			}

			continue
		}

		outputs++

		if o.Size <= ActionRecordSize {
			t.Errorf("output %s is %d bytes, no bigger than a record: the two classes have collapsed", o.Key, o.Size)
		}

		if o.Size > outputCap {
			t.Errorf("output %s is %d bytes, past the cap: one object would dominate a run", o.Key, o.Size)
		}
	}

	if records != outputs {
		t.Fatalf("%d records and %d outputs; a lookup is one of each", records, outputs)
	}

	// The sharded key layout only spreads if the ids do. All-same prefixes
	// would put the corpus in one directory and measure that instead.
	shards := map[string]bool{}
	for _, o := range c.Objects {
		parts := strings.Split(o.Key, "/")
		shards[parts[len(parts)-2]] = true
	}

	if len(shards) < 100 {
		t.Errorf("only %d distinct shards across %d objects; the ids are not spreading", len(shards), len(c.Objects))
	}
}

// Compressible is swept rather than assumed, so the two ends have to differ.
func TestBodyCompressibilityIsAKnob(t *testing.T) {
	o := Object{Size: 64 << 10, Seed: 3}

	noise := o.Body(0)
	runs := o.Body(1)

	if len(noise) != len(runs) || len(noise) != int(o.Size) {
		t.Fatalf("bodies are %d and %d bytes, want %d", len(noise), len(runs), o.Size)
	}

	if bytes.Equal(noise, runs) {
		t.Fatal("compressible 0 and 1 produced identical bytes; the knob does nothing")
	}

	// Cheap proxy for compressibility without pulling in a codec: count
	// distinct bytes. Noise uses nearly all 256; a run of 7 repeating values
	// uses 7.
	if d := distinct(runs); d > 16 {
		t.Errorf("compressible=1 body has %d distinct bytes; it should be highly repetitive", d)
	}

	if d := distinct(noise); d < 200 {
		t.Errorf("compressible=0 body has only %d distinct bytes; it should be noise", d)
	}
}

func distinct(b []byte) int {
	var seen [256]bool
	for _, c := range b {
		seen[c] = true
	}

	n := 0

	for _, s := range seen {
		if s {
			n++
		}
	}

	return n
}

// Percentiles are exact because p99 is the number a change has to move, and
// an estimator's p99 is the number plus an apology.
func TestPercentilesAreExact(t *testing.T) {
	xs := make([]float64, 100)
	for i := range xs {
		xs[i] = float64(i + 1)
	}

	p50, p95, p99 := percentiles(xs)
	if p50 != 50 || p95 != 95 || p99 != 99 {
		t.Fatalf("p50/p95/p99 = %v/%v/%v, want 50/95/99", p50, p95, p99)
	}

	if a, b, c := percentiles(nil); a != 0 || b != 0 || c != 0 {
		t.Fatalf("empty input gave %v/%v/%v", a, b, c)
	}
}

// Errors are excluded from the latency distribution and counted separately.
// Folding a fast failure into p50 makes a broken server look quick.
func TestErrorsDoNotEnterTheDistribution(t *testing.T) {
	r := Summarise("t", 4, time.Second, []Sample{
		{FirstByte: 10 * time.Millisecond, Complete: 20 * time.Millisecond, Bytes: 100},
		{FirstByte: time.Microsecond, Complete: time.Microsecond, Err: errors.New("refused")},
	})

	if r.Errors != 1 || r.Requests != 2 {
		t.Fatalf("errors=%d requests=%d, want 1 and 2", r.Errors, r.Requests)
	}

	if r.FirstByteP50 != 10 {
		t.Errorf("p50 first byte is %v ms; the failed request was counted", r.FirstByteP50)
	}

	if r.Bytes != 100 {
		t.Errorf("bytes = %d; a failed request contributed some", r.Bytes)
	}
}

// The two traps that make a broken run look like a fast one. Both report
// zero at every percentile, which reads like the best result the bench has
// ever produced.
func TestARunThatMeasuredNothingIsAnError(t *testing.T) {
	ctx := context.Background()
	corpus := NewCorpus(4, 1, "")

	t.Run("no requests at all", func(t *testing.T) {
		// A duration this short means the workers see a cancelled context
		// before their first fetch returns.
		_, err := Run(ctx, Config{
			Scenario: WarmDisk, Concurrency: 1, Duration: time.Nanosecond, Corpus: corpus,
		}, &stubStore{}, nil)
		if err == nil {
			t.Fatal("a run with no completed requests reported success")
		}

		if !strings.Contains(err.Error(), "measured nothing") {
			t.Errorf("error is %q; it should say the run measured nothing", err)
		}
	})

	t.Run("every request failed", func(t *testing.T) {
		_, err := Run(ctx, Config{
			Scenario: WarmDisk, Concurrency: 2, Duration: 30 * time.Millisecond, Corpus: corpus,
		}, &stubStore{getErr: errors.New("connection refused")}, nil)
		if err == nil {
			t.Fatal("a run where every request failed reported success")
		}

		if !strings.Contains(err.Error(), "failed") {
			t.Errorf("error is %q; it should say the requests failed", err)
		}
	})
}

// A scenario that cannot reach its starting state must say so, not quietly
// run a different scenario. cold-bucket without a wiper IS warm-disk, and
// the numbers would be attributed to the wrong thing forever.
func TestAScenarioThatCannotBePreparedRefuses(t *testing.T) {
	_, err := Run(context.Background(), Config{
		Scenario: ColdBucket, Concurrency: 1, Duration: time.Second, Corpus: NewCorpus(2, 1, ""),
	}, &stubStore{}, nil)
	if err == nil {
		t.Fatal("cold-bucket ran without a way to wipe the disk")
	}

	if !strings.Contains(err.Error(), "admin") {
		t.Errorf("error is %q; it should name what is missing", err)
	}
}

// The admin API reads a prefix as a prefix only when it ends in "/".
// Without the slash the wipe is an exact-key delete that matches nothing,
// and cold-bucket silently becomes warm-disk.
func TestTheWipePrefixEndsInASlash(t *testing.T) {
	w := &stubWiper{}

	_, err := Run(context.Background(), Config{
		Scenario: ColdBucket, Concurrency: 1, Duration: 20 * time.Millisecond, Corpus: NewCorpus(2, 1, ""),
	}, &stubStore{}, w)
	if err != nil && !strings.Contains(err.Error(), "measured nothing") {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(w.prefixes) == 0 {
		t.Fatal("cold-bucket did not wipe the disk")
	}

	for _, p := range w.prefixes {
		if !strings.HasSuffix(p, "/") {
			t.Errorf("wiped %q, which the admin API reads as one exact key, not a prefix", p)
		}
	}
}

type stubStore struct {
	mu     sync.Mutex
	getErr error
}

func (s *stubStore) Get(_ context.Context, _ string) (io.ReadCloser, tier.Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.getErr != nil {
		return nil, tier.Meta{}, s.getErr
	}

	return io.NopCloser(bytes.NewReader([]byte("x"))), tier.Meta{Size: 1}, nil
}

func (s *stubStore) Put(_ context.Context, _ string, r io.Reader, _ tier.Meta) error {
	_, _ = io.Copy(io.Discard, r)

	return nil
}

type stubWiper struct {
	mu       sync.Mutex
	prefixes []string
}

func (w *stubWiper) WipeDisk(_ context.Context, prefix string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prefixes = append(w.prefixes, prefix)

	return nil
}
