package admin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/truvity/ci-cache/engine/tier"
	adminv1 "github.com/truvity/ci-cache/gen/admin/v1"
)

// Paging for List. A page size of zero means the default rather than "no
// entries": a caller that sends no page size wants entries, and answering
// nothing would look like an empty cache.
const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// bucketPrefix is what WipeBucket will accept.
//
// It is the shape the cache's own key layout takes -- a front-end segment
// ("go", "nix", "maven"), then path segments, then a trailing slash. The
// pattern is here rather than in the proto because a proto cannot refuse
// anything; it can only describe. And it is strict on purpose: this call
// deletes objects the disk tier cannot bring back, so the argument that
// reaches the store must look like something a human meant to type, not like
// a variable that expanded to nothing.
var bucketPrefix = regexp.MustCompile(`^[a-z][a-z0-9-]*(/[a-z0-9._=@-]+)*/$`)

// Stats reports the counters, the volume and the queue.
//
// It reads the same Recorder the collector observes, so a dashboard and a
// `ci-cache stats` run cannot disagree. That matters most where there is no
// collector at all -- a laptop, or an estate before the observability stack
// lands -- which is exactly when somebody needs to ask what the cache did.
func (s *Service) Stats(_ context.Context, _ *connect.Request[adminv1.StatsRequest]) (
	*connect.Response[adminv1.StatsResponse], error,
) {
	snap := s.deps.Stats.Snapshot()
	usage := s.deps.Disk.Usage()
	perFrontend := s.deps.Disk.PerFrontend()

	counters := make(map[string][]*adminv1.TierStats, len(snap.Frontends))
	for _, f := range snap.Frontends {
		tiers := make([]*adminv1.TierStats, 0, len(f.Tiers))
		for _, t := range f.Tiers {
			tiers = append(tiers, &adminv1.TierStats{
				Tier:         t.Tier,
				Gets:         t.Gets,
				Hits:         t.Hits,
				Misses:       t.Misses,
				Errors:       t.Errors,
				Puts:         t.Puts,
				BytesRead:    t.BytesRead,
				BytesWritten: t.BytesWritten,
			})
		}
		counters[f.Frontend] = tiers
	}

	// The two sources are unioned rather than one driving the other. A
	// front-end that has served nothing since start-up still holds bytes on
	// the volume, and one that has only missed holds none; reporting either
	// list alone hides a real front-end from whoever is looking for it.
	names := make([]string, 0, len(counters)+len(perFrontend))
	seen := make(map[string]bool, len(counters)+len(perFrontend))
	for name := range counters {
		names = append(names, name)
		seen[name] = true
	}
	for name := range perFrontend {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	frontends := make([]*adminv1.FrontendStats, 0, len(names))
	for _, name := range names {
		u := perFrontend[name]
		frontends = append(frontends, &adminv1.FrontendStats{
			Frontend:        name,
			Tiers:           counters[name],
			Entries:         u.Entries,
			Bytes:           u.Bytes,
			NegativeEntries: u.NegativeEntries,
		})
	}

	var depth int64
	if s.deps.Queue != nil {
		depth = s.deps.Queue.Depth()
	}

	return connect.NewResponse(&adminv1.StatsResponse{
		Version:          s.deps.Version,
		StartedAt:        timestamppb.New(s.deps.StartedAt),
		Frontends:        frontends,
		DiskUsedBytes:    usage.UsedBytes,
		DiskBudgetBytes:  usage.BudgetBytes,
		UploadQueueDepth: depth,
		IndexCold:        usage.IndexCold,
	}), nil
}

// List enumerates the disk index.
//
// The DISK index, and never the bucket. Listing a bucket costs a request per
// thousand objects and answers a different question: "what has this estate
// ever cached" rather than "what is on this volume now". The questions an
// operator brings here -- what is filling the disk, what will the next
// eviction take -- are about the volume, and answering them from the store
// would be slower, billable, and wrong.
func (s *Service) List(ctx context.Context, req *connect.Request[adminv1.ListRequest]) (
	*connect.Response[adminv1.ListResponse], error,
) {
	size := int(req.Msg.PageSize)
	switch {
	case size <= 0:
		size = defaultPageSize
	case size > maxPageSize:
		// Clamped rather than refused. A caller that asks for a million gets
		// a page and a token, which is the answer it wanted; an error would
		// only teach it to retry with a smaller number.
		size = maxPageSize
	}

	entries, next, err := s.deps.Disk.List(ctx, req.Msg.Prefix, req.Msg.PageToken, size)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list %q: %w", req.Msg.Prefix, err))
	}

	out := make([]*adminv1.Entry, 0, len(entries))
	for _, e := range entries {
		entry := &adminv1.Entry{Key: e.Key, Size: e.Size, Immutable: e.Immutable}
		// A zero time means the index has no access record for the entry --
		// a cold index mid-rebuild. Sending the zero instant would claim the
		// object was last touched in year one, and every "oldest entries"
		// view would show the whole cache.
		if !e.LastAccess.IsZero() {
			entry.LastAccess = timestamppb.New(e.LastAccess)
		}
		out = append(out, entry)
	}
	return connect.NewResponse(&adminv1.ListResponse{Entries: out, NextPageToken: next}), nil
}

// WipeDisk removes one key, or everything under a prefix.
//
// The trailing slash decides: "go/build/" is a prefix, "go/build/ab/cdef" is
// one object. It is the same rule the key layout already uses, so an operator
// who can read a key can predict what a wipe will take.
//
// Unlike WipeBucket this accepts any prefix but the empty one, because what
// it removes is recoverable: the bucket still has the objects, and the cost
// of a wide wipe is a slow morning rather than a rebuild. The empty string is
// still refused -- an argument that expanded to nothing is never what
// somebody meant by "wipe the disk", and emptying the whole volume should
// take as many calls as there are front-ends on it.
func (s *Service) WipeDisk(ctx context.Context, req *connect.Request[adminv1.WipeDiskRequest]) (
	*connect.Response[adminv1.WipeDiskResponse], error,
) {
	target := req.Msg.PrefixOrKey
	if target == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("prefix_or_key is empty: name a prefix ending in \"/\" (for example \"go/build/\") "+
				"or one exact key"))
	}

	var entries, bytes int64
	var err error
	if strings.HasSuffix(target, "/") {
		entries, bytes, err = s.deps.Disk.DeletePrefix(ctx, target)
	} else {
		entries, bytes, err = s.deleteOne(ctx, target)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("wipe disk %q: %w", target, err))
	}

	s.log.Info("admin: wiped disk",
		"caller", caller(req.Peer()),
		"target", target,
		"entries", entries,
		"bytes", bytes)

	return connect.NewResponse(&adminv1.WipeDiskResponse{Removed: removed(entries, bytes)}), nil
}

// deleteOne removes one exact key and reports what it was worth.
//
// The Stat comes first because after the Delete there is nothing left to
// measure. A key that is already gone reports zero and is not an error: a
// wipe that runs twice -- a retried script, two operators on the same
// incident -- must not fail the second time.
func (s *Service) deleteOne(ctx context.Context, key string) (entries, bytes int64, err error) {
	m, err := s.deps.Disk.Stat(ctx, key)
	if errors.Is(err, tier.ErrNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if err := s.deps.Disk.Delete(ctx, key); err != nil && !errors.Is(err, tier.ErrNotFound) {
		return 0, 0, err
	}
	return 1, m.Size, nil
}

// WipeBucket deletes a prefix from the object store, and from the disk.
//
// This is the real reset: what goes here is not behind anything, and the next
// build pays to produce it again. Hence the two guards -- "" and "/" are
// refused outright, and the prefix must look like a key prefix this cache
// writes -- and hence its being a separate call from WipeDisk, so that one
// wrong argument to the cheap wipe cannot become the expensive one.
//
// The disk entries under the prefix go too, and that is not a convenience.
// Keeping a disk copy of what the bucket no longer has would make the next
// read return the object the operator just paid to delete, which is a cache
// that lies about a wipe having happened.
func (s *Service) WipeBucket(ctx context.Context, req *connect.Request[adminv1.WipeBucketRequest]) (
	*connect.Response[adminv1.WipeBucketResponse], error,
) {
	prefix := req.Msg.Prefix
	// Named explicitly before the pattern is consulted, because these two are
	// what an unset variable and a "just the root, surely" expand to, and the
	// error they deserve says which mistake was made.
	if prefix == "" || prefix == "/" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("refusing to wipe the whole bucket (prefix %q): "+
				"name a prefix such as \"go/build/\"", prefix))
	}
	if !bucketPrefix.MatchString(prefix) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("prefix %q is not a cache key prefix: it must start with a front-end segment, "+
				"use only [a-z0-9._=@-] in its path segments, and end in \"/\" (for example \"go/build/\")",
				prefix))
	}
	// The pattern above allows "." in a path segment -- a version like
	// "1.2.3" needs it -- which lets "." and ".." through as whole segments.
	// On the store they would be literal and merely nonsense, but the same
	// prefix is handed to the disk tier, which joins it onto the volume's
	// directory, and there ".." walks out of the volume.
	for _, seg := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if seg == "." || seg == ".." {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("prefix %q contains a %q segment: a wipe takes a key prefix, not a path to resolve",
					prefix, seg))
		}
	}
	if s.deps.Bucket == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("no bucket is configured: this installation caches to disk only"))
	}

	entries, bytes, err := s.deps.Bucket.DeletePrefix(ctx, prefix)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("wipe bucket %q: %w", prefix, err))
	}

	diskEntries, diskBytes, err := s.deps.Disk.DeletePrefix(ctx, prefix)
	if err != nil {
		// The bucket half already happened and cannot be undone, so it is
		// logged before the error is returned. Otherwise the only record of
		// what was deleted would be the response nobody receives, and the
		// disk is left holding objects the bucket no longer has.
		s.log.Error("admin: wiped bucket but not disk",
			"caller", caller(req.Peer()),
			"prefix", prefix,
			"bucketEntries", entries,
			"bucketBytes", bytes,
			"err", err)
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("wiped %d objects from the bucket, then failed to wipe the disk under %q "+
				"(the disk now holds objects the bucket does not; re-run WipeDisk): %w",
				entries, prefix, err))
	}

	s.log.Info("admin: wiped bucket",
		"caller", caller(req.Peer()),
		"prefix", prefix,
		"entries", entries,
		"bytes", bytes,
		"diskEntries", diskEntries,
		"diskBytes", diskBytes)

	return connect.NewResponse(&adminv1.WipeBucketResponse{
		Removed:         removed(entries, bytes),
		RemovedFromDisk: removed(diskEntries, diskBytes),
	}), nil
}

// Invalidate drops negative-cache entries under a prefix.
//
// Negative entries ONLY: no cached object is touched. This is the call for
// "the upstream has it now and we are still saying 404", which is a handful
// of remembered absences and not a reason to throw away a warm cache.
//
// An empty prefix is allowed here, unlike in the two wipes. Every negative
// entry is a memory of an absence, costs a re-check upstream to rebuild, and
// is rebuilt in one request; dropping all of them is a cheap thing to do by
// accident.
func (s *Service) Invalidate(ctx context.Context, req *connect.Request[adminv1.InvalidateRequest]) (
	*connect.Response[adminv1.InvalidateResponse], error,
) {
	prefix := req.Msg.Prefix
	entries, bytes, err := s.deps.Disk.Invalidate(ctx, prefix)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("invalidate %q: %w", prefix, err))
	}

	s.log.Info("admin: invalidated negative entries",
		"caller", caller(req.Peer()),
		"prefix", prefix,
		"entries", entries)

	return connect.NewResponse(&adminv1.InvalidateResponse{Removed: removed(entries, bytes)}), nil
}
