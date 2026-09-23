package bucket

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
)

// putTimeout bounds one upload.
//
// Without it a store that accepts a connection and then says nothing holds a
// worker forever, and eight such objects are the whole upload capacity of the
// process. The value is generous because the alternative -- timing out a slow
// but working upload -- costs a cache entry, while hanging costs the cache.
const putTimeout = 5 * time.Minute

// Queue is the write-behind path to the bucket.
//
// It exists because of who waits. A runner that has just compiled something
// wants its object cached locally and wants to get on with the build; whether
// the object has reached the bucket yet matters to the NEXT job, an hour from
// now, and to nobody in this one. So the disk tier's Put is on the request
// path and the bucket's is not.
//
// Everything here follows from that. The queue is bounded, because a store
// that is slow or down must cost memory that is decided in advance rather
// than by how fast CI is going; and a full queue is not an error, because the
// only thing a caller could do about it is fail a build over a cache write.
type Queue struct {
	b   *Bucket
	cfg config.Upload
	log *slog.Logger

	items chan item

	// mu guards the close of items against a concurrent Offer. A send on a
	// closed channel panics, and the whole point of this queue is that it is
	// written to by every request goroutine in the process.
	mu     sync.RWMutex
	closed bool

	queued  atomic.Int64 // bytes accepted and not yet flushed
	depth   atomic.Int64 // objects accepted and not yet flushed
	dropped atomic.Int64

	wg   sync.WaitGroup
	halt chan struct{}
	once sync.Once
}

// item is one pending upload. The body is held whole: it has already been
// buffered by whoever called Put, and the queue's byte bound is what keeps
// that from being unbounded.
type item struct {
	key  string
	meta tier.Meta
	body []byte
}

// NewQueue returns a write-behind queue in front of b.
func NewQueue(b *Bucket, cfg config.Upload, opts ...Option) *Queue {
	var o options
	for _, f := range opts {
		f(&o)
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 1
	}
	q := &Queue{
		b:     b,
		cfg:   cfg,
		log:   o.log,
		items: make(chan item, cfg.Queue),
		halt:  make(chan struct{}),
	}
	if q.log == nil {
		q.log = b.log
	}
	return q
}

// Start launches the workers. Cancelling ctx abandons whatever is queued;
// Stop is how a shutdown that wants the objects ends this.
func (q *Queue) Start(ctx context.Context) {
	for range q.cfg.Concurrency {
		q.wg.Add(1)
		go q.worker()
	}
	go func() {
		<-ctx.Done()
		close(q.halt)
	}()
}

// Offer hands the queue an object, reporting whether it took it.
//
// False is a drop, and a drop is a cache miss for some later job and nothing
// worse. The caller counts it and carries on: failing a build because a cache
// upload could not be queued would turn a degraded cache into an outage,
// which is the failure mode this whole service is supposed to remove.
func (q *Queue) Offer(key string, m tier.Meta, body []byte) bool {
	size := int64(len(body))

	// Below MinSize the bucket is skipped, and that is not a drop: it is the
	// configured answer for an object whose round trip costs more than
	// deriving it again. Reporting it as a drop would make a healthy cache
	// look like a failing one on a dashboard.
	if size < q.cfg.MinSize {
		return true
	}

	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		q.dropped.Add(1)
		return false
	}

	// The byte bound is claimed before the slot, and given back if the slot
	// is not there, so that the accounting never reports more in flight than
	// there is. Both bounds are needed: the entry count keeps a flood of
	// small objects from becoming a long tail of work, and the byte count
	// keeps a handful of large ones from becoming the pod's memory limit.
	if !q.claim(size) {
		q.dropped.Add(1)
		return false
	}
	select {
	case q.items <- item{key: key, meta: m, body: body}:
		q.depth.Add(1)
		return true
	default:
		q.queued.Add(-size)
		q.dropped.Add(1)
		return false
	}
}

// claim reserves size bytes if that keeps the queue inside its byte bound.
func (q *Queue) claim(size int64) bool {
	if q.cfg.QueueBytes <= 0 {
		q.queued.Add(size)
		return true
	}
	for {
		have := q.queued.Load()
		if have+size > q.cfg.QueueBytes {
			return false
		}
		if q.queued.CompareAndSwap(have, have+size) {
			return true
		}
	}
}

// Depth is how many objects are accepted and not yet in the bucket, in
// flight included. A depth that does not come back down is a store that
// cannot keep up with CI, which is the number worth alerting on.
func (q *Queue) Depth() int { return int(q.depth.Load()) }

// Dropped is how many objects the queue refused since start. It only grows.
func (q *Queue) Dropped() int64 { return q.dropped.Load() }

// Stop stops taking objects and waits for what is queued, up to ctx's
// deadline.
//
// It reports what it could not flush rather than swallowing it. A pod that is
// being drained has a grace period, and knowing that a rollout dropped four
// hundred objects on the floor every time is the difference between a cache
// that seems to have a poor hit rate and one that is being restarted too
// aggressively.
//
// It can return while one upload is still in flight: an upload is bounded by
// its own timeout rather than by this deadline, so a store that has accepted
// a connection and then gone quiet cannot hold a shutdown open. Such an
// upload dies with the process, and is counted here as unflushed.
func (q *Queue) Stop(ctx context.Context) error {
	q.once.Do(func() {
		q.mu.Lock()
		q.closed = true
		close(q.items)
		q.mu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}

	// The count is read after the wait either way. Workers also stop when
	// Start's context is cancelled, so a drain can end with objects still
	// queued and no deadline having passed, and staying quiet about that
	// would be the exact silence this method exists to break.
	n, queued := q.depth.Load(), q.queued.Load()
	if n == 0 {
		return nil
	}
	cause := ctx.Err()
	if cause == nil {
		cause = errors.New("workers had already stopped")
	}
	return fmt.Errorf("bucket: upload queue stopped with %d objects (%d bytes) unflushed: %w", n, queued, cause)
}

// worker drains the channel until it is closed, or until Start's context is
// cancelled, whichever comes first.
func (q *Queue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.halt:
			return
		case it, ok := <-q.items:
			if !ok {
				return
			}
			q.flush(it)
		}
	}
}

// flush uploads one object.
//
// The context is built here rather than inherited from Start, so that a
// shutdown draining the queue is not racing a cancelled context: Stop's
// deadline decides how long the drain gets, and this decides how long any one
// upload gets.
func (q *Queue) flush(it item) {
	defer func() {
		q.depth.Add(-1)
		q.queued.Add(-int64(len(it.body)))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), putTimeout)
	defer cancel()

	m := it.meta
	m.Size = int64(len(it.body))
	err := q.b.Put(ctx, it.key, bytes.NewReader(it.body), m)
	switch {
	case err == nil, errors.Is(err, tier.ErrExists):
		// Already there is the ordinary end of a race between two runners
		// that built the same thing, and it is a success for both.
	default:
		q.log.Warn("upload failed", "key", it.key, "bytes", len(it.body), "err", err)
	}
}

// WriteBehind is the callback chain.WriteBehind wants, so that a server wires
// the two together without either package knowing about the other.
func (q *Queue) WriteBehind() func(t tier.Tier, key string, m tier.Meta, body []byte) bool {
	return func(t tier.Tier, key string, m tier.Meta, body []byte) bool {
		if bt, ok := t.(*Bucket); !ok || bt != q.b {
			// Some other tier behind the disk -- a remote peer, say -- is not
			// this queue's business, and writing it here would send it to the
			// bucket instead. Decline, and the chain writes it inline.
			return false
		}
		return q.Offer(key, m, body)
	}
}
