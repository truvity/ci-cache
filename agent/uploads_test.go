package agent

import (
	"sync/atomic"
	"testing"
	"time"
)

// The pool exists to bound concurrency, so the bound is the thing to test.
// Unbounded, a build that outruns the network accumulates one in-flight
// request per compiled object and dies on descriptors somewhere nobody can
// diagnose.
func TestSubmitBlocksOnlyWhenEveryWorkerIsBusy(t *testing.T) {
	u := newUploads(2)

	release := make(chan struct{})

	var running atomic.Int64

	var peak atomic.Int64

	for range 2 {
		u.submit(func() {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			running.Add(-1)
		})
	}

	// Both workers are now occupied. A third submit must not return until
	// one of them is free, which is what makes the pool a bound rather than
	// a suggestion.
	third := make(chan struct{})

	go func() {
		u.submit(func() {})
		close(third)
	}()

	select {
	case <-third:
		t.Fatal("a third submit returned while both workers were busy: the pool is not bounding anything")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-third:
	case <-time.After(5 * time.Second):
		t.Fatal("the third submit never ran after a worker freed up")
	}

	if finished, _, left := u.drain(5 * time.Second); !finished {
		t.Fatalf("drain left %d outstanding", left)
	}

	if got := peak.Load(); got > 2 {
		t.Errorf("%d uploads ran at once with a limit of 2", got)
	}
}

// A zero limit must pick a real default rather than a channel of capacity
// zero, which would make every submit block until a worker happened to be
// receiving -- turning the whole change back into the blocking path it
// replaces.
func TestZeroWorkersPicksAUsableDefault(t *testing.T) {
	u := newUploads(0)

	if got := cap(u.limit); got < minUploadWorkers {
		t.Fatalf("default pool is %d workers, want at least %d", got, minUploadWorkers)
	}

	done := make(chan struct{})
	go func() {
		u.submit(func() {})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("submit blocked on a freshly built pool")
	}

	if finished, _, _ := u.drain(5 * time.Second); !finished {
		t.Error("drain did not finish")
	}
}

// The drain is what pays back the time the puts did not spend. If it
// returned early, objects would be missing from the chain and the next build
// would be slower for a reason nothing recorded.
func TestDrainWaitsForOutstandingWork(t *testing.T) {
	u := newUploads(4)

	var done atomic.Int64

	for range 4 {
		u.submit(func() {
			time.Sleep(50 * time.Millisecond)
			done.Add(1)
		})
	}

	finished, _, left := u.drain(10 * time.Second)
	if !finished {
		t.Fatalf("drain timed out with %d left", left)
	}

	if got := done.Load(); got != 4 {
		t.Errorf("%d of 4 uploads finished before drain returned", got)
	}
}

// A drain that cannot finish must say how much it abandoned rather than
// hang. A job that appears to finish and then sits there is a job somebody
// cancels, and a cancelled job tells us nothing.
func TestDrainReportsWhatItAbandoned(t *testing.T) {
	u := newUploads(2)

	release := make(chan struct{})
	defer close(release)

	for range 2 {
		u.submit(func() { <-release })
	}

	finished, waited, left := u.drain(50 * time.Millisecond)
	if finished {
		t.Fatal("drain reported success while two uploads were still blocked")
	}

	if left != 2 {
		t.Errorf("drain abandoned %d uploads, want 2", left)
	}

	if waited < 50*time.Millisecond {
		t.Errorf("drain waited %s, less than the timeout it was given", waited)
	}
}
