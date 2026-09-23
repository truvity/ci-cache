package main

import (
	"context"

	"github.com/truvity/ci-cache/admin"
	"github.com/truvity/ci-cache/engine/bucket"
	"github.com/truvity/ci-cache/engine/disk"
	"github.com/truvity/ci-cache/engine/gc"
	"github.com/truvity/ci-cache/engine/tier"
)

// The adapters between what the engine offers and what the admin service
// asks for.
//
// They exist so that `admin` imports neither `engine/disk` nor
// `engine/bucket`: the admin API is a wire contract over an interface, and a
// package that reaches into the disk tier to answer a question would have to
// be rewritten the day that tier changes. Every one of them is a shape
// change and nothing more -- no policy lives here, which is why there is
// nothing to test beyond the compiler agreeing.

// diskInfo presents the disk tier and its collector as one thing, because
// "how full is the cache" is a question about both: the disk knows what it
// holds and only the collector knows what it may hold.
type diskInfo struct {
	d  *disk.Disk
	gc *gc.GC
	// frontends are the names to report usage for. The disk accounts per
	// front-end but cannot enumerate them -- a front-end with nothing
	// cached has left no trace on the volume -- so the list comes from the
	// configuration, and a front-end that is enabled and empty is reported
	// as empty rather than being missing from the page.
	frontends []string
}

func (a diskInfo) List(ctx context.Context, prefix, pageToken string, pageSize int) ([]tier.Entry, string, error) {
	return a.d.List(ctx, prefix, pageToken, pageSize)
}

func (a diskInfo) DeletePrefix(ctx context.Context, prefix string) (entries, bytes int64, err error) {
	return a.d.DeletePrefix(ctx, prefix)
}

func (a diskInfo) Stat(ctx context.Context, key string) (tier.Meta, error) {
	return a.d.Stat(ctx, key)
}

func (a diskInfo) Delete(ctx context.Context, key string) error {
	return a.d.Delete(ctx, key)
}

func (a diskInfo) Usage() admin.Usage {
	return admin.Usage{
		UsedBytes:   a.d.Used(),
		BudgetBytes: a.gc.Budget(),
		Entries:     a.d.Entries(),
		IndexCold:   a.d.IndexCold(),
	}
}

func (a diskInfo) PerFrontend() map[string]admin.FrontendUsage {
	out := make(map[string]admin.FrontendUsage, len(a.frontends))
	for _, f := range a.frontends {
		entries, bytes := a.d.UsedBy(f)
		out[f] = admin.FrontendUsage{Entries: entries, Bytes: bytes}
	}
	return out
}

// Invalidate drops negative entries only. The disk tier reports how many it
// removed but not their size, because a negative entry has none -- it is the
// absence of an object, held so that the same absence is not looked up
// upstream a hundred times a minute.
func (a diskInfo) Invalidate(_ context.Context, prefix string) (entries, bytes int64, err error) {
	return a.d.InvalidatePrefix(prefix), 0, nil
}

// queueInfo widens the upload queue's depth, which is an int because a
// channel's length is, to the int64 the wire format carries.
type queueInfo struct{ q *bucket.Queue }

func (a queueInfo) Depth() int64 { return int64(a.q.Depth()) }

// adminBucket and adminQueue turn a nil concrete pointer into a nil
// interface.
//
// Without them an installation with no object store would hand the admin
// service a non-nil interface holding a nil pointer, and its own nil check
// would pass -- so WipeBucket would answer by dereferencing nothing at all.
// It is the classic Go trap, and the two lines that avoid it are cheaper
// than the panic.
func adminBucket(b *bucket.Bucket) admin.BucketInfo {
	if b == nil {
		return nil
	}
	return b
}

func adminQueue(q *bucket.Queue) admin.QueueInfo {
	if q == nil {
		return nil
	}
	return queueInfo{q: q}
}
