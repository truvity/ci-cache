package breaker_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truvity/ci-cache/agent/breaker"
	"github.com/truvity/ci-cache/engine/tier"
)

// fakeTier fails whenever it is told to, and counts how often it was asked.
// The count is the whole point of the breaker test: what matters is not what
// the caller was told but whether the broken tier was called at all.
type fakeTier struct {
	mu      sync.Mutex
	failing bool
	calls   atomic.Int64
	body    string
}

func (f *fakeTier) fail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = v
}

func (f *fakeTier) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return errors.New("fake tier: unreachable")
	}
	return nil
}

func (f *fakeTier) Get(_ context.Context, _ string) (io.ReadCloser, tier.Meta, error) {
	f.calls.Add(1)
	if err := f.err(); err != nil {
		return nil, tier.Meta{}, err
	}
	return io.NopCloser(strings.NewReader(f.body)), tier.Meta{Size: int64(len(f.body))}, nil
}

func (f *fakeTier) Put(_ context.Context, _ string, r io.Reader, _ tier.Meta) error {
	f.calls.Add(1)
	_, _ = io.Copy(io.Discard, r)
	return f.err()
}

func (f *fakeTier) Stat(_ context.Context, _ string) (tier.Meta, error) {
	f.calls.Add(1)
	if err := f.err(); err != nil {
		return tier.Meta{}, err
	}
	return tier.Meta{Size: int64(len(f.body))}, nil
}

func (f *fakeTier) Delete(_ context.Context, _ string) error {
	f.calls.Add(1)
	return f.err()
}

func (f *fakeTier) Name() string { return "fake" }

func TestGetFailureIsAMiss(t *testing.T) {
	ctx := context.Background()
	f := &fakeTier{failing: true, body: "xyzzy"}
	b := breaker.New(f)

	_, _, err := b.Get(ctx, "k")
	if !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("Get on a failing tier = %v, want %v", err, tier.ErrNotFound)
	}
}

func TestPutFailureIsADrop(t *testing.T) {
	ctx := context.Background()
	f := &fakeTier{failing: true}
	b := breaker.New(f)

	if err := b.Put(ctx, "k", strings.NewReader("body"), tier.Meta{Size: 4}); err != nil {
		t.Fatalf("Put on a failing tier = %v, want nil: a drop is never an error", err)
	}
	if got := b.Drops(); got != 1 {
		t.Errorf("Drops = %d, want 1", got)
	}
}

func TestTripsAfterThresholdAndRecoversAfterWindow(t *testing.T) {
	ctx := context.Background()
	f := &fakeTier{failing: true, body: "xyzzy"}

	// A hand-wound clock: the window is a duration, and a test that sleeps
	// through a real one is a test that is either slow or flaky.
	var mu sync.Mutex
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}

	b := breaker.New(f, breaker.Threshold(5), breaker.Window(30*time.Second), breaker.Clock(clock))

	for i := range 5 {
		if _, _, err := b.Get(ctx, "k"); !errors.Is(err, tier.ErrNotFound) {
			t.Fatalf("Get %d = %v, want a miss", i, err)
		}
		if i < 4 && b.Open() {
			t.Fatalf("circuit opened after %d failures, want it to hold until 5", i+1)
		}
	}
	if !b.Open() {
		t.Fatal("circuit did not open after 5 consecutive failures")
	}
	if got, want := f.calls.Load(), int64(5); got != want {
		t.Fatalf("tier calls = %d, want %d", got, want)
	}
	if got := b.Trips(); got != 1 {
		t.Errorf("Trips = %d, want 1", got)
	}

	// While it is open the tier is not called at all: that is the cost the
	// breaker exists to stop paying.
	for range 10 {
		if _, _, err := b.Get(ctx, "k"); !errors.Is(err, tier.ErrNotFound) {
			t.Fatalf("Get while open = %v, want a miss", err)
		}
	}
	if got, want := f.calls.Load(), int64(5); got != want {
		t.Fatalf("tier calls while open = %d, want it to stay %d", got, want)
	}
	if got := b.Bypassed(); got != 10 {
		t.Errorf("Bypassed = %d, want 10", got)
	}

	// Just short of the window it is still open.
	advance(29 * time.Second)
	if !b.Open() {
		t.Fatal("circuit closed before the window elapsed")
	}

	// Past it, the tier is tried again, and a healthy answer is served.
	advance(2 * time.Second)
	f.fail(false)
	rc, m, err := b.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get after the window = %v, want a hit", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != "xyzzy" {
		t.Fatalf("Get after the window = %q, %v; want %q", got, err, "xyzzy")
	}
	if m.Size != 5 {
		t.Errorf("Meta.Size = %d, want 5", m.Size)
	}
	if b.Open() {
		t.Error("circuit still open after a successful call")
	}
	if got, want := f.calls.Load(), int64(6); got != want {
		t.Errorf("tier calls after recovery = %d, want %d", got, want)
	}
}

func TestMissDoesNotTrip(t *testing.T) {
	ctx := context.Background()
	f := &missTier{}
	b := breaker.New(f, breaker.Threshold(2))

	for range 10 {
		if _, _, err := b.Get(ctx, "k"); !errors.Is(err, tier.ErrNotFound) {
			t.Fatalf("Get = %v, want a miss", err)
		}
	}
	if b.Open() {
		t.Error("a run of misses opened the circuit: a tier that answers is not a tier that is broken")
	}
}

type missTier struct{}

func (missTier) Get(context.Context, string) (io.ReadCloser, tier.Meta, error) {
	return nil, tier.Meta{}, tier.ErrNotFound
}
func (missTier) Put(context.Context, string, io.Reader, tier.Meta) error { return nil }
func (missTier) Stat(context.Context, string) (tier.Meta, error) {
	return tier.Meta{}, tier.ErrNotFound
}
func (missTier) Delete(context.Context, string) error { return nil }
func (missTier) Name() string                         { return "miss" }
