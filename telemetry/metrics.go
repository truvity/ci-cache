package telemetry

// The instrument names.
//
// They are constants rather than literals at the call site because they are a
// published interface: the dashboard queries them, the gate issues name them,
// and an alert that reads cicache.gc.runs keeps reading it across a rename
// only if the rename is one edit here and a red mark in TestMetricNames.
const (
	// MetricGet counts reads, by frontend, tier and outcome.
	MetricGet = "cicache.get"
	// MetricPut counts writes, by frontend, tier and outcome.
	MetricPut = "cicache.put"
	// MetricBytes counts bytes moved, by frontend, tier and direction.
	MetricBytes = "cicache.bytes"
	// MetricLatency is how long a tier took, by frontend, tier and op.
	MetricLatency = "cicache.latency"

	// MetricDiskUsedBytes is what the volume holds.
	MetricDiskUsedBytes = "cicache.disk.used_bytes"
	// MetricDiskBudgetBytes is what the volume may hold. It is derived from
	// statfs, so it moves on its own when a volume is resized; a dashboard
	// that divides one by the other therefore stays right without an edit.
	MetricDiskBudgetBytes = "cicache.disk.budget_bytes"
	// MetricDiskEntries is how many objects the volume holds.
	MetricDiskEntries = "cicache.disk.entries"

	// MetricGCEvictedBytes is how much eviction has freed.
	MetricGCEvictedBytes = "cicache.gc.evicted_bytes"
	// MetricGCRuns is how many times eviction has run.
	MetricGCRuns = "cicache.gc.runs"
	// MetricGCDuration is how long an eviction pass took.
	MetricGCDuration = "cicache.gc.duration"

	// MetricNegativeEntries is how many "known absent" answers are cached.
	MetricNegativeEntries = "cicache.negative.entries"
	// MetricUploadQueueDepth is how many objects are waiting for the bucket.
	// A depth that only grows means the bucket is slower than the cache is
	// being filled, which no other number says.
	MetricUploadQueueDepth = "cicache.upload.queue_depth"
	// MetricAgentDegraded is 1 while a runner's agent cannot reach the
	// server. Only the agent sets it; a server exports it as nothing at all,
	// which is how a query tells the two deployments apart.
	MetricAgentDegraded = "cicache.agent.degraded"
)

// The attribute keys. One spelling, because a dashboard cannot group by
// "frontend" and "front_end" at once and will not tell you which it got.
const (
	// AttrFrontend is which protocol's front-end the work belongs to.
	AttrFrontend = "frontend"
	// AttrTier is which tier did the work ("disk", "bucket", "remote").
	AttrTier = "tier"
	// AttrOutcome is how it ended: see the Outcome constants.
	AttrOutcome = "outcome"
	// AttrDirection is which way the bytes went: see the Direction constants.
	AttrDirection = "direction"
	// AttrOp is which tier method was called: see the Op constants.
	AttrOp = "op"
)

// The outcome values, shared by MetricGet and MetricPut.
//
// One vocabulary for both calls is what lets a dashboard put reads and writes
// on the same axis. It costs the put side a reading: on a write, "hit" means
// the tier already had the object -- an immutable key that returned ErrExists,
// so nothing was written -- and "miss" is the ordinary case where it was not
// there and now is.
const (
	// OutcomeHit is a read that was answered, or a write the tier already had.
	OutcomeHit = "hit"
	// OutcomeMiss is a read that found nothing, or a write that stored.
	OutcomeMiss = "miss"
	// OutcomeError is a failure. A read that misses is NOT one of these: a
	// miss is the ordinary case, and counting it as an error would make every
	// cold cache look broken.
	OutcomeError = "error"
	// OutcomeDropped is work deliberately abandoned rather than failed -- a
	// full write-behind queue, or a tier with no room. It costs a later miss
	// and nothing else, which is why it must not land in OutcomeError where
	// an alert would see it.
	OutcomeDropped = "dropped"
)

// The direction values for MetricBytes.
const (
	// DirectionRead is bytes served out of a tier.
	DirectionRead = "read"
	// DirectionWritten is bytes stored into a tier.
	DirectionWritten = "written"
)

// The op values for MetricLatency, one per tier.Tier method that does I/O.
const (
	// OpGet is tier.Tier.Get.
	OpGet = "get"
	// OpPut is tier.Tier.Put.
	OpPut = "put"
	// OpStat is tier.Tier.Stat.
	OpStat = "stat"
	// OpDelete is tier.Tier.Delete.
	OpDelete = "delete"
)

// MetricNames is every instrument this package exports, sorted.
//
// It exists so that adding an instrument is a change to a committed golden
// file, reviewed next to the dashboard change that has to accompany it. A
// metric nothing graphs is a metric nobody notices has stopped.
var MetricNames = []string{
	MetricAgentDegraded,
	MetricBytes,
	MetricDiskBudgetBytes,
	MetricDiskEntries,
	MetricDiskUsedBytes,
	MetricGCDuration,
	MetricGCEvictedBytes,
	MetricGCRuns,
	MetricGet,
	MetricLatency,
	MetricNegativeEntries,
	MetricPut,
	MetricUploadQueueDepth,
}
