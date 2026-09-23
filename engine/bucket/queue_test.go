package bucket

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
)

// waitFor polls until cond holds or the budget runs out. Every wait in these
// tests is bounded and asks a falsifiable question: a sleep long enough to be
// reliable is a test that takes that long even when it passes, and one short
// enough to be quick is a test that fails on a loaded machine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueueUploads(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{KeyPrefix: "p"}, f)
	q := NewQueue(b, config.Upload{Concurrency: 4, Queue: 16, QueueBytes: 1 << 20})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	body := []byte("an object worth keeping")
	if !q.Offer("go/build/v1/abc", tier.Meta{Size: int64(len(body))}, body) {
		t.Fatal("Offer declined an object with an empty queue")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := q.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := q.Depth(); got != 0 {
		t.Errorf("Depth after a clean stop = %d, want 0", got)
	}
	if len(f.putInputs()) != 1 {
		t.Fatalf("PutObject calls = %d, want 1", len(f.putInputs()))
	}
	if got := f.stored["p/go/build/v1/abc"]; string(got.body) != string(body) {
		t.Errorf("stored %q, want %q", got.body, body)
	}
}

// TestQueueSkipsTinyObjects: below MinSize the bucket is skipped, and that is
// policy rather than a drop. Counting it as a drop would make a healthy cache
// look like a failing one on a dashboard.
func TestQueueSkipsTinyObjects(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)
	q := NewQueue(b, config.Upload{Concurrency: 1, Queue: 8, QueueBytes: 1 << 20, MinSize: 1024})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	if !q.Offer("tiny", tier.Meta{Size: 8}, []byte("12345678")) {
		t.Fatal("Offer of a tiny object reported a drop")
	}
	if got := q.Dropped(); got != 0 {
		t.Errorf("Dropped = %d after a skipped object, want 0", got)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := q.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := len(f.putInputs()); n != 0 {
		t.Errorf("PutObject calls = %d, want 0: the object was below MinSize", n)
	}
}

// TestQueueIsBoundedWhenTheStoreStalls is what the whole queue is for.
//
// A store that has stopped answering must cost a fixed amount of memory and
// a growing drop count, never the pod's memory limit, and an offer that does
// not fit is never an error -- the caller counts it and gets on with the
// build.
func TestQueueIsBoundedWhenTheStoreStalls(t *testing.T) {
	f := newFake()
	f.stall = make(chan struct{})
	b := newTestBucket(t, config.Store{}, f)

	const (
		objectSize = 4096
		queueBytes = 256 * objectSize
	)
	cfg := config.Upload{Concurrency: 4, Queue: 512, QueueBytes: queueBytes}
	q := NewQueue(b, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	body := make([]byte, objectSize)
	accepted := 0
	for i := range 2000 {
		if q.Offer(fmt.Sprintf("k/%d", i), tier.Meta{Size: objectSize}, body) {
			accepted++
		}
		if got := q.queued.Load(); got > queueBytes {
			t.Fatalf("queued %d bytes, over the %d byte bound", got, queueBytes)
		}
	}

	if q.Dropped() == 0 {
		t.Fatalf("Dropped = 0 after 2000 offers into a stalled store; accepted %d", accepted)
	}
	if got := int64(accepted) + q.Dropped(); got != 2000 {
		t.Errorf("accepted + dropped = %d, want 2000: an offer is either taken or counted", got)
	}
	if got := q.Depth(); got > cfg.Queue+cfg.Concurrency {
		t.Errorf("Depth = %d, over the %d slots and %d workers", got, cfg.Queue, cfg.Concurrency)
	}

	// Stop must not hang on a store that never answers, and must say what it
	// left behind. A rollout that silently drops four hundred objects every
	// time looks like a cache with a poor hit rate.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stopCancel()
	err := q.Stop(stopCtx)
	if err == nil {
		t.Fatal("Stop = nil while the store was stalled, want the remainder reported")
	}
	if !strings.Contains(err.Error(), "unflushed") {
		t.Errorf("Stop error %q does not say what was left", err)
	}

	close(f.stall)
	cancel()
}

// TestQueueStopDrains: a graceful shutdown inside its deadline gets the
// objects to the bucket, which is the difference between a rollout and a
// rollout that empties the cache.
func TestQueueStopDrains(t *testing.T) {
	f := newFake()
	f.stall = make(chan struct{})
	b := newTestBucket(t, config.Store{}, f)
	q := NewQueue(b, config.Upload{Concurrency: 2, Queue: 64, QueueBytes: 1 << 20})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	body := []byte("pending")
	for i := range 16 {
		if !q.Offer(fmt.Sprintf("k/%d", i), tier.Meta{Size: int64(len(body))}, body) {
			t.Fatalf("Offer %d declined with a queue of 64", i)
		}
	}

	// Let go of the store only once everything is queued, so that the drain
	// is what empties the queue rather than a race with the workers.
	waitFor(t, "the queue to fill", func() bool { return q.Depth() == 16 })
	close(f.stall)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := q.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := len(f.putInputs()); n != 16 {
		t.Errorf("PutObject calls = %d, want 16", n)
	}
	if got := q.queued.Load(); got != 0 {
		t.Errorf("queued = %d bytes after a clean drain, want 0", got)
	}
}

// TestQueueRefusesAfterStop: once a shutdown has begun, an offer has nowhere
// to go. It has to report that rather than panic on a closed channel, because
// every request goroutine in the process may be in Offer when Stop is called.
func TestQueueRefusesAfterStop(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)
	q := NewQueue(b, config.Upload{Concurrency: 1, Queue: 4, QueueBytes: 1 << 20})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := q.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if q.Offer("late", tier.Meta{Size: 1}, []byte("x")) {
		t.Error("Offer was accepted after Stop")
	}
	if q.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1", q.Dropped())
	}
}

// TestQueueOffersAreConcurrent is here for the race detector rather than for
// the assertion: Offer is called from every request goroutine at once, and
// the bounds it keeps are the ones a data race would corrupt silently.
func TestQueueOffersAreConcurrent(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)
	q := NewQueue(b, config.Upload{Concurrency: 4, Queue: 32, QueueBytes: 64 << 10})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	done := make(chan struct{})
	for w := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			body := make([]byte, 512)
			for i := range 100 {
				q.Offer(fmt.Sprintf("k/%d/%d", w, i), tier.Meta{Size: 512}, body)
				_, _ = q.Depth(), q.Dropped()
			}
		}()
	}
	for range 8 {
		<-done
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := q.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := q.queued.Load(); got != 0 {
		t.Errorf("queued = %d bytes after the drain, want 0", got)
	}
}

// TestWriteBehindDeclinesOtherTiers: the callback the chain holds is offered
// every tier behind the disk. Taking one that is not this bucket would send
// somebody else's object here instead of letting the chain write it inline.
func TestWriteBehindDeclinesOtherTiers(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)
	q := NewQueue(b, config.Upload{Concurrency: 1, Queue: 4, QueueBytes: 1 << 20})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = q.Stop(stopCtx)
	}()

	wb := q.WriteBehind()
	other := newTestBucket(t, config.Store{Bucket: "elsewhere"}, newFake())
	if wb(other, "k", tier.Meta{Size: 1}, []byte("x")) {
		t.Error("write-behind took an object for another tier")
	}
	if !wb(b, "k", tier.Meta{Size: 1}, []byte("x")) {
		t.Error("write-behind declined an object for its own bucket")
	}
}

// TestQueueStopWithoutWorkersReportsTheRemainder covers the drain that ends
// with nobody left to do it -- Start's context cancelled first, or never
// called. Nothing is waiting on a deadline, so a Stop that only looked at its
// own context would return nil and say the queue emptied.
func TestQueueStopWithoutWorkersReportsTheRemainder(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)
	q := NewQueue(b, config.Upload{Concurrency: 2, Queue: 8, QueueBytes: 1 << 20})

	body := []byte("queued and abandoned")
	for i := range 3 {
		if !q.Offer(fmt.Sprintf("k/%d", i), tier.Meta{Size: int64(len(body))}, body) {
			t.Fatalf("Offer %d declined", i)
		}
	}

	err := q.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop = nil with three objects still queued")
	}
	if !strings.Contains(err.Error(), "3 objects") {
		t.Errorf("Stop error %q does not say how much was left", err)
	}
	if n := len(f.putInputs()); n != 0 {
		t.Errorf("PutObject calls = %d, want 0: no worker ever ran", n)
	}
}
