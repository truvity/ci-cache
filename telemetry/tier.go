package telemetry

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/truvity/ci-cache/engine/tier"
)

// WrapTier returns t with every call counted against frontend.
//
// This is how a chain gets instrumented without knowing that metrics exist.
// The alternative -- a counter inside each tier -- would mean the disk tier
// could not say which front-end asked, because by the time a key reaches it
// the front-end is only a prefix on a string.
//
// A nil recorder returns t unchanged, so a test can build a chain without a
// meter and a caller never has to branch.
func WrapTier(t tier.Tier, frontend string, r *Recorder) tier.Tier {
	if r == nil {
		return t
	}
	w := &wrappedTier{Tier: t, frontend: frontend, tierName: t.Name(), rec: r}

	// A tier's optional interfaces have to survive the wrapping. Losing
	// tier.Lister makes the admin API's List answer nothing at all -- the
	// disk is the only tier that has it -- and losing tier.Deleter turns a
	// bucket wipe back into one request per object, which is the difference
	// between a wipe that finishes and one somebody gives up on. Go has no
	// way to say "t plus these methods", so the combinations are spelled out.
	lister, canList := t.(tier.Lister)
	deleter, canDelete := t.(tier.Deleter)
	switch {
	case canList && canDelete:
		return &listerDeleterTier{wrappedTier: w, Lister: lister, Deleter: deleter}
	case canList:
		return &listerTier{wrappedTier: w, Lister: lister}
	case canDelete:
		return &deleterTier{wrappedTier: w, Deleter: deleter}
	default:
		return w
	}
}

type wrappedTier struct {
	tier.Tier
	frontend string
	tierName string
	rec      *Recorder
}

type listerTier struct {
	*wrappedTier
	tier.Lister
}

type deleterTier struct {
	*wrappedTier
	tier.Deleter
}

type listerDeleterTier struct {
	*wrappedTier
	tier.Lister
	tier.Deleter
}

// Get counts the read and the bytes it hands back.
func (w *wrappedTier) Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error) {
	start := time.Now()
	rc, m, err := w.Tier.Get(ctx, key)
	w.rec.Observe(w.frontend, w.tierName, OpGet, time.Since(start))

	switch {
	case err == nil:
		w.rec.Get(w.frontend, w.tierName, OutcomeHit)
		// The bytes are counted as the caller reads them, not from m.Size: a
		// Meta may say a size the tier never delivered, and a byte counter
		// that believes the metadata cannot show a truncated object.
		return &countingReadCloser{ReadCloser: rc, w: w}, m, nil
	case errors.Is(err, tier.ErrNotFound):
		// A miss is the ordinary case. Counting it as an error is the single
		// easiest way to make a healthy cold cache page somebody.
		w.rec.Get(w.frontend, w.tierName, OutcomeMiss)
	case errors.Is(err, tier.ErrNoSpace):
		w.rec.Get(w.frontend, w.tierName, OutcomeDropped)
	default:
		w.rec.Get(w.frontend, w.tierName, OutcomeError)
	}
	return rc, m, err
}

// Put counts the write and the bytes it consumed.
func (w *wrappedTier) Put(ctx context.Context, key string, r io.Reader, m tier.Meta) error {
	cr := &countingReader{r: r}
	start := time.Now()
	err := w.Tier.Put(ctx, key, cr, m)
	w.rec.Observe(w.frontend, w.tierName, OpPut, time.Since(start))

	// What the tier read is recorded whatever the outcome: a write that
	// failed halfway still cost that traffic, and a bucket bill is paid on
	// bytes sent rather than on objects stored.
	w.rec.Bytes(w.frontend, w.tierName, DirectionWritten, cr.n())

	switch {
	case err == nil:
		w.rec.Put(w.frontend, w.tierName, OutcomeMiss)
	case errors.Is(err, tier.ErrExists):
		w.rec.Put(w.frontend, w.tierName, OutcomeHit)
	case errors.Is(err, tier.ErrNoSpace):
		w.rec.Put(w.frontend, w.tierName, OutcomeDropped)
	default:
		w.rec.Put(w.frontend, w.tierName, OutcomeError)
	}
	return err
}

// Stat is timed but not counted as a read.
//
// A Stat moves no bytes and answers a different question; folding it into the
// get counter would inflate the denominator of every hit rate on the estate.
func (w *wrappedTier) Stat(ctx context.Context, key string) (tier.Meta, error) {
	start := time.Now()
	m, err := w.Tier.Stat(ctx, key)
	w.rec.Observe(w.frontend, w.tierName, OpStat, time.Since(start))
	return m, err
}

// Delete is timed but not counted: what a wipe removed is reported by the
// admin call that asked for it, where a human is reading the answer.
func (w *wrappedTier) Delete(ctx context.Context, key string) error {
	start := time.Now()
	err := w.Tier.Delete(ctx, key)
	w.rec.Observe(w.frontend, w.tierName, OpDelete, time.Since(start))
	return err
}

// Name is the wrapped tier's name. The wrapper is not a tier of its own and
// must never appear as one in a metric.
func (w *wrappedTier) Name() string { return w.tierName }

// countingReadCloser counts what a caller actually read out of a tier.
type countingReadCloser struct {
	io.ReadCloser
	w *wrappedTier

	mu     sync.Mutex
	read   int64
	closed bool
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.mu.Lock()
	c.read += int64(n)
	c.mu.Unlock()
	return n, err
}

// Close records the total once. A caller that closes twice -- a deferred
// Close beside an explicit one is the usual way -- must not double the bytes.
func (c *countingReadCloser) Close() error {
	c.mu.Lock()
	first := !c.closed
	c.closed = true
	n := c.read
	c.mu.Unlock()
	if first {
		c.w.rec.Bytes(c.w.frontend, c.w.tierName, DirectionRead, n)
	}
	return c.ReadCloser.Close()
}

// countingReader counts what a tier consumed from a Put body.
type countingReader struct {
	r io.Reader

	mu    sync.Mutex
	count int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	c.count += int64(n)
	c.mu.Unlock()
	return n, err
}

func (c *countingReader) n() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}
