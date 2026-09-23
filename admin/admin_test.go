package admin_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/truvity/ci-cache/admin"
	"github.com/truvity/ci-cache/engine/tier"
	adminv1 "github.com/truvity/ci-cache/gen/admin/v1"
	"github.com/truvity/ci-cache/gen/admin/v1/adminv1connect"
	"github.com/truvity/ci-cache/telemetry"
)

// fakeDisk is the disk tier as a map.
//
// Every one of these tests runs over it rather than over a volume, which is
// what the Deps interfaces are for: a wipe that took a real directory would
// be slow, would need a temp dir per case, and would be testing the
// filesystem rather than the rule about which argument means what.
type fakeDisk struct {
	mu      sync.Mutex
	objects map[string]tier.Entry
	// negative is the negative cache: keys remembered as absent.
	negative map[string]bool
	usage    admin.Usage
	perFE    map[string]admin.FrontendUsage
	// listErr, when set, is what List returns.
	listErr error
}

func newFakeDisk(keys ...string) *fakeDisk {
	d := &fakeDisk{
		objects:  map[string]tier.Entry{},
		negative: map[string]bool{},
		perFE:    map[string]admin.FrontendUsage{},
	}
	for i, k := range keys {
		d.objects[k] = tier.Entry{
			Key:        k,
			Size:       int64(10 * (i + 1)),
			LastAccess: time.Unix(1700000000+int64(i), 0).UTC(),
		}
	}
	return d
}

func (d *fakeDisk) List(_ context.Context, prefix, pageToken string, pageSize int) ([]tier.Entry, string, error) {
	if d.listErr != nil {
		return nil, "", d.listErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	keys := make([]string, 0, len(d.objects))
	for k := range d.objects {
		if strings.HasPrefix(k, prefix) && k > pageToken {
			keys = append(keys, k)
		}
	}
	// Sorted, because a page token that is the last key returned only makes
	// sense over a stable order. A real index does the same.
	sort.Strings(keys)

	var next string
	if len(keys) > pageSize {
		keys = keys[:pageSize]
		next = keys[len(keys)-1]
	}
	out := make([]tier.Entry, 0, len(keys))
	for _, k := range keys {
		out = append(out, d.objects[k])
	}
	return out, next, nil
}

func (d *fakeDisk) DeletePrefix(_ context.Context, prefix string) (int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var entries, bytes int64
	for k, e := range d.objects {
		if strings.HasPrefix(k, prefix) {
			entries++
			bytes += e.Size
			delete(d.objects, k)
		}
	}
	return entries, bytes, nil
}

func (d *fakeDisk) Stat(_ context.Context, key string) (tier.Meta, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.objects[key]
	if !ok {
		return tier.Meta{}, tier.ErrNotFound
	}
	return tier.Meta{Size: e.Size, Immutable: e.Immutable}, nil
}

func (d *fakeDisk) Delete(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.objects, key)
	return nil
}

func (d *fakeDisk) Usage() admin.Usage                          { return d.usage }
func (d *fakeDisk) PerFrontend() map[string]admin.FrontendUsage { return d.perFE }

func (d *fakeDisk) Invalidate(_ context.Context, prefix string) (int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	for k := range d.negative {
		if strings.HasPrefix(k, prefix) {
			n++
			delete(d.negative, k)
		}
	}
	return n, 0, nil
}

func (d *fakeDisk) keys() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.objects))
	for k := range d.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fakeBucket records what it was asked to delete.
type fakeBucket struct {
	mu       sync.Mutex
	prefixes []string
	err      error
}

func (b *fakeBucket) DeletePrefix(_ context.Context, prefix string) (int64, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, 0, b.err
	}
	b.prefixes = append(b.prefixes, prefix)
	return 7, 700, nil
}

func (b *fakeBucket) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.prefixes...)
}

type fakeQueue struct{ depth int64 }

func (q fakeQueue) Depth() int64 { return q.depth }

// serve starts the service on a real listener and returns a client.
//
// Over the wire rather than by calling the methods directly, because half of
// what this package does is decide a connect.Code, and a code is only a code
// once it has been through the protocol. A handler that returned a bare
// error would pass a direct call and reach a client as Internal.
func serve(t *testing.T, deps admin.Deps) adminv1connect.AdminServiceClient {
	t.Helper()

	svc := admin.New(deps, admin.WithLogger(discardLogger()))
	path, h := svc.Handler()

	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return adminv1connect.NewAdminServiceClient(srv.Client(), srv.URL)
}

// discardLogger keeps a failing test's output to its assertion rather than to
// the log lines every mutating call is required to write.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func defaultDeps(disk *fakeDisk, bucket admin.BucketInfo) admin.Deps {
	rec := telemetry.NewRecorder()
	rec.Get("go/build", "disk", telemetry.OutcomeHit)
	rec.Get("go/build", "disk", telemetry.OutcomeMiss)
	rec.Get("go/build", "bucket", telemetry.OutcomeHit)
	rec.Put("go/build", "bucket", telemetry.OutcomeError)
	rec.Bytes("go/build", "disk", telemetry.DirectionRead, 512)
	rec.Bytes("go/build", "bucket", telemetry.DirectionWritten, 1024)

	return admin.Deps{
		Version:   "1.2.3",
		StartedAt: time.Unix(1700000000, 0).UTC(),
		Disk:      disk,
		Bucket:    bucket,
		Stats:     rec,
		Queue:     fakeQueue{depth: 4},
	}
}

func TestStats(t *testing.T) {
	t.Parallel()

	disk := newFakeDisk("go/build/aa", "nix/nar/bb")
	disk.usage = admin.Usage{UsedBytes: 900, BudgetBytes: 1000, Entries: 2, IndexCold: true}
	// "nix" has bytes on the volume and has served nothing; "go/build" has
	// served and holds bytes. Both must appear: a front-end that is missing
	// from Stats is one nobody thinks to look at.
	disk.perFE = map[string]admin.FrontendUsage{
		"go/build": {Entries: 1, Bytes: 10, NegativeEntries: 3},
		"nix":      {Entries: 1, Bytes: 20},
	}

	client := serve(t, defaultDeps(disk, &fakeBucket{}))
	resp, err := client.Stats(context.Background(), connect.NewRequest(&adminv1.StatsRequest{}))
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	msg := resp.Msg

	if msg.Version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", msg.Version)
	}
	if got := msg.StartedAt.AsTime(); !got.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("startedAt = %v, want 1700000000", got)
	}
	if msg.DiskUsedBytes != 900 || msg.DiskBudgetBytes != 1000 {
		t.Errorf("disk = %d/%d, want 900/1000", msg.DiskUsedBytes, msg.DiskBudgetBytes)
	}
	if !msg.IndexCold {
		t.Error("indexCold = false, want true")
	}
	if msg.UploadQueueDepth != 4 {
		t.Errorf("uploadQueueDepth = %d, want 4", msg.UploadQueueDepth)
	}

	if len(msg.Frontends) != 2 {
		t.Fatalf("got %d front-ends, want 2: %v", len(msg.Frontends), msg.Frontends)
	}
	if msg.Frontends[0].Frontend != "go/build" || msg.Frontends[1].Frontend != "nix" {
		t.Errorf("front-ends = %q, %q; want go/build, nix (sorted)",
			msg.Frontends[0].Frontend, msg.Frontends[1].Frontend)
	}

	goBuild := msg.Frontends[0]
	if goBuild.NegativeEntries != 3 || goBuild.Entries != 1 || goBuild.Bytes != 10 {
		t.Errorf("go/build usage = %+v, want entries 1, bytes 10, negative 3", goBuild)
	}
	byTier := map[string]*adminv1.TierStats{}
	for _, ts := range goBuild.Tiers {
		byTier[ts.Tier] = ts
	}
	if d := byTier["disk"]; d == nil || d.Gets != 2 || d.Hits != 1 || d.Misses != 1 || d.BytesRead != 512 {
		t.Errorf("go/build disk = %+v, want gets 2, hits 1, misses 1, bytesRead 512", d)
	}
	// The failed put lands in Errors, which is the point of folding write
	// failures in: a bucket that cannot be written is not healthy.
	if b := byTier["bucket"]; b == nil || b.Puts != 1 || b.Errors != 1 || b.BytesWritten != 1024 {
		t.Errorf("go/build bucket = %+v, want puts 1, errors 1, bytesWritten 1024", b)
	}

	// "nix" has served nothing, so it has no tier counters and still has to
	// report its bytes.
	if nix := msg.Frontends[1]; len(nix.Tiers) != 0 || nix.Bytes != 20 {
		t.Errorf("nix = %+v, want no tiers and 20 bytes", nix)
	}
}

func TestStatsWithoutAQueue(t *testing.T) {
	t.Parallel()

	deps := defaultDeps(newFakeDisk(), nil)
	deps.Queue = nil
	client := serve(t, deps)

	resp, err := client.Stats(context.Background(), connect.NewRequest(&adminv1.StatsRequest{}))
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if resp.Msg.UploadQueueDepth != 0 {
		t.Errorf("uploadQueueDepth = %d, want 0 when there is no queue", resp.Msg.UploadQueueDepth)
	}
}

// TestListPagesStably walks the whole index a page at a time and checks that
// every key appears exactly once.
//
// A pager that loses or repeats an entry is the kind of bug that only shows
// on a volume larger than anyone's test, which is why the page size here is
// two and the index is seven.
func TestListPagesStably(t *testing.T) {
	t.Parallel()

	var keys []string
	for i := range 7 {
		keys = append(keys, fmt.Sprintf("go/build/%02d", i))
	}
	keys = append(keys, "nix/nar/zz")
	client := serve(t, defaultDeps(newFakeDisk(keys...), nil))

	var got []string
	token := ""
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("List did not terminate: the page token is not advancing")
		}
		resp, err := client.List(context.Background(), connect.NewRequest(&adminv1.ListRequest{
			Prefix:    "go/build/",
			PageSize:  2,
			PageToken: token,
		}))
		if err != nil {
			t.Fatalf("List page %d: %v", page, err)
		}
		for _, e := range resp.Msg.Entries {
			got = append(got, e.Key)
		}
		token = resp.Msg.NextPageToken
		if token == "" {
			break
		}
	}

	want := keys[:7]
	if len(got) != len(want) {
		t.Fatalf("listed %d keys, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestListClampsThePageSize(t *testing.T) {
	t.Parallel()

	var keys []string
	for i := range 3 {
		keys = append(keys, fmt.Sprintf("go/build/%02d", i))
	}
	client := serve(t, defaultDeps(newFakeDisk(keys...), nil))

	// Far above the cap. A refusal would only teach the caller to retry.
	resp, err := client.List(context.Background(), connect.NewRequest(&adminv1.ListRequest{PageSize: 1 << 20}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Msg.Entries) != 3 {
		t.Errorf("got %d entries, want 3", len(resp.Msg.Entries))
	}
	// The metadata has to survive the wire: an "oldest entries" view is
	// built on it.
	if got := resp.Msg.Entries[0].LastAccess.AsTime(); !got.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("lastAccess = %v, want 1700000000", got)
	}
	if resp.Msg.Entries[0].Size != 10 {
		t.Errorf("size = %d, want 10", resp.Msg.Entries[0].Size)
	}
}

func TestListReportsAnIndexFailure(t *testing.T) {
	t.Parallel()

	disk := newFakeDisk()
	disk.listErr = errors.New("index closed")
	client := serve(t, defaultDeps(disk, nil))

	_, err := client.List(context.Background(), connect.NewRequest(&adminv1.ListRequest{}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal (err %v)", connect.CodeOf(err), err)
	}
}

// TestWipeDiskPrefix is the rule the trailing slash encodes.
func TestWipeDiskPrefix(t *testing.T) {
	t.Parallel()

	disk := newFakeDisk("go/build/aa", "go/build/bb", "go/mod/cc", "nix/nar/dd")
	client := serve(t, defaultDeps(disk, nil))

	resp, err := client.WipeDisk(context.Background(),
		connect.NewRequest(&adminv1.WipeDiskRequest{PrefixOrKey: "go/build/"}))
	if err != nil {
		t.Fatalf("WipeDisk: %v", err)
	}
	if got := resp.Msg.Removed.Entries; got != 2 {
		t.Errorf("removed %d entries, want 2", got)
	}
	if got := resp.Msg.Removed.Bytes; got != 30 {
		t.Errorf("removed %d bytes, want 30", got)
	}
	// "go/mod/cc" must survive: the prefix was "go/build/", not "go/".
	want := []string{"go/mod/cc", "nix/nar/dd"}
	if got := disk.keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("left on disk = %v, want %v", got, want)
	}
}

// TestWipeDiskExactKey checks the other half of the same rule, and that a key
// which is already gone is not an error.
func TestWipeDiskExactKey(t *testing.T) {
	t.Parallel()

	disk := newFakeDisk("go/build/aa", "go/build/ab")
	client := serve(t, defaultDeps(disk, nil))
	ctx := context.Background()

	resp, err := client.WipeDisk(ctx, connect.NewRequest(&adminv1.WipeDiskRequest{PrefixOrKey: "go/build/aa"}))
	if err != nil {
		t.Fatalf("WipeDisk: %v", err)
	}
	if resp.Msg.Removed.Entries != 1 || resp.Msg.Removed.Bytes != 10 {
		t.Errorf("removed = %+v, want 1 entry of 10 bytes", resp.Msg.Removed)
	}
	// "go/build/ab" survives: an exact key is not a prefix, even when
	// another key starts with it.
	if got := disk.keys(); strings.Join(got, ",") != "go/build/ab" {
		t.Errorf("left on disk = %v, want [go/build/ab]", got)
	}

	// Again. A retried script and two operators on one incident both do this.
	resp, err = client.WipeDisk(ctx, connect.NewRequest(&adminv1.WipeDiskRequest{PrefixOrKey: "go/build/aa"}))
	if err != nil {
		t.Fatalf("second WipeDisk: %v", err)
	}
	if resp.Msg.Removed.Entries != 0 {
		t.Errorf("removed = %+v, want nothing the second time", resp.Msg.Removed)
	}
}

func TestWipeDiskRefusesAnEmptyArgument(t *testing.T) {
	t.Parallel()

	client := serve(t, defaultDeps(newFakeDisk("go/build/aa"), nil))
	_, err := client.WipeDisk(context.Background(),
		connect.NewRequest(&adminv1.WipeDiskRequest{PrefixOrKey: ""}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
}

// TestWipeBucketRefusesTheWholeBucket is the guard that matters most here.
//
// "" and "/" are what an unset variable and a hasty argument expand to, and
// what they would delete is not behind anything: every build in the estate
// pays to produce it again.
func TestWipeBucketRefusesTheWholeBucket(t *testing.T) {
	t.Parallel()

	cases := []string{"", "/", "go", "go/build", "/go/build/", "Go/build/", "../", "go/build/../../"}
	bucket := &fakeBucket{}
	client := serve(t, defaultDeps(newFakeDisk("go/build/aa"), bucket))

	for _, prefix := range cases {
		t.Run(fmt.Sprintf("%q", prefix), func(t *testing.T) {
			_, err := client.WipeBucket(context.Background(),
				connect.NewRequest(&adminv1.WipeBucketRequest{Prefix: prefix}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
			}
		})
	}
	if got := bucket.seen(); len(got) != 0 {
		t.Errorf("the store was asked to delete %v; nothing should have reached it", got)
	}
}

func TestWipeBucketAcceptsAKeyPrefix(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"go/", "go/build/", "maven/central/org/junit/", "nix/nar/", "go-build/x=1/"} {
		t.Run(prefix, func(t *testing.T) {
			bucket := &fakeBucket{}
			client := serve(t, defaultDeps(newFakeDisk(), bucket))
			_, err := client.WipeBucket(context.Background(),
				connect.NewRequest(&adminv1.WipeBucketRequest{Prefix: prefix}))
			if err != nil {
				t.Fatalf("WipeBucket(%q): %v", prefix, err)
			}
			if got := bucket.seen(); len(got) != 1 || got[0] != prefix {
				t.Errorf("store saw %v, want [%s]", got, prefix)
			}
		})
	}
}

// TestWipeBucketAlsoWipesTheDisk is the invariant the proto states: a disk
// copy of what the bucket no longer has would make the next read lie.
func TestWipeBucketAlsoWipesTheDisk(t *testing.T) {
	t.Parallel()

	disk := newFakeDisk("go/build/aa", "go/build/bb", "nix/nar/cc")
	bucket := &fakeBucket{}
	client := serve(t, defaultDeps(disk, bucket))

	resp, err := client.WipeBucket(context.Background(),
		connect.NewRequest(&adminv1.WipeBucketRequest{Prefix: "go/build/"}))
	if err != nil {
		t.Fatalf("WipeBucket: %v", err)
	}
	if resp.Msg.Removed.Entries != 7 || resp.Msg.Removed.Bytes != 700 {
		t.Errorf("bucket removed = %+v, want 7 entries of 700 bytes", resp.Msg.Removed)
	}
	if resp.Msg.RemovedFromDisk.Entries != 2 || resp.Msg.RemovedFromDisk.Bytes != 30 {
		t.Errorf("disk removed = %+v, want 2 entries of 30 bytes", resp.Msg.RemovedFromDisk)
	}
	if got := disk.keys(); strings.Join(got, ",") != "nix/nar/cc" {
		t.Errorf("left on disk = %v, want [nix/nar/cc]", got)
	}
}

// TestWipeBucketLeavesTheDiskAloneWhenTheStoreFails: the disk is the cheap
// copy, and throwing it away for a wipe that did not happen would cost the
// estate a re-download for nothing.
func TestWipeBucketLeavesTheDiskAloneWhenTheStoreFails(t *testing.T) {
	t.Parallel()

	disk := newFakeDisk("go/build/aa")
	client := serve(t, defaultDeps(disk, &fakeBucket{err: errors.New("access denied")}))

	_, err := client.WipeBucket(context.Background(),
		connect.NewRequest(&adminv1.WipeBucketRequest{Prefix: "go/build/"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal (err %v)", connect.CodeOf(err), err)
	}
	if got := disk.keys(); len(got) != 1 {
		t.Errorf("left on disk = %v, want the object untouched", got)
	}
}

// TestWipeBucketWithoutABucket is the agent's shape: disk and a remote tier,
// no store. Refusing says so instead of reporting a successful wipe of
// nothing.
func TestWipeBucketWithoutABucket(t *testing.T) {
	t.Parallel()

	client := serve(t, defaultDeps(newFakeDisk("go/build/aa"), nil))
	_, err := client.WipeBucket(context.Background(),
		connect.NewRequest(&adminv1.WipeBucketRequest{Prefix: "go/build/"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition (err %v)", connect.CodeOf(err), err)
	}
}

// TestInvalidateTouchesOnlyNegativeEntries is the whole reason the call is
// separate from WipeDisk.
func TestInvalidateTouchesOnlyNegativeEntries(t *testing.T) {
	t.Parallel()

	disk := newFakeDisk("maven/central/a.jar", "maven/central/b.jar")
	disk.negative["maven/central/missing.jar"] = true
	disk.negative["maven/central/gone.jar"] = true
	disk.negative["nix/nar/absent"] = true
	client := serve(t, defaultDeps(disk, nil))

	resp, err := client.Invalidate(context.Background(),
		connect.NewRequest(&adminv1.InvalidateRequest{Prefix: "maven/"}))
	if err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if resp.Msg.Removed.Entries != 2 {
		t.Errorf("removed %d negative entries, want 2", resp.Msg.Removed.Entries)
	}
	if got := len(disk.keys()); got != 2 {
		t.Errorf("%d cached objects left, want 2: Invalidate must not touch an object", got)
	}
	if !disk.negative["nix/nar/absent"] {
		t.Error("Invalidate removed a negative entry outside the prefix")
	}
}

// TestMutatingCallsAreLogged is the audit line the package promises.
//
// With no authentication, the log is the only record of who emptied the
// cache. A wipe that left no line would make "which pod did this" an
// unanswerable question during the incident it caused.
func TestMutatingCallsAreLogged(t *testing.T) {
	t.Parallel()

	var buf lockedBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	svc := admin.New(defaultDeps(newFakeDisk("go/build/aa"), &fakeBucket{}), admin.WithLogger(log))

	path, h := svc.Handler()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := adminv1connect.NewAdminServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	if _, err := client.WipeDisk(ctx,
		connect.NewRequest(&adminv1.WipeDiskRequest{PrefixOrKey: "go/build/"})); err != nil {
		t.Fatalf("WipeDisk: %v", err)
	}
	if _, err := client.WipeBucket(ctx,
		connect.NewRequest(&adminv1.WipeBucketRequest{Prefix: "go/build/"})); err != nil {
		t.Fatalf("WipeBucket: %v", err)
	}
	if _, err := client.Invalidate(ctx,
		connect.NewRequest(&adminv1.InvalidateRequest{Prefix: "go/"})); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	// Stats is not mutating and must not add to the audit trail.
	if _, err := client.Stats(ctx, connect.NewRequest(&adminv1.StatsRequest{})); err != nil {
		t.Fatalf("Stats: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"wiped disk", "wiped bucket", "invalidated negative entries"} {
		if !strings.Contains(out, want) {
			t.Errorf("no log line for %q:\n%s", want, out)
		}
	}
	// The caller's address, on every one of them.
	if n := strings.Count(out, "caller=127.0.0.1:"); n != 3 {
		t.Errorf("logged a caller address %d times, want 3:\n%s", n, out)
	}
	if strings.Count(out, "\n") != 3 {
		t.Errorf("want exactly three log lines (Stats must not log):\n%s", out)
	}
}

// TestListenAndServeAndShutdown checks the listener's own lifecycle.
func TestListenAndServeAndShutdown(t *testing.T) {
	t.Parallel()

	svc := admin.New(defaultDeps(newFakeDisk(), nil), admin.WithLogger(discardLogger()))
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- svc.ListenAndServe(ctx, "127.0.0.1:0") }()

	// Cancelling the context is the ordinary stop, and it must not surface
	// http.ErrServerClosed: callers treat a non-nil return as fatal and
	// would log a crash on every clean shutdown.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("ListenAndServe returned %v on an orderly stop, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after its context was cancelled")
	}

	// And again, from a process that is running its deferred stops.
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

func TestShutdownBeforeServing(t *testing.T) {
	t.Parallel()

	svc := admin.New(defaultDeps(newFakeDisk(), nil), admin.WithLogger(discardLogger()))
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown before ListenAndServe: %v", err)
	}
}

// lockedBuffer is a strings.Builder a handler goroutine and the test can
// share. slog's handler writes from whichever goroutine served the request.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
