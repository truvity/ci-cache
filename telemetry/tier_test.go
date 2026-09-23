package telemetry

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/truvity/ci-cache/engine/tier"
)

// fakeTier answers whatever the test tells it to.
type fakeTier struct {
	body string
	err  error
	put  error
}

func (f *fakeTier) Get(_ context.Context, _ string) (io.ReadCloser, tier.Meta, error) {
	if f.err != nil {
		return nil, tier.Meta{}, f.err
	}
	return io.NopCloser(strings.NewReader(f.body)), tier.Meta{Size: int64(len(f.body))}, nil
}

func (f *fakeTier) Put(_ context.Context, _ string, r io.Reader, _ tier.Meta) error {
	// Read the body even when the put will fail: a real tier streams before
	// it discovers it cannot store, and the byte counter has to match that.
	_, _ = io.Copy(io.Discard, r)
	return f.put
}

func (f *fakeTier) Stat(_ context.Context, _ string) (tier.Meta, error) {
	return tier.Meta{Size: int64(len(f.body))}, f.err
}
func (f *fakeTier) Delete(_ context.Context, _ string) error { return nil }
func (f *fakeTier) Name() string                             { return "disk" }

// listDeleteTier is a tier that also implements both optional interfaces.
type listDeleteTier struct{ fakeTier }

func (listDeleteTier) List(_ context.Context, _, _ string, _ int) ([]tier.Entry, string, error) {
	return []tier.Entry{{Key: "go/build/aa"}}, "", nil
}

func (listDeleteTier) DeletePrefix(_ context.Context, _ string) (int64, int64, error) {
	return 3, 30, nil
}

// TestWrapTierCountsOutcomes is the distinction the whole dashboard rests on:
// a miss is not an error.
//
// tier.ErrNotFound is the ordinary answer of a cold cache. Counting it as an
// error means every new installation, and every new key, looks like a cache
// that is failing, and the alert built on the error rate has to be silenced
// on day one and is never heard again.
func TestWrapTierCountsOutcomes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		err     error
		outcome string
	}{
		{"hit", nil, OutcomeHit},
		{"not found is a miss", tier.ErrNotFound, OutcomeMiss},
		// This one only LOOKS like a not-found. Matching on the message
		// rather than with errors.Is is the mistake being asserted against:
		// it would turn a real I/O failure into a miss and hide a dying disk.
		{"a lookalike message is not a miss", errors.New("disk: tier: not found"), OutcomeError},
		{"no space is dropped", tier.ErrNoSpace, OutcomeDropped},
		{"anything else is an error", errors.New("i/o error"), OutcomeError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec := NewRecorder()
			w := WrapTier(&fakeTier{body: "hello", err: c.err}, "go/build", rec)

			rc, _, err := w.Get(context.Background(), "k")
			if err == nil {
				if _, cerr := io.Copy(io.Discard, rc); cerr != nil {
					t.Fatalf("read: %v", cerr)
				}
				_ = rc.Close()
			}

			ti := rec.Snapshot().Frontends[0].Tiers[0]
			got := map[string]int64{
				OutcomeHit:     ti.Hits,
				OutcomeMiss:    ti.Misses,
				OutcomeError:   ti.Errors,
				OutcomeDropped: ti.Dropped,
			}
			if got[c.outcome] != 1 {
				t.Errorf("outcome %s = %d, want 1 (counters: %+v)", c.outcome, got[c.outcome], ti)
			}
			if ti.Gets != 1 {
				t.Errorf("gets = %d, want 1", ti.Gets)
			}
		})
	}
}

// TestWrapTierCountsBytesActuallyMoved checks that the byte counters follow
// the stream rather than the metadata.
func TestWrapTierCountsBytesActuallyMoved(t *testing.T) {
	t.Parallel()

	rec := NewRecorder()
	w := WrapTier(&fakeTier{body: "0123456789"}, "go/build", rec)

	rc, _, err := w.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Closed twice on purpose: a deferred Close beside an explicit one is the
	// usual shape, and it must not count the bytes twice.
	_ = rc.Close()
	_ = rc.Close()

	if err := w.Put(context.Background(), "k", strings.NewReader("abc"), tier.Meta{Size: 3}); err != nil {
		t.Fatalf("put: %v", err)
	}

	ti := rec.Snapshot().Frontends[0].Tiers[0]
	if ti.BytesRead != 10 {
		t.Errorf("bytesRead = %d, want 10", ti.BytesRead)
	}
	if ti.BytesWritten != 3 {
		t.Errorf("bytesWritten = %d, want 3", ti.BytesWritten)
	}
}

// TestWrapTierPutOutcomes checks the put vocabulary: a write that found the
// object already there is a hit, and one that stored is a miss.
func TestWrapTierPutOutcomes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		err     error
		outcome string
	}{
		{"stored", nil, OutcomeMiss},
		{"already there", tier.ErrExists, OutcomeHit},
		{"no room", tier.ErrNoSpace, OutcomeDropped},
		{"broken", errors.New("boom"), OutcomeError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec := NewRecorder()
			w := WrapTier(&fakeTier{put: c.err}, "nix", rec)
			_ = w.Put(context.Background(), "k", strings.NewReader("x"), tier.Meta{Size: 1})

			ti := rec.Snapshot().Frontends[0].Tiers[0]
			if ti.Puts != 1 {
				t.Errorf("puts = %d, want 1", ti.Puts)
			}
			// Read through PutByOutcome rather than through Hits and
			// Misses: those two are READ counters by definition, because a
			// hit rate that counted writes would not be a hit rate.
			if n := ti.PutByOutcome[c.outcome]; n != 1 {
				t.Errorf("put outcome %s = %d, want 1 (counters: %+v)", c.outcome, n, ti)
			}
		})
	}
}

// TestWrapTierPreservesOptionalInterfaces is the trap this wrapper exists to
// fall into exactly once.
//
// A decorator that returns a plain tier.Tier silently hides tier.Lister and
// tier.Deleter. Nothing fails to compile; the admin API's List just starts
// answering nothing, and a bucket wipe quietly becomes one request per
// object. Both are found in production, by an operator, during an incident.
func TestWrapTierPreservesOptionalInterfaces(t *testing.T) {
	t.Parallel()

	rec := NewRecorder()

	plain := WrapTier(&fakeTier{}, "go/build", rec)
	if _, ok := plain.(tier.Lister); ok {
		t.Error("wrapping a plain tier invented a Lister")
	}
	if _, ok := plain.(tier.Deleter); ok {
		t.Error("wrapping a plain tier invented a Deleter")
	}

	full := WrapTier(&listDeleteTier{}, "go/build", rec)
	lister, ok := full.(tier.Lister)
	if !ok {
		t.Fatal("wrapping lost tier.Lister: the admin API's List would answer nothing")
	}
	entries, _, err := lister.List(context.Background(), "go/", "", 10)
	if err != nil || len(entries) != 1 {
		t.Errorf("List = %v, %v; want one entry", entries, err)
	}

	deleter, ok := full.(tier.Deleter)
	if !ok {
		t.Fatal("wrapping lost tier.Deleter: a prefix wipe would walk key by key")
	}
	n, b, err := deleter.DeletePrefix(context.Background(), "go/")
	if err != nil || n != 3 || b != 30 {
		t.Errorf("DeletePrefix = %d, %d, %v; want 3, 30, nil", n, b, err)
	}

	if full.Name() != "disk" {
		t.Errorf("Name = %q, want the wrapped tier's name", full.Name())
	}
}

// TestWrapTierWithoutARecorderIsTheTierItself keeps the no-meter path free.
func TestWrapTierWithoutARecorderIsTheTierItself(t *testing.T) {
	t.Parallel()

	inner := &fakeTier{}
	if got := WrapTier(inner, "go/build", nil); got != tier.Tier(inner) {
		t.Errorf("WrapTier with a nil recorder returned %T, want the tier unchanged", got)
	}
}

// TestWrapTierTimesEveryMethod checks that the latency instrument covers all
// four operations, not only the two that move bytes.
func TestWrapTierTimesEveryMethod(t *testing.T) {
	t.Parallel()

	rec := NewRecorder()
	w := WrapTier(&fakeTier{body: "x"}, "maven", rec)
	ctx := context.Background()

	rc, _, err := w.Get(ctx, "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = rc.Close()
	_ = w.Put(ctx, "k", strings.NewReader("x"), tier.Meta{Size: 1})
	_, _ = w.Stat(ctx, "k")
	_ = w.Delete(ctx, "k")

	var ops []string
	for _, o := range rec.Snapshot().Frontends[0].Ops {
		if o.Count != 1 {
			t.Errorf("op %s counted %d times, want 1", o.Op, o.Count)
		}
		ops = append(ops, o.Op)
	}
	if want := []string{OpDelete, OpGet, OpPut, OpStat}; !equal(ops, want) {
		t.Errorf("ops = %v, want %v", ops, want)
	}
}
