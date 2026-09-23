// Package agent is the cache as the Go toolchain sees it.
//
// It is a GOCACHEPROG program: the toolchain starts it, speaks a small JSON
// protocol to it over stdin and stdout, and asks it for every compile
// action's result. That makes it the one component here whose failure is not
// a slow build but a broken one -- when GOCACHEPROG answers an error, `go
// build` fails, and no amount of a warm cache elsewhere makes up for it.
//
// So this package is written around a single rule: the only error that ever
// reaches the toolchain is a local disk failure. A server that is down, a
// bucket that refuses, a network that has gone away -- each of those is a
// miss on the way in and a dropped write on the way out, counted and
// reported, never returned.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creachadair/gocache"

	"github.com/truvity/ci-cache/agent/breaker"
	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/bucket"
	"github.com/truvity/ci-cache/engine/chain"
	"github.com/truvity/ci-cache/engine/disk"
	"github.com/truvity/ci-cache/engine/gc"
	"github.com/truvity/ci-cache/engine/remote"
	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/frontend/gobuild"
)

const (
	// probeTimeout is how long a back-end gets to prove it is there. It is
	// deliberately short: a runner that waits on a dead address has already
	// lost more than the cache was ever going to save it.
	probeTimeout = 2 * time.Second

	// startBudget is the whole of start-up, reachable back-ends or not. The
	// toolchain is blocked on us for all of it, on every `go build` in the
	// job, so it is a budget and not an aspiration.
	startBudget = 2400 * time.Millisecond

	// objectMaxAge is how long a materialised object survives without being
	// rewritten. See objectDir.prune for why being wrong about it is cheap.
	objectMaxAge = 7 * 24 * time.Hour
)

// Options is everything the agent needs to know.
type Options struct {
	// Remote is the cache server's base URL, including the front-end path
	// (".../go/build"). Empty runs without a server, which is what `direct`
	// mode is.
	Remote string

	// CacheDir is the local directory. Empty picks one under the user's
	// cache directory, so that a developer running this by hand does not
	// have to choose.
	CacheDir string

	// LocalBudget caps the local tier in bytes. Zero leaves the disk tier's
	// own default, which is derived from the filesystem.
	LocalBudget int64

	// Bucket, when set, puts the object store directly behind the local
	// disk. On a runner that is a fallback for a server that is down; on a
	// laptop it is the whole chain.
	Bucket *config.Store

	// Metrics prints a one-line summary to stderr at exit. The habit comes
	// from GOCACHE_METRICS, and it is worth keeping: a job log that does not
	// say what the cache did is a job nobody can tell was slow because of it.
	Metrics bool

	// Label names this agent in that summary -- a job name, a repository,
	// whatever makes the line findable in a log that has several.
	Label string

	// Logf receives the agent's own logs. It must not write to stdout: that
	// is the protocol's channel, and a log line in it corrupts the stream.
	Logf func(string, ...any)
}

// Agent is a constructed GOCACHEPROG server, before it is serving.
//
// New and Serve are split so that start-up is testable on its own. Whether
// the chain came up degraded is a property of start-up, and a test that has
// to speak the cache protocol to find out has not tested start-up.
type Agent struct {
	opts    Options
	logf    func(string, ...any)
	dir     string
	objects *objectDir

	d      *disk.Disk
	gcr    *gc.GC
	chain  *chain.Chain
	meters []*meter
	cache  *gobuild.Cache

	degraded bool
	started  time.Time

	// These count what the TOOLCHAIN asked for. The per-tier meters count
	// tier reads, which is a different number: answering one action means
	// reading its record and then its output, so a single hit shows up as
	// two reads of whichever tier held them.
	gets, hits, drops atomic.Int64

	closeOnce sync.Once
}

// New builds the agent's chain. It returns an error only when the LOCAL
// cache cannot be set up: that is the one failure the agent has no answer
// for, because every other tier is optional by construction.
func New(ctx context.Context, opts Options) (*Agent, error) {
	started := time.Now()
	deadline := started.Add(startBudget)

	if opts.Logf == nil {
		opts.Logf = stderrLogf
	}
	dir, err := cacheDir(opts.CacheDir)
	if err != nil {
		return nil, err
	}

	a := &Agent{opts: opts, logf: opts.Logf, dir: dir, started: started}

	// The materialised objects and the disk tier live in sibling directories
	// under the cache dir. Keeping them apart means neither has to know the
	// other's layout, and a tier that changes how it stores things cannot
	// collide with a file the compiler is holding open.
	if a.objects, err = newObjectDir(filepath.Join(dir, "objects")); err != nil {
		return nil, err
	}
	if a.d, err = disk.New(filepath.Join(dir, "tier")); err != nil {
		return nil, fmt.Errorf("agent: local cache in %s: %w", dir, err)
	}

	tiers := []tier.Tier{a.meterFor(a.d)}
	if opts.Remote != "" {
		r, err := a.dial(ctx, opts.Remote, deadline)
		if err != nil {
			// One line, naming the address, because the next question anyone
			// asks is "which server did it try?". Everything after this
			// point behaves as if the remote had never been configured.
			a.degraded = true
			a.warnf("remote cache %s is unreachable (%v): serving from the local cache only", opts.Remote, err)
		} else {
			tiers = append(tiers, a.meterFor(breaker.New(r)))
		}
	}
	if opts.Bucket != nil {
		bctx, cancel := withBudget(ctx, deadline)
		b, err := a.openBucket(bctx, *opts.Bucket)
		cancel()
		if err != nil {
			a.degraded = true
			a.warnf("object store %s is unavailable (%v): it will not be used", opts.Bucket.Bucket, err)
		} else {
			tiers = append(tiers, a.meterFor(breaker.New(b)))
		}
	}

	a.chain = chain.New(tiers)
	a.cache = gobuild.New(a.chain, config.GoBuild{Enabled: true})

	// The agent has no admin port and nobody to ask, so its disk tier is
	// kept inside its budget by the same collector the server uses.
	gcCfg := config.Default().GC
	switch {
	case opts.LocalBudget > 0:
		gcCfg.BudgetBytes = opts.LocalBudget
	default:
		// Derived, and from the cgroup rather than from statfs when there is
		// one: see cgroup.go for why the filesystem is the wrong question
		// inside a container. Saying which source was used matters -- the
		// two answers differ by two orders of magnitude, and the symptom of
		// picking the wrong one is a job that dies with no error.
		if budget, ok := budgetFromMemoryLimit(); ok {
			gcCfg.BudgetBytes = budget
			a.logf("local cache budget %d bytes, derived from the container's memory limit", budget)
		} else {
			a.logf("local cache budget derived from the filesystem: no container memory limit found")
		}
	}
	a.gcr = gc.New(a.d, gcCfg)
	a.gcr.Start(ctx)

	return a, nil
}

// Run is the whole program: build the chain, serve the toolchain on stdin and
// stdout until it goes away, and say what happened.
func Run(ctx context.Context, opts Options) error {
	a, err := New(ctx, opts)
	if err != nil {
		return err
	}
	serveErr := a.Serve(ctx, os.Stdin, os.Stdout)
	a.Close()
	if opts.Metrics {
		// stderr, and one line. The toolchain owns stdout, and a summary
		// spread over ten lines is a summary nobody greps for twice.
		fmt.Fprintln(os.Stderr, a.Stats().Line())
	}
	return serveErr
}

// Serve speaks the cache protocol until in reports EOF or ctx ends.
func (a *Agent) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	// Materialised objects are pruned behind the server rather than before
	// it: start-up has a budget measured in milliseconds and a directory
	// walk is measured in whatever the filesystem feels like.
	go func() {
		if n, freed := a.objects.prune(objectMaxAge); n > 0 {
			a.logf("pruned %d materialised objects (%s)", n, humanBytes(freed))
		}
	}()

	srv := &gocache.Server{
		Get:   a.get,
		Put:   a.put,
		Close: a.protocolClose,
		Logf:  a.logf,
	}
	return srv.Run(ctx, in, out)
}

// Close stops the agent's background work and writes the local tier's index.
//
// The index is why this is not optional. An agent is started and stopped once
// per `go build`, and without a written index the next one walks the whole
// cache directory to rediscover what is in it -- which on a warm runner is
// the largest thing the agent does.
//
// It is safe to call twice, because Run calls it and a caller that built the
// agent itself will too.
func (a *Agent) Close() {
	a.closeOnce.Do(func() {
		if a.gcr != nil {
			a.gcr.Stop()
		}
		if a.d != nil {
			if err := a.d.Close(); err != nil {
				a.warnf("local cache index not persisted (%v): the next build will rebuild it", err)
			}
		}
	})
}

// Degraded reports whether the chain that came up is the chain that was
// asked for. It is the single fact a caller needs in order to decide whether
// a slow build is worth investigating.
func (a *Agent) Degraded() bool { return a.degraded }

// TierNames is the chain, front to back.
func (a *Agent) TierNames() []string {
	names := make([]string, 0, len(a.meters))
	for _, m := range a.meters {
		names = append(names, m.Name())
	}
	return names
}

// Dir is the local cache directory the agent settled on.
func (a *Agent) Dir() string { return a.dir }

// get answers the toolchain's "get".
//
// Every path out of here that is not a hit is a miss. That includes a remote
// that failed, a bucket that refused and a chain that returned something
// unreadable: the toolchain's only use for an error is to stop the build.
func (a *Agent) get(ctx context.Context, actionID string) (outputID, diskPath string, _ error) {
	a.gets.Add(1)
	id, body, m, ok, err := a.cache.Get(ctx, actionID)
	if err != nil {
		a.logf("get %s: %v (treated as a miss)", actionID, err)
		return "", "", nil
	}
	if !ok {
		return "", "", nil
	}
	defer body.Close()

	path, _, err := a.objects.write(id, m.Size, m.ModTime, body)
	if err == nil {
		a.hits.Add(1)
	}
	if err != nil {
		// The local filesystem is the one dependency with no fallback: there
		// is nowhere else to put a file the compiler is about to open, and
		// pretending this was a miss would hand back a path that is not
		// there.
		return "", "", err
	}
	return id, path, nil
}

// put answers the toolchain's "put".
//
// The object is landed locally FIRST, because that is what the response has
// to name and what the next build will read. Recording it in the chain comes
// after and is allowed to fail.
func (a *Agent) put(ctx context.Context, obj gocache.Object) (string, error) {
	modTime := obj.ModTime
	if modTime.IsZero() {
		modTime = time.Now()
	}
	path, size, err := a.objects.write(obj.OutputID, obj.Size, modTime, obj.Body)
	if err != nil {
		return "", err
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("agent: reread object %s: %w", obj.OutputID, err)
	}
	defer f.Close()

	if err := a.cache.Put(ctx, obj.ActionID, obj.OutputID, size, modTime, f); err != nil {
		if !errors.Is(err, tier.ErrExists) {
			a.drops.Add(1)
			a.logf("put %s: %v (dropped)", obj.ActionID, err)
		}
	}
	return path, nil
}

// protocolClose is called once, when the toolchain closes the stream.
//
// It exists mostly so that the server advertises "close" and waits for us
// instead of stopping the moment stdin ends; there is nothing here that has
// to be flushed, because a put is only answered once its object is on disk.
func (a *Agent) protocolClose(context.Context) error { return nil }

// dial constructs the remote tier and proves it is answering.
//
// The probe is the whole reason a degraded chain is possible at all: without
// it the first compile action of the build would be the thing that discovers
// the server is gone, and it would discover it one connection timeout at a
// time.
func (a *Agent) dial(ctx context.Context, addr string, deadline time.Time) (tier.Tier, error) {
	r, err := remote.New(addr)
	if err != nil {
		return nil, err
	}
	pctx, cancel := withBudget(ctx, deadline)
	defer cancel()
	if err := r.Probe(pctx); err != nil {
		return nil, err
	}
	return r, nil
}

// openBucket constructs the object store tier and proves it answers.
//
// Constructing it costs nothing and fails only on a credential or an
// endpoint, so without the reachability check a bucket that is not there
// would join the chain and be discovered one request at a time -- which is
// the cost the start-up probe exists to pay once.
func (a *Agent) openBucket(ctx context.Context, store config.Store) (tier.Tier, error) {
	b, err := bucket.New(ctx, store)
	if err != nil {
		return nil, err
	}
	if err := b.Reachable(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

func (a *Agent) meterFor(t tier.Tier) tier.Tier {
	m := newMeter(t)
	a.meters = append(a.meters, m)
	return m
}

func (a *Agent) warnf(format string, args ...any) {
	// The prefix is the contract with whatever is reading these lines: the
	// CLI routes anything carrying it to a warning and everything else to
	// debug, so a normal job log stays quiet and a degraded one does not.
	a.logf(WarnPrefix+format, args...)
}

// WarnPrefix marks the log lines that a level above debug must still show.
//
// The agent is handed a plain printf-style sink because that is what the
// protocol library takes, so severity has to travel in the text. A caller
// that wants structured logs splits on this; one that does not gets a line
// that reads correctly anyway.
const WarnPrefix = "WARN "

// withBudget bounds a call by what is left of start-up, never giving it more
// than a probe's worth. A budget that has already run out still buys a
// millisecond, so that the call fails rather than hangs on a context that was
// dead before it was made.
func withBudget(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	d := min(time.Until(deadline), probeTimeout)
	if d <= 0 {
		d = time.Millisecond
	}
	return context.WithTimeout(ctx, d)
}

// cacheDir settles on the local directory. A developer who has said nothing
// about where the cache goes should still get one, in the place their system
// already keeps caches.
func cacheDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("agent: no cache directory given and none could be derived: %w", err)
	}
	return filepath.Join(base, "ci-cache", "agent"), nil
}

// stderrLogf is the default log sink. It is stderr and not stdout because
// stdout carries the protocol; writing a log line there does not produce a
// noisy build, it produces a build that fails on a malformed response.
func stderrLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ci-cache agent: "+strings.TrimPrefix(format, WarnPrefix)+"\n", args...)
}
