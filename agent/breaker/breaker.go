// Package breaker keeps a remote tier's bad day out of the build.
//
// A GOCACHEPROG program is not an optional accelerator. When it answers a
// request with an error the Go toolchain fails the action, so a cache that
// passes a network fault back to its caller has broken the compiler rather
// than slowed it down. Everything here exists to turn a tier's failure into a
// miss -- and, once a tier has failed enough times in a row, to stop asking
// for a while, because a tier that is timing out charges every single action
// its full timeout before the build can carry on without it.
package breaker

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truvity/ci-cache/engine/tier"
)

// The defaults. Five in a row is past the point where a retry is plausibly a
// blip, and thirty seconds is short enough that a remote which comes back
// mid-build is used again for the rest of it.
const (
	DefaultThreshold = 5
	DefaultWindow    = 30 * time.Second
)

// Breaker wraps a tier so that its failures cost a miss instead of a build.
//
// It is a tier.Tier, so it goes wherever a tier goes -- in practice behind
// the local disk in a chain, which is the only place a failure is survivable
// at all.
type Breaker struct {
	t         tier.Tier
	threshold int
	window    time.Duration
	now       func() time.Time

	// mu guards the failure bookkeeping only. The wrapped tier is called
	// outside it: holding a lock across a network request would serialise
	// every compile action in the build behind the slowest one.
	mu          sync.Mutex
	consecutive int
	openUntil   time.Time

	drops    atomic.Int64
	bypassed atomic.Int64
	trips    atomic.Int64
}

// Option configures a Breaker.
type Option func(*Breaker)

// Threshold sets how many consecutive failures open the circuit.
func Threshold(n int) Option {
	return func(b *Breaker) {
		if n > 0 {
			b.threshold = n
		}
	}
}

// Window sets how long the circuit stays open once it has tripped.
func Window(d time.Duration) Option {
	return func(b *Breaker) {
		if d > 0 {
			b.window = d
		}
	}
}

// Clock replaces time.Now, so that a test can cross the window without
// waiting for it.
func Clock(f func() time.Time) Option {
	return func(b *Breaker) {
		if f != nil {
			b.now = f
		}
	}
}

// New wraps t.
func New(t tier.Tier, opts ...Option) *Breaker {
	b := &Breaker{t: t, threshold: DefaultThreshold, window: DefaultWindow, now: time.Now}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Name reports the wrapped tier's name: a breaker is not a place a key lives,
// so it must not show up as one in metrics or logs.
func (b *Breaker) Name() string { return b.t.Name() }

// Open reports whether the circuit is currently bypassing the tier.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now().Before(b.openUntil)
}

// Drops is the number of writes the tier never saw, whether because it failed
// or because the circuit was open. Each one is a slower read later and
// nothing worse, which is exactly why it is counted rather than returned.
func (b *Breaker) Drops() int64 { return b.drops.Load() }

// Bypassed is the number of requests the open circuit answered by itself.
func (b *Breaker) Bypassed() int64 { return b.bypassed.Load() }

// Trips is the number of times the circuit has opened.
func (b *Breaker) Trips() int64 { return b.trips.Load() }

// Get returns the object, or a miss. A failure of the wrapped tier is
// reported as tier.ErrNotFound, because a chain treats a miss as "ask the
// next tier" and an error as "give up", and giving up is what fails the
// build.
func (b *Breaker) Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error) {
	if !b.allow() {
		b.bypassed.Add(1)
		return nil, tier.Meta{}, tier.ErrNotFound
	}
	rc, m, err := b.t.Get(ctx, key)
	b.record(err)
	if err != nil && !errors.Is(err, tier.ErrNotFound) {
		// A tier that returns both a reader and an error is misbehaving, but
		// leaking its descriptor would be our bug, not its.
		if rc != nil {
			_ = rc.Close()
		}
		return nil, tier.Meta{}, tier.ErrNotFound
	}
	return rc, m, err
}

// Put writes the object and never reports a failure.
//
// The body has already been handed to whatever tier the caller reads from
// next; what is left is an optimisation for somebody else's build, and it is
// not worth failing this one for.
func (b *Breaker) Put(ctx context.Context, key string, r io.Reader, m tier.Meta) error {
	if !b.allow() {
		b.bypassed.Add(1)
		b.drops.Add(1)
		// The body is drained so that a caller streaming from a file or a
		// pipe is not left blocked on a reader nobody will finish.
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	err := b.t.Put(ctx, key, r, m)
	b.record(err)
	if err != nil && !errors.Is(err, tier.ErrExists) {
		b.drops.Add(1)
	}
	return nil
}

// Stat reports the object's metadata, or a miss, on the same terms as Get.
func (b *Breaker) Stat(ctx context.Context, key string) (tier.Meta, error) {
	if !b.allow() {
		b.bypassed.Add(1)
		return tier.Meta{}, tier.ErrNotFound
	}
	m, err := b.t.Stat(ctx, key)
	b.record(err)
	if err != nil && !errors.Is(err, tier.ErrNotFound) {
		return tier.Meta{}, tier.ErrNotFound
	}
	return m, err
}

// Delete removes the object, and swallows a failure to do so.
//
// A delete that a broken tier never received is the one case here that can
// leave a wrong answer behind, so it is counted as a drop and the caller is
// expected to read that count before believing a wipe.
func (b *Breaker) Delete(ctx context.Context, key string) error {
	if !b.allow() {
		b.bypassed.Add(1)
		b.drops.Add(1)
		return nil
	}
	err := b.t.Delete(ctx, key)
	b.record(err)
	if err != nil && !errors.Is(err, tier.ErrNotFound) {
		b.drops.Add(1)
	}
	return nil
}

// allow reports whether the wrapped tier should be called. Crossing the end
// of an open window also clears the failure count, so a tier that comes back
// and then goes away again gets a fresh threshold rather than tripping on its
// first stumble.
func (b *Breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if b.now().Before(b.openUntil) {
		return false
	}
	b.openUntil = time.Time{}
	b.consecutive = 0
	return true
}

// record folds one call's outcome into the failure count. A miss is not a
// failure: the tier answered, and the answer was that it does not have the
// object.
func (b *Breaker) record(err error) {
	healthy := err == nil || errors.Is(err, tier.ErrNotFound) || errors.Is(err, tier.ErrExists)

	b.mu.Lock()
	defer b.mu.Unlock()
	if healthy {
		b.consecutive = 0
		return
	}
	b.consecutive++
	if b.consecutive >= b.threshold {
		b.openUntil = b.now().Add(b.window)
		b.consecutive = 0
		b.trips.Add(1)
	}
}
