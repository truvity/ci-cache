// Package chain composes tiers into one.
//
// A chain is read in order and written through. What makes it worth a package
// rather than a loop is what happens on a hit at the back: the bytes are
// streamed to the caller AND written into every tier in front, so the next
// read is answered closer. That is the whole idea the cache rests on -- a
// runner's disk in front of a server's disk in front of a bucket -- and it is
// written once, here.
package chain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/truvity/ci-cache/engine/tier"
)

// faultThreshold is where a fault-in stops buffering in memory and spills to
// a temp file. Below it the object is held whole while it is copied to the
// caller and to the tiers in front; above it, memory would be decided by
// whatever is being built rather than by us.
const faultThreshold = 8 << 20

// Chain is a Tier composed of tiers, read front to back.
type Chain struct {
	tiers []tier.Tier

	// group collapses concurrent misses on one key into a single read of the
	// tier behind. Without it, sixty-four parallel compile actions that all
	// miss locally become sixty-four downloads of the same object.
	group singleflight.Group

	// onFault, when set, is told which tier answered. The metrics wrapper
	// uses it; the chain itself counts nothing, so that a test can use a
	// chain without a meter.
	onFault func(key string, from tier.Tier)

	// writeBehind, when set, receives puts for tiers after the first instead
	// of writing them inline. The bucket uses it: a runner should not wait
	// on an upload to have its object cached locally.
	writeBehind func(t tier.Tier, key string, m tier.Meta, body []byte) bool

	// onTierError, when set, is told about a tier that failed while the
	// chain carried on without it. The chain swallows such errors by design
	// -- see Stat -- and this is how they still reach a log and a counter
	// instead of vanishing.
	onTierError func(t tier.Tier, op string, err error)
}

// Option configures a Chain.
type Option func(*Chain)

// OnFault registers a callback told which tier answered a Get that the front
// tiers missed.
func OnFault(f func(key string, from tier.Tier)) Option {
	return func(c *Chain) { c.onFault = f }
}

// WriteBehind hands puts for the tiers behind the first to f instead of
// writing them inline. f reports whether it took the object; when it declines
// -- a full queue -- the put is dropped for that tier, which is a cache miss
// later and never an error now.
func WriteBehind(f func(t tier.Tier, key string, m tier.Meta, body []byte) bool) Option {
	return func(c *Chain) { c.writeBehind = f }
}

// OnTierError registers a callback for a tier that failed while the chain
// answered from another one. Without it those failures are invisible, which
// is how a bucket stays broken for a week behind a warm disk.
func OnTierError(f func(t tier.Tier, op string, err error)) Option {
	return func(c *Chain) { c.onTierError = f }
}

// New returns a chain over the tiers, read in the order given.
func New(tiers []tier.Tier, opts ...Option) *Chain {
	c := &Chain{tiers: tiers}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Tiers returns the chain's tiers, front to back.
func (c *Chain) Tiers() []tier.Tier { return c.tiers }

// Name implements tier.Tier.
func (c *Chain) Name() string { return "chain" }

// Get returns the object from the frontmost tier that has it, faulting it
// into every tier in front of that one on the way past.
func (c *Chain) Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error) {
	// The front tier is read directly rather than through the singleflight
	// group: a local hit is the common case and must not queue behind
	// anything.
	if len(c.tiers) > 0 {
		rc, m, err := c.tiers[0].Get(ctx, key)
		if err == nil {
			return rc, m, nil
		}
		if !errors.Is(err, tier.ErrNotFound) && c.onTierError != nil {
			// A broken front tier is not a reason to stop: the object may
			// still be behind it, and a cache that fails closed on a bad
			// disk is worse than one that is slow. It is a reason to say so.
			c.onTierError(c.tiers[0], "get", err)
		}
	}

	type result struct {
		path string // a temp file, when the object was large
		body []byte // the object, when it was small
		meta tier.Meta
	}

	v, err, _ := c.group.Do(key, func() (any, error) {
		for i := 1; i < len(c.tiers); i++ {
			rc, m, err := c.tiers[i].Get(ctx, key)
			if errors.Is(err, tier.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			r := &result{meta: m}
			if m.Size > 0 && m.Size <= faultThreshold {
				b, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					return nil, fmt.Errorf("chain: read %s from %s: %w", key, c.tiers[i].Name(), err)
				}
				r.body = b
			} else {
				// Unknown or large: spill to a temp file so that memory is
				// bounded by the number of concurrent faults, not by the
				// size of what is being cached.
				f, err := os.CreateTemp("", "ci-cache-fault-*")
				if err != nil {
					_ = rc.Close()
					return nil, err
				}
				n, err := io.Copy(f, rc)
				_ = rc.Close()
				if err != nil {
					_ = f.Close()
					_ = os.Remove(f.Name())
					return nil, fmt.Errorf("chain: read %s from %s: %w", key, c.tiers[i].Name(), err)
				}
				_ = f.Close()
				r.path = f.Name()
				r.meta.Size = n
			}
			if c.onFault != nil {
				c.onFault(key, c.tiers[i])
			}
			// Fault forward, nearest-last so that the tier a subsequent read
			// hits first is written last and a reader racing this never sees
			// a front tier that has the object while a middle one does not.
			for j := i - 1; j >= 0; j-- {
				var src io.ReadCloser
				if r.path != "" {
					f, err := os.Open(r.path)
					if err != nil {
						break
					}
					src = f
				} else {
					src = io.NopCloser(bytesReader(r.body))
				}
				err := c.tiers[j].Put(ctx, key, src, r.meta)
				_ = src.Close()
				if err != nil && !errors.Is(err, tier.ErrExists) && !errors.Is(err, tier.ErrNoSpace) {
					// Faulting forward is an optimisation. Failing it costs
					// the next reader a slower answer and nothing else.
					_ = err
				}
			}
			return r, nil
		}
		return nil, tier.ErrNotFound
	})
	if err != nil {
		return nil, tier.Meta{}, err
	}

	r := v.(*result)
	if r.path != "" {
		f, err := os.Open(r.path)
		if err != nil {
			return nil, tier.Meta{}, err
		}
		// The file is unlinked now and closed by the caller: the data stays
		// readable through the open descriptor, and nothing has to remember
		// to clean up after a caller that goes away.
		_ = os.Remove(r.path)
		return f, r.meta, nil
	}
	return io.NopCloser(bytesReader(r.body)), r.meta, nil
}

// Put writes the object to every tier.
//
// The FIRST tier decides the caller's outcome: it is the one the caller will
// read from next, and the one whose failure means the cache did not work.
// Tiers behind it are written best-effort, inline or through write-behind.
func (c *Chain) Put(ctx context.Context, key string, r io.Reader, m tier.Meta) error {
	if len(c.tiers) == 0 {
		return errors.New("chain: no tiers")
	}
	if len(c.tiers) == 1 {
		return c.tiers[0].Put(ctx, key, r, m)
	}

	// The body has to reach more than one tier, so it is buffered. Small
	// objects stay in memory; anything larger, or of unknown length, goes to
	// a temp file for the same reason a fault-in does.
	var (
		body []byte
		path string
	)
	if m.Size > 0 && m.Size <= faultThreshold {
		b, err := io.ReadAll(io.LimitReader(r, m.Size+1))
		if err != nil {
			return err
		}
		body = b
		m.Size = int64(len(b))
	} else {
		f, err := os.CreateTemp("", "ci-cache-put-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		n, err := io.Copy(f, r)
		if err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		path = f.Name()
		m.Size = n
	}

	open := func() (io.ReadCloser, error) {
		if path != "" {
			return os.Open(path)
		}
		return io.NopCloser(bytesReader(body)), nil
	}

	src, err := open()
	if err != nil {
		return err
	}
	err = c.tiers[0].Put(ctx, key, src, m)
	_ = src.Close()
	if err != nil && !errors.Is(err, tier.ErrExists) {
		return err
	}

	for i := 1; i < len(c.tiers); i++ {
		if c.writeBehind != nil {
			b := body
			if b == nil {
				// Write-behind needs the bytes after this call returns, and
				// the temp file is removed on return; read it once.
				f, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				b = f
			}
			c.writeBehind(c.tiers[i], key, m, b)
			continue
		}
		src, err := open()
		if err != nil {
			continue
		}
		err = c.tiers[i].Put(ctx, key, src, m)
		_ = src.Close()
		if err != nil && !errors.Is(err, tier.ErrExists) && !errors.Is(err, tier.ErrNoSpace) {
			_ = err
		}
	}
	return nil
}

// Stat answers from the frontmost tier that has the object, and reports a
// miss when none of them does -- EVEN IF A TIER FAILED while being asked.
//
// That is deliberate, and it was a real fault before it was a rule. A runner's
// agent probes the server with a Stat before it trusts it; the server answers
// through this chain; and while Stat returned the bucket's error, a bucket
// answering 403 made every agent on the estate conclude the server was
// unreachable and fall back to local-only -- although the server's warm disk
// would have served them all. One back-end's outage became everybody's cache
// outage, which is precisely the failure the tiers exist to prevent.
//
// "Is this key here" has a usable answer even when part of the chain cannot
// be asked: not as far as I can tell. A tier's health is reported by the
// readiness probe and by cicache.get{outcome=error}, which is where an
// operator looks for it -- not smuggled into the answer to a different
// question.
func (c *Chain) Stat(ctx context.Context, key string) (tier.Meta, error) {
	for _, t := range c.tiers {
		m, err := t.Stat(ctx, key)
		if err == nil {
			return m, nil
		}
		if !errors.Is(err, tier.ErrNotFound) && c.onTierError != nil {
			c.onTierError(t, "stat", err)
		}
	}
	return tier.Meta{}, tier.ErrNotFound
}

// Delete removes the object from every tier. Tier errors are joined, so a
// wipe reports everything that went wrong rather than the first thing.
func (c *Chain) Delete(ctx context.Context, key string) error {
	var errs []error
	for _, t := range c.tiers {
		if err := t.Delete(ctx, key); err != nil && !errors.Is(err, tier.ErrNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// bytesReader avoids importing bytes for one call in a file that otherwise
// deals in streams.
func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b   []byte
	off int
	mu  sync.Mutex
}

func (r *sliceReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}
