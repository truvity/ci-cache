package agent

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Why this file exists.
//
// The toolchain calls put once per compiled object and waits for the answer
// before it goes on. Recording that object in the chain means a write to the
// local tier and an HTTP round trip to the cache server, and doing both
// before answering puts the whole of it on the critical path of the build.
// A `go build` writes thousands of objects, so that cost is paid thousands of
// times in series.
//
// It is also the measured difference against the tool this replaces.
// tailscale/go-cache-plugin answers a put as soon as the object is on local
// disk and pushes to S3 from a bounded background group, waiting once at the
// end. That is why it beat this agent on every recipe while talking to a
// bucket in another datacentre rather than to a server on the same LAN: its
// uploads were never on the critical path and ours were.
//
// So: land the object locally, answer, and record it behind the toolchain's
// back.

const (
	// uploadTimeout bounds one recording. A server that has stopped
	// answering must not pin a worker for the rest of the build; losing the
	// record costs somebody a slower build later and nothing now.
	uploadTimeout = 1 * time.Minute

	// uploadDrainTimeout bounds the wait at the end. Uploads are worth
	// waiting for -- they are what makes the NEXT build fast -- but not
	// worth hanging a job over, and a job that appears to finish and then
	// sits there is a job somebody cancels.
	uploadDrainTimeout = 2 * time.Minute

	// minUploadWorkers is a floor under the worker count.
	//
	// Recording an object is I/O: a disk write and a socket. Sizing the pool
	// by CPU count would put two workers on a 2-vCPU runner, which is the
	// shape most of our runners have, and the drain at the end of a build
	// would then trickle out two at a time. The floor is a starting point
	// chosen for that reason and not a measured optimum -- `--upload-workers`
	// exists so the next person can measure it rather than argue about it.
	minUploadWorkers = 8
)

// uploads runs the recordings that the toolchain is no longer waiting for.
//
// It is a bounded pool rather than an unbounded queue on purpose. Unbounded,
// a build that outruns the network would accumulate one in-flight request per
// compiled object, and the agent would fail on file descriptors or memory
// somewhere past the point where anyone could tell why. Bounded, submit
// blocks once every worker is busy -- which is still nothing like the old
// behaviour, where every single put blocked on a round trip.
type uploads struct {
	limit chan struct{}
	wg    sync.WaitGroup

	// pending counts submitted-but-unfinished work, for the log line at the
	// end. It is not load-bearing: the WaitGroup decides when the drain is
	// done, and this only decides what the message says.
	pending atomic.Int64
}

func newUploads(workers int) *uploads {
	if workers <= 0 {
		workers = defaultUploadWorkers()
	}

	return &uploads{limit: make(chan struct{}, workers)}
}

func defaultUploadWorkers() int {
	if n := runtime.NumCPU(); n > minUploadWorkers {
		return n
	}

	return minUploadWorkers
}

// submit runs f on a worker, blocking only while every worker is busy.
func (u *uploads) submit(f func()) {
	u.limit <- struct{}{}
	u.pending.Add(1)
	u.wg.Add(1)

	go func() {
		defer func() {
			u.pending.Add(-1)
			<-u.limit
			u.wg.Done()
		}()
		f()
	}()
}

// drain waits for everything submitted so far, and reports whether it
// finished inside d.
//
// A timeout here is not an error the build should see. The objects are on
// local disk either way; what is lost is that some of them were not recorded
// for the next build, and the caller says so in one line.
func (u *uploads) drain(d time.Duration) (finished bool, waited time.Duration, left int64) {
	start := time.Now()
	done := make(chan struct{})

	go func() {
		u.wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-done:
		return true, time.Since(start), 0
	case <-timer.C:
		return false, time.Since(start), u.pending.Load()
	}
}
