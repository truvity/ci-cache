// Package gc keeps the disk tier inside its volume.
//
// The budget is derived from the filesystem, not configured: a cache whose
// size is a number in a values file is a cache that overflows the first time
// somebody resizes the PVC and forgets to change it. statfs is asked what the
// volume is, a floor of free space is left for everything else on it, and
// eviction runs between a high and a low watermark so that a busy cache
// evicts in batches instead of once per Put.
//
// Nothing here is on a Put's critical path. A Put tells the collector that
// the volume grew and carries on; only an object that cannot fit at all makes
// a writer wait, and only for one pass, after which it is refused with
// tier.ErrNoSpace. A build that waits on somebody else's eviction is a build
// for which the cache has made things worse.
package gc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/disk"
	"github.com/truvity/ci-cache/engine/tier"
)

const (
	// The watermarks used when the configuration says nothing. They match
	// config.Default(); they are repeated rather than imported so that a GC
	// built from a zero config.GC -- which a test or an embedded use does --
	// still behaves, instead of evicting everything down to zero percent.
	defaultFloor = 10
	defaultHigh  = 95
	defaultLow   = 85

	// defaultInterval is the backstop sweep. Notify carries the load; this
	// is what catches a volume that grew without a Put -- a co-tenant
	// filling the disk, a restore dropping files in.
	defaultInterval = 30 * time.Second

	// defaultNegativeInterval is how often expired negative entries are
	// swept. They are small and expire lazily on read as well, so this only
	// has to stop a burst of one-shot lookups from being remembered forever.
	defaultNegativeInterval = 30 * time.Second

	// defaultBudgetRefresh re-reads statfs. The volume's size changes when
	// somebody resizes it, which is rare, and the reading is a syscall, so
	// once a minute is both often enough and free.
	defaultBudgetRefresh = time.Minute

	// fallbackBudget stands in when statfs cannot be read at all.
	//
	// It is deliberately enormous. A collector that treats an unreadable
	// filesystem as a zero budget refuses every write, which turns a
	// monitoring problem into a total cache outage; letting the filesystem
	// itself say ENOSPC is the lesser failure, and it is visible.
	fallbackBudget = int64(1) << 60
)

// A GC is what the disk tier asks about space. Asserting it here means a
// signature change breaks the build rather than the wiring.
var _ disk.SpaceManager = (*GC)(nil)

// GC is the disk tier's garbage collector.
type GC struct {
	disk *disk.Disk
	cfg  config.GC

	interval      time.Duration
	negInterval   time.Duration
	budgetRefresh time.Duration
	capacity      func(dir string) (int64, error)

	budget  atomic.Int64
	evicted atomic.Int64
	runs    atomic.Int64

	// wake has room for one. A thousand Puts between two passes must not
	// queue a thousand passes, and must not block a single one of those Puts
	// either -- one pending wake-up says everything they all had to say.
	wake chan struct{}

	stop     chan struct{}
	stopOnce sync.Once
	started  atomic.Bool
	wg       sync.WaitGroup
}

// Option configures a GC.
type Option func(*GC)

// WithInterval sets the backstop sweep period.
func WithInterval(d time.Duration) Option {
	return func(g *GC) { g.interval = d }
}

// WithNegativeInterval sets how often expired negative entries are swept.
func WithNegativeInterval(d time.Duration) Option {
	return func(g *GC) { g.negInterval = d }
}

// WithBudgetRefresh sets how often the filesystem is re-measured.
func WithBudgetRefresh(d time.Duration) Option {
	return func(g *GC) { g.budgetRefresh = d }
}

// WithCapacity replaces statfs with f, which reports the volume's total size
// in bytes.
//
// It exists for tests, which need a budget small enough to overflow in a
// second, and for a deployment where the cache shares a filesystem whose real
// size is not the number that should govern it.
func WithCapacity(f func(dir string) (int64, error)) Option {
	return func(g *GC) { g.capacity = f }
}

// New returns a collector for d.
//
// The budget is measured immediately, so Admit is answerable before Start is
// ever called -- a tier that was handed a collector must not refuse or admit
// writes on the strength of a zero budget during start-up.
func New(d *disk.Disk, cfg config.GC, opts ...Option) *GC {
	g := &GC{
		disk:          d,
		cfg:           cfg,
		interval:      defaultInterval,
		negInterval:   defaultNegativeInterval,
		budgetRefresh: defaultBudgetRefresh,
		capacity:      fsCapacity,
		wake:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
	}
	for _, o := range opts {
		o(g)
	}
	g.refreshBudget()
	return g
}

// Start runs the collector until ctx is cancelled or Stop is called.
func (g *GC) Start(ctx context.Context) {
	if !g.started.CompareAndSwap(false, true) {
		return
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.loop(ctx)
	}()
}

// Stop ends the collector and waits for its goroutine.
func (g *GC) Stop() {
	g.stopOnce.Do(func() { close(g.stop) })
	g.wg.Wait()
}

// Notify tells the collector that the volume grew.
//
// It never blocks and never returns anything, because its caller is a Put
// that has just committed an object and owes the writer an answer.
func (g *GC) Notify() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Budget is the bytes the cache may occupy.
func (g *GC) Budget() int64 { return g.budget.Load() }

// EvictedBytes is everything this collector has reclaimed since it was built.
func (g *GC) EvictedBytes() int64 { return g.evicted.Load() }

// Runs is how many eviction passes have been made, whether or not they found
// anything to do. A cache whose Runs climbs while EvictedBytes does not is a
// cache comfortably inside its budget, which is the shape to expect.
func (g *GC) Runs() int64 { return g.runs.Load() }

// Collect makes one pass now, synchronously.
//
// The admin API's "collect" button and the tests use it. Ordinary operation
// does not: the background loop is what keeps the volume in bounds.
func (g *GC) Collect() { g.collect(0) }

// Admit reports whether the volume can take size more bytes.
//
// The common answer is yes and costs one atomic read. It is only when the
// object genuinely does not fit that a writer pays for a pass here, and only
// one: an object that still does not fit after everything evictable has gone
// is refused with tier.ErrNoSpace, which tells the chain to carry on
// uncached rather than to fail the build.
func (g *GC) Admit(size int64) error {
	if size <= 0 {
		return nil
	}
	budget := g.Budget()
	if size > budget {
		// No amount of eviction makes room for this one. Say so now rather
		// than emptying the whole cache for an object that will not fit into
		// the empty volume either.
		return fmt.Errorf("gc: object of %d bytes cannot fit a budget of %d: %w", size, budget, tier.ErrNoSpace)
	}
	if g.disk.Used()+size <= budget {
		g.Notify()
		return nil
	}
	g.collect(size)
	if g.disk.Used()+size > budget {
		return fmt.Errorf("gc: %d bytes used of %d after a full pass, no room for %d: %w",
			g.disk.Used(), budget, size, tier.ErrNoSpace)
	}
	return nil
}

// --- internals --------------------------------------------------------

func (g *GC) loop(ctx context.Context) {
	sweep := time.NewTicker(g.interval)
	defer sweep.Stop()
	neg := time.NewTicker(g.negInterval)
	defer neg.Stop()
	budget := time.NewTicker(g.budgetRefresh)
	defer budget.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stop:
			return
		case <-g.wake:
			g.collect(0)
		case <-sweep.C:
			g.collect(0)
		case <-neg.C:
			g.disk.ExpireNegatives()
		case <-budget.C:
			g.refreshBudget()
		}
	}
}

// collect is one pass. headroom is bytes a caller is about to write and needs
// room for on top of the low watermark.
func (g *GC) collect(headroom int64) {
	g.runs.Add(1)
	budget := g.Budget()
	if budget <= 0 {
		return
	}

	// Front-ends over their own cap go first, whatever the volume's total
	// looks like. That is the point of a sub-budget: one front-end that has
	// run away must not be able to evict every other front-end's objects by
	// pushing the volume over its high watermark.
	g.enforceSubBudgets(budget)

	used := g.disk.Used()
	if used+headroom <= pct(budget, g.high()) {
		return
	}
	target := pct(budget, g.low()) - headroom
	if target < 0 {
		target = 0
	}
	if used > target {
		g.evicted.Add(g.disk.Evict(used - target))
	}
}

func (g *GC) enforceSubBudgets(budget int64) {
	for frontend, percent := range g.cfg.Budgets {
		if percent <= 0 || percent > 100 {
			continue
		}
		limit := pct(budget, percent)
		_, used := g.disk.UsedBy(frontend)
		if used > limit {
			g.evicted.Add(g.disk.EvictFrontend(frontend, used-limit))
		}
	}
}

// refreshBudget re-measures the volume.
//
// A measurement that fails leaves the previous budget in place: the volume
// did not change size because a syscall failed, and the last good reading is
// a far better guess than zero.
func (g *GC) refreshBudget() {
	if g.cfg.BudgetBytes > 0 {
		g.budget.Store(g.cfg.BudgetBytes)
		return
	}
	total, err := g.capacity(g.disk.Dir())
	if err != nil || total <= 0 {
		if g.budget.Load() <= 0 {
			g.budget.Store(fallbackBudget)
		}
		return
	}
	g.budget.Store(pct(total, 100-g.floor()))
}

func (g *GC) floor() int {
	if g.cfg.Floor < 0 || g.cfg.Floor > 90 {
		return defaultFloor
	}
	if g.cfg.Floor == 0 && g.cfg.High == 0 && g.cfg.Low == 0 {
		// A wholly zero config.GC is "nothing was said", not "leave no free
		// space and evict down to zero".
		return defaultFloor
	}
	return g.cfg.Floor
}

func (g *GC) high() int {
	if g.cfg.High <= 0 || g.cfg.High > 100 || g.cfg.High <= g.cfg.Low {
		return defaultHigh
	}
	return g.cfg.High
}

func (g *GC) low() int {
	if g.cfg.Low <= 0 || g.cfg.Low >= 100 || g.cfg.Low >= g.high() {
		return defaultLow
	}
	return g.cfg.Low
}

// pct is v*percent/100 in a form that cannot overflow for any real volume:
// the division is done first when v is large enough for the multiplication to
// matter, which costs a little precision and no correctness.
func pct(v int64, percent int) int64 {
	if v > (1<<62)/100 {
		return (v / 100) * int64(percent)
	}
	return v * int64(percent) / 100
}
