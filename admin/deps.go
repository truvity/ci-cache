package admin

import (
	"context"
	"time"

	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/telemetry"
)

// Deps is everything the administrative service needs from the rest of the
// process.
//
// Every field is an interface this package declares, so that admin imports
// neither engine/disk nor engine/bucket. That is not tidiness: the disk and
// bucket tiers are meant to stay swappable -- an agent on a runner has a
// remote tier where a server has a bucket -- and a package that named the
// concrete types would have to be edited every time one of them moved. It is
// also what makes the tests below run over a few maps instead of a volume.
//
// The interfaces are deliberately the shapes engine/tier already defines
// (tier.Lister, tier.Deleter), so the CLI wires the real tiers in without an
// adapter.
type Deps struct {
	// Version is what the binary reports as its own. It is read back by
	// Stats, which is how an operator tells whether a rollout has landed.
	Version string
	// StartedAt is when the process came up. Every counter in Stats is since
	// this instant, and a rate computed from them is wrong without it.
	StartedAt time.Time

	// Disk is the volume: what it holds, what may be listed, and what may be
	// removed. Required.
	Disk DiskInfo
	// Bucket is the object store behind the disk. It is nil when no bucket
	// is configured -- an agent on a runner has none -- and WipeBucket then
	// refuses rather than pretending to have wiped something.
	Bucket BucketInfo
	// Stats is the counter set, normally the telemetry package's Recorder.
	// Required.
	Stats StatsSource
	// Queue is the write-behind queue in front of the bucket. Nil when there
	// is no bucket, and reported as a depth of zero.
	Queue QueueInfo
}

// DiskInfo is the disk tier as the administrative API needs it.
//
// It embeds tier.Lister and tier.Deleter because the disk tier already
// implements both; the extra methods are the ones only an operator asks for.
type DiskInfo interface {
	tier.Lister
	tier.Deleter

	// Stat and Delete are the two tier.Tier methods a single-key wipe needs:
	// Stat to report how many bytes went, Delete to remove them.
	Stat(ctx context.Context, key string) (tier.Meta, error)
	Delete(ctx context.Context, key string) error

	// Usage is the volume as a whole.
	Usage() Usage
	// PerFrontend is what each front-end holds on that volume, keyed by
	// front-end name.
	PerFrontend() map[string]FrontendUsage

	// Invalidate drops negative-cache entries under a prefix and reports
	// what went. It never touches a cached object: see the Invalidate RPC.
	Invalidate(ctx context.Context, prefix string) (entries, bytes int64, err error)
}

// BucketInfo is the object store as the administrative API needs it.
//
// One method, because there is exactly one thing an operator does to a bucket
// from here. Reading it is the data port's job and listing it costs money.
type BucketInfo interface {
	tier.Deleter
}

// StatsSource is the process's counters.
//
// telemetry.Recorder satisfies it. The type comes from telemetry rather than
// being redeclared here on purpose: the numbers a collector scrapes and the
// numbers this API reports must be the same numbers, and two structurally
// identical types copied between packages is how they stop being.
type StatsSource interface {
	Snapshot() telemetry.Snapshot
}

// QueueInfo is the write-behind queue in front of the bucket.
type QueueInfo interface {
	// Depth is how many objects are waiting. A number that only grows means
	// the bucket is slower than the cache is being filled.
	Depth() int64
}

// Usage is the disk tier as a whole.
type Usage struct {
	// UsedBytes and BudgetBytes are what the volume holds and may hold. The
	// budget is derived from statfs, so it moves when the volume is resized
	// and never has to be stated twice.
	UsedBytes   int64
	BudgetBytes int64
	// Entries is how many objects are on the volume.
	Entries int64
	// IndexCold is true while the index is still being rebuilt from the
	// filesystem. The cache serves throughout; its counters are incomplete,
	// and a reader that does not know that will read a half-built index as a
	// cache that lost most of its contents.
	IndexCold bool
}

// FrontendUsage is what one front-end holds.
type FrontendUsage struct {
	Entries int64
	Bytes   int64
	// NegativeEntries is how many "known absent" answers are cached. They
	// hold no bytes worth counting and are not part of Bytes.
	NegativeEntries int64
}
