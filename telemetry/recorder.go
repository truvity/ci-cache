package telemetry

import (
	"sort"
	"sync"
	"time"
)

// Recorder is the process's own copy of every counter.
//
// It exists because a cache with no collector must still be able to answer
// "what did you do". The admin API's Stats reads this, and so does whatever
// OTel exports: the exporter registers OBSERVABLE instruments that read
// Snapshot at collection time rather than keeping sums of its own. That is
// the whole reason the type is here instead of inside the exporter -- two
// sets of counters incremented at the same call sites drift the first time
// somebody adds a call site to one of them, and the pair that disagrees is
// found months later by an operator who trusts the wrong one.
//
// Safe for concurrent use: every tier in the process records into one of
// these from whatever goroutine is serving.
type Recorder struct {
	// One mutex rather than per-key atomics. A snapshot has to be internally
	// consistent -- gets and hits read a microsecond apart make a hit rate
	// above one, which is the kind of number that costs an afternoon -- and
	// that needs a lock the readers can take too.
	mu    sync.Mutex
	tiers map[tierKey]*tierCounts
	ops   map[opKey]*opCounts
	gc    gcCounts

	// hook is set once at construction and never again, so reading it needs
	// no lock.
	hook Hook
}

// Hook receives the events that have no asynchronous instrument.
//
// Everything countable is exported by observing Snapshot, so there is exactly
// one store. A histogram has no observable form in OpenTelemetry, though: a
// distribution cannot be reconstructed from a total. Latency and GC duration
// therefore have to be handed to the exporter as they happen, and those two
// are the only things that travel this way.
type Hook interface {
	// Observe is one tier call's duration, matching MetricLatency.
	Observe(frontend, tier, op string, d time.Duration)
	// GCDuration is one eviction pass's duration, matching MetricGCDuration.
	GCDuration(d time.Duration)
}

type tierKey struct{ frontend, tier string }

type opKey struct{ frontend, tier, op string }

type tierCounts struct {
	// Indexed by outcome, so a new outcome is a new map entry and never a
	// silently dropped increment.
	get          map[string]int64
	put          map[string]int64
	bytesRead    int64
	bytesWritten int64
}

type opCounts struct {
	count int64
	total time.Duration
}

type gcCounts struct {
	runs    int64
	evicted int64
	total   time.Duration
}

// NewRecorder returns a recorder that keeps its counters and exports nothing.
//
// This is what a test uses, and what New returns when no collector is
// configured.
func NewRecorder() *Recorder { return newRecorder(nil) }

func newRecorder(h Hook) *Recorder {
	return &Recorder{
		tiers: make(map[tierKey]*tierCounts),
		ops:   make(map[opKey]*opCounts),
		hook:  h,
	}
}

func (r *Recorder) counts(frontend, tier string) *tierCounts {
	k := tierKey{frontend, tier}
	c := r.tiers[k]
	if c == nil {
		c = &tierCounts{get: make(map[string]int64), put: make(map[string]int64)}
		r.tiers[k] = c
	}
	return c
}

// Get records one read of a tier. outcome is one of the Outcome constants.
func (r *Recorder) Get(frontend, tier, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts(frontend, tier).get[outcome]++
}

// Put records one write to a tier. outcome is one of the Outcome constants;
// see the comment on them for what "hit" means on a write.
func (r *Recorder) Put(frontend, tier, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts(frontend, tier).put[outcome]++
}

// Bytes records bytes moved. direction is DirectionRead or DirectionWritten.
//
// It is separate from Get and Put because the bytes are only known when the
// body has been streamed, which is after the outcome is decided. A caller
// that reports zero is recorded as zero rather than skipped: an object of no
// bytes is a fact about the cache, not a missing measurement.
func (r *Recorder) Bytes(frontend, tier, direction string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.counts(frontend, tier)
	if direction == DirectionWritten {
		c.bytesWritten += n
		return
	}
	c.bytesRead += n
}

// Observe records how long one tier call took. op is one of the Op constants.
func (r *Recorder) Observe(frontend, tier, op string, d time.Duration) {
	r.mu.Lock()
	k := opKey{frontend, tier, op}
	c := r.ops[k]
	if c == nil {
		c = &opCounts{}
		r.ops[k] = c
	}
	c.count++
	c.total += d
	r.mu.Unlock()

	// Outside the lock: the exporter's histogram takes its own, and holding
	// two locks in an order nothing else knows about is how a metrics call
	// ends up deadlocking a request path.
	if r.hook != nil {
		r.hook.Observe(frontend, tier, op, d)
	}
}

// GC records one eviction pass: what it freed and how long it took.
func (r *Recorder) GC(evictedBytes int64, d time.Duration) {
	r.mu.Lock()
	r.gc.runs++
	r.gc.evicted += evictedBytes
	r.gc.total += d
	r.mu.Unlock()

	if r.hook != nil {
		r.hook.GCDuration(d)
	}
}

// Snapshot is every counter at one instant, per front-end and tier.
//
// Everything in it is sorted by name. Two reads of an unchanged cache are
// then identical byte for byte, which is what lets a test diff them and a
// human diff two `ci-cache stats` runs.
type Snapshot struct {
	// Frontends, sorted by name.
	Frontends []FrontendSnapshot
	// GC is process-wide: eviction is a property of the volume, not of any
	// one front-end that happens to live on it.
	GC GCSnapshot
}

// FrontendSnapshot is one front-end's counters.
type FrontendSnapshot struct {
	// Frontend is the front-end's name ("go/build", "nix", ...).
	Frontend string
	// Tiers, sorted by tier name.
	Tiers []TierSnapshot
	// Ops, sorted by tier then op. This is the latency total, kept so that a
	// process with no collector can still say where its time went.
	Ops []OpSnapshot
}

// TierSnapshot is one front-end's counters in one tier.
//
// It carries the raw per-outcome counts AND the few totals the admin API's
// wire format has fields for. Both, because they answer to different readers:
// the exporter needs the raw ones to put an outcome attribute on
// cicache.get and cicache.put, and dropping them would force it to keep a
// second set of counters -- the exact thing this type exists to prevent.
type TierSnapshot struct {
	// Tier is the tier's name ("disk", "bucket", "remote").
	Tier string
	// GetByOutcome and PutByOutcome are the raw counts, keyed by one of the
	// Outcome constants. They are copies: a caller may hold one while the
	// cache carries on recording.
	GetByOutcome map[string]int64
	PutByOutcome map[string]int64
	// Gets is every read, whatever its outcome.
	Gets int64
	// Hits and Misses are reads only. A write that found the object already
	// there is not a cache hit in the sense a hit rate means, so it is not
	// counted here.
	Hits   int64
	Misses int64
	// Errors is reads AND writes that failed. A tier that cannot be written
	// is failing, and an error line that showed only read failures would say
	// a broken bucket was healthy.
	Errors int64
	// Puts is every write, whatever its outcome.
	Puts int64
	// Dropped is work abandoned rather than failed, reads and writes both.
	Dropped int64
	// BytesRead and BytesWritten are what actually moved, not what was
	// promised by a Meta.
	BytesRead    int64
	BytesWritten int64
}

// OpSnapshot is how much time one tier method has taken.
type OpSnapshot struct {
	Tier  string
	Op    string
	Count int64
	Total time.Duration
}

// GCSnapshot is what eviction has done since the process started.
type GCSnapshot struct {
	Runs         int64
	EvictedBytes int64
	Total        time.Duration
}

// Snapshot returns the counters as one consistent view.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	byFrontend := make(map[string]*FrontendSnapshot)
	get := func(frontend string) *FrontendSnapshot {
		f := byFrontend[frontend]
		if f == nil {
			f = &FrontendSnapshot{Frontend: frontend}
			byFrontend[frontend] = f
		}
		return f
	}

	for k, c := range r.tiers {
		f := get(k.frontend)
		f.Tiers = append(f.Tiers, TierSnapshot{
			Tier:         k.tier,
			GetByOutcome: copyCounts(c.get),
			PutByOutcome: copyCounts(c.put),
			Gets:         sum(c.get),
			Hits:         c.get[OutcomeHit],
			Misses:       c.get[OutcomeMiss],
			Errors:       c.get[OutcomeError] + c.put[OutcomeError],
			Puts:         sum(c.put),
			Dropped:      c.get[OutcomeDropped] + c.put[OutcomeDropped],
			BytesRead:    c.bytesRead,
			BytesWritten: c.bytesWritten,
		})
	}
	for k, c := range r.ops {
		f := get(k.frontend)
		f.Ops = append(f.Ops, OpSnapshot{Tier: k.tier, Op: k.op, Count: c.count, Total: c.total})
	}

	s := Snapshot{
		GC: GCSnapshot{Runs: r.gc.runs, EvictedBytes: r.gc.evicted, Total: r.gc.total},
	}
	for _, f := range byFrontend {
		sort.Slice(f.Tiers, func(i, j int) bool { return f.Tiers[i].Tier < f.Tiers[j].Tier })
		sort.Slice(f.Ops, func(i, j int) bool {
			if f.Ops[i].Tier != f.Ops[j].Tier {
				return f.Ops[i].Tier < f.Ops[j].Tier
			}
			return f.Ops[i].Op < f.Ops[j].Op
		})
		s.Frontends = append(s.Frontends, *f)
	}
	sort.Slice(s.Frontends, func(i, j int) bool { return s.Frontends[i].Frontend < s.Frontends[j].Frontend })
	return s
}

func copyCounts(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sum(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}
