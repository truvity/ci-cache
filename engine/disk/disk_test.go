package disk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truvity/ci-cache/engine/tier"
)

// --- helpers ----------------------------------------------------------

// fakeClock makes recency deterministic. Negative-entry expiry and LRU order
// are both "did enough time pass", and a test that answers that with a sleep
// is a test that fails on a loaded CI runner.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newDisk opens a tier on a throwaway directory. fsync is off and the index
// flush is disabled by default: neither is what any of these tests are about,
// and a sync per object turns a ten-thousand-file case into a minute.
func newDisk(t *testing.T, opts ...Option) *Disk {
	t.Helper()
	return newDiskAt(t, t.TempDir(), opts...)
}

func newDiskAt(t *testing.T, dir string, opts ...Option) *Disk {
	t.Helper()
	all := append([]Option{WithFsync(false), WithIndexFlush(0)}, opts...)
	d, err := New(dir, all...)
	if err != nil {
		t.Fatalf("New(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	waitWarm(t, d)
	return d
}

func waitWarm(t *testing.T, d *Disk) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for d.IndexCold() {
		if time.Now().After(deadline) {
			t.Fatal("index never finished its walk")
		}
		time.Sleep(time.Millisecond)
	}
}

func mustPut(t *testing.T, d *Disk, key string, body []byte, m tier.Meta) {
	t.Helper()
	if err := d.Put(context.Background(), key, bytes.NewReader(body), m); err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
}

func mustGet(t *testing.T, d *Disk, key string) ([]byte, tier.Meta) {
	t.Helper()
	rc, m, err := d.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return b, m
}

func present(t *testing.T, d *Disk, key string) bool {
	t.Helper()
	_, err := d.Stat(context.Background(), key)
	switch {
	case err == nil:
		return true
	case errors.Is(err, tier.ErrNotFound):
		return false
	default:
		t.Fatalf("Stat(%s): %v", key, err)
		return false
	}
}

// countFiles reports the object files and the leftover temp files under dir.
func countFiles(t *testing.T, dir string) (objects, temps int) {
	t.Helper()
	err := filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		switch {
		case strings.HasPrefix(e.Name(), ".tmp-"):
			temps++
		case strings.HasPrefix(e.Name(), "."):
			// .index and friends are not objects.
		default:
			objects++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return objects, temps
}

// --- the contract -----------------------------------------------------

func TestPutGetRoundTrip(t *testing.T) {
	t.Parallel()
	mod := time.Date(2026, 9, 23, 11, 22, 33, 123456789, time.UTC)

	cases := []struct {
		name     string
		key      string
		body     []byte
		meta     tier.Meta
		wantSize int64
	}{
		{"empty body", "go/build/action/aa/empty", nil, tier.Meta{ModTime: mod}, 0},
		{"plain object", "go/build/output/ab/cdef", []byte("hello"), tier.Meta{Size: 5, ModTime: mod}, 5},
		{
			name:     "content type survives",
			key:      "maven/central/org/example/thing-1.0.jar",
			body:     bytes.Repeat([]byte{0x7f}, 4096),
			meta:     tier.Meta{Size: 4096, ModTime: mod, ContentType: "application/java-archive"},
			wantSize: 4096,
		},
		{"immutable flag survives", "nix/nar/abc123", []byte("nar"), tier.Meta{Size: 3, ModTime: mod, Immutable: true}, 3},
		{"zero modtime survives", "npm/registry/pkg/-/pkg-1.0.0.tgz", []byte("tgz"), tier.Meta{Size: 3}, 3},
		{"size unknown at Put", "gradle/build/abcdef", []byte("streamed"), tier.Meta{ModTime: mod}, 8},
		{"key with no front-end segment", "orphan", []byte("x"), tier.Meta{Size: 1, ModTime: mod}, 1},
		{"key whose segment is not a safe path", "../escape/key", []byte("y"), tier.Meta{Size: 1, ModTime: mod}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDisk(t)
			mustPut(t, d, tc.key, tc.body, tc.meta)

			body, got := mustGet(t, d, tc.key)
			if !bytes.Equal(body, tc.body) {
				t.Fatalf("body: got %q, want %q", body, tc.body)
			}
			if got.Size != tc.wantSize {
				t.Errorf("Size: got %d, want %d", got.Size, tc.wantSize)
			}
			// Equal, not ==: a time.Time loses its monotonic reading and its
			// location to any serialisation, and neither is part of the
			// instant the Go toolchain compares.
			if !got.ModTime.Equal(tc.meta.ModTime) {
				t.Errorf("ModTime: got %v, want %v", got.ModTime, tc.meta.ModTime)
			}
			if got.ContentType != tc.meta.ContentType {
				t.Errorf("ContentType: got %q, want %q", got.ContentType, tc.meta.ContentType)
			}
			if got.Immutable != tc.meta.Immutable {
				t.Errorf("Immutable: got %v, want %v", got.Immutable, tc.meta.Immutable)
			}

			statMeta, err := d.Stat(context.Background(), tc.key)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if statMeta != got {
				t.Errorf("Stat disagrees with Get: %+v vs %+v", statMeta, got)
			}
		})
	}
}

func TestGetMissIsErrNotFound(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	if _, _, err := d.Get(context.Background(), "go/build/nothing"); !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("Get miss: got %v, want ErrNotFound", err)
	}
	if _, err := d.Stat(context.Background(), "go/build/nothing"); !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("Stat miss: got %v, want ErrNotFound", err)
	}
}

// refusingReader fails the test if anything reads it. It is how "without
// reading the body" is checked rather than asserted.
type refusingReader struct{ read bool }

func (r *refusingReader) Read([]byte) (int, error) {
	r.read = true
	return 0, io.EOF
}

func TestImmutablePutOfExistingKeyDoesNotReadTheBody(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	const key = "nix/nar/immutable"
	mustPut(t, d, key, []byte("original"), tier.Meta{Size: 8, Immutable: true})

	r := &refusingReader{}
	err := d.Put(context.Background(), key, r, tier.Meta{Size: 8, Immutable: true})
	if !errors.Is(err, tier.ErrExists) {
		t.Fatalf("second immutable Put: got %v, want ErrExists", err)
	}
	if r.read {
		t.Error("the body was read for a key that already existed")
	}
	if body, _ := mustGet(t, d, key); string(body) != "original" {
		t.Errorf("object changed: got %q", body)
	}
}

func TestMutablePutOverwrites(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	const key = "maven/central/metadata.xml"
	mustPut(t, d, key, []byte("first"), tier.Meta{Size: 5})
	mustPut(t, d, key, []byte("second-and-longer"), tier.Meta{Size: 17})

	body, m := mustGet(t, d, key)
	if string(body) != "second-and-longer" {
		t.Fatalf("got %q", body)
	}
	if m.Size != 17 {
		t.Fatalf("Size: got %d, want 17", m.Size)
	}
	if got, want := d.Entries(), int64(1); got != want {
		t.Errorf("Entries: got %d, want %d", got, want)
	}
	if got, want := d.Used(), int64(headerSize+17); got != want {
		t.Errorf("Used: got %d, want %d -- the overwritten object was double counted", got, want)
	}
}

func TestDeleteOfAbsentKeyIsNil(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	if err := d.Delete(context.Background(), "go/build/never-existed"); err != nil {
		t.Fatalf("Delete of an absent key: got %v, want nil", err)
	}
	// Twice, because the case this protects is a wipe that runs again.
	mustPut(t, d, "go/build/x", []byte("x"), tier.Meta{Size: 1})
	for i := range 2 {
		if err := d.Delete(context.Background(), "go/build/x"); err != nil {
			t.Fatalf("Delete #%d: %v", i, err)
		}
	}
	if d.Used() != 0 || d.Entries() != 0 {
		t.Errorf("after delete: Used=%d Entries=%d, want 0/0", d.Used(), d.Entries())
	}
}

// --- concurrency ------------------------------------------------------

func TestConcurrentPutOfOneKeyLeavesOneFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := newDiskAt(t, dir)
	const (
		key     = "go/build/action/cc/contended"
		writers = 32
		size    = 4096
	)

	bodies := make([][]byte, writers)
	for i := range bodies {
		bodies[i] = bytes.Repeat([]byte{byte('a' + i)}, size)
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = d.Put(context.Background(), key, bytes.NewReader(bodies[i]), tier.Meta{Size: size})
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	objects, temps := countFiles(t, dir)
	if objects != 1 {
		t.Errorf("object files: got %d, want 1", objects)
	}
	if temps != 0 {
		t.Errorf("temp files left behind: %d -- a byte nothing will ever reclaim", temps)
	}

	body, m := mustGet(t, d, key)
	if m.Size != size || len(body) != size {
		t.Fatalf("size: meta %d, body %d, want %d", m.Size, len(body), size)
	}
	// The winner is whichever renamed last, but it must be ONE writer's
	// bytes from end to end and never a splice of two.
	whole := bytes.Repeat(body[:1], size)
	if !bytes.Equal(body, whole) {
		t.Error("the stored object is a mix of two writers' bodies")
	}
	if got, want := d.Entries(), int64(1); got != want {
		t.Errorf("Entries: got %d, want %d", got, want)
	}
	if got, want := d.Used(), int64(headerSize+size); got != want {
		t.Errorf("Used: got %d, want %d", got, want)
	}
}

func TestGetInProgressSurvivesDelete(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	const (
		key  = "gradle/build/long-entry"
		size = 1 << 20
	)
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i)
	}
	mustPut(t, d, key, body, tier.Meta{Size: size})

	rc, m, err := d.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if m.Size != size {
		t.Fatalf("Size: got %d, want %d", m.Size, size)
	}

	head := make([]byte, 128)
	if _, err := io.ReadFull(rc, head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	// The name goes now. The reader holds a descriptor, and POSIX keeps the
	// bytes alive until it closes -- so the read below must still reach the
	// end of the object it started on.
	if err := d.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if present(t, d, key) {
		t.Fatal("the key is still visible after Delete")
	}

	rest, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read the rest after Delete: %v", err)
	}
	got := make([]byte, 0, len(body))
	got = append(got, head...)
	got = append(got, rest...)
	if !bytes.Equal(got, body) {
		t.Fatalf("read %d bytes of %d, and they do not match", len(got), size)
	}
}

// --- crash safety -----------------------------------------------------

func TestCrashBetweenWriteAndRenameIsInvisible(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const key = "go/build/action/dd/half-written"

	// Build what a crashed Put leaves: a complete, valid object file that
	// never got its final name.
	func() {
		d := newDiskAt(t, dir)
		objDir, _ := d.dirAndPathFor(key)
		if err := os.MkdirAll(objDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		hdr, err := encodeHeader(header{Key: key, Size: 5})
		if err != nil {
			t.Fatalf("encodeHeader: %v", err)
		}
		if err := os.WriteFile(filepath.Join(objDir, ".tmp-crashed"), append(hdr, []byte("hello")...), 0o644); err != nil {
			t.Fatalf("plant temp file: %v", err)
		}

		if _, _, err := d.Get(context.Background(), key); !errors.Is(err, tier.ErrNotFound) {
			t.Fatalf("Get of a never-renamed object: got %v, want ErrNotFound", err)
		}
	}()

	// And a restart must not adopt it either: the start-up walk skips
	// dotfiles, so the half-written object stays invisible rather than being
	// indexed and then served.
	d2 := newDiskAt(t, dir)
	if _, _, err := d2.Get(context.Background(), key); !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("after restart: got %v, want ErrNotFound", err)
	}
	if d2.Entries() != 0 || d2.Used() != 0 {
		t.Errorf("the walk indexed a temp file: Entries=%d Used=%d", d2.Entries(), d2.Used())
	}
}

func TestCorruptObjectIsRemovedRatherThanServed(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	const key = "go/build/action/ee/rotten"
	mustPut(t, d, key, []byte("good"), tier.Meta{Size: 4})

	path := d.pathFor(key)
	if err := os.WriteFile(path, []byte("this is not a header"), 0o644); err != nil {
		t.Fatalf("corrupt the file: %v", err)
	}
	if _, _, err := d.Get(context.Background(), key); !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("Get of a corrupt object: got %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the corrupt file was left on the volume, where nothing can ever reclaim it")
	}
	if d.Entries() != 0 {
		t.Errorf("Entries: got %d, want 0", d.Entries())
	}
}

func TestObjectUnderAnotherKeysNameIsRefused(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	const key = "go/build/action/ff/mine"
	mustPut(t, d, key, []byte("mine"), tier.Meta{Size: 4})

	// Write another key's header into this key's file, which is what a hash
	// collision or a hand-edited volume would look like.
	hdr, err := encodeHeader(header{Key: "go/build/action/ff/someone-else", Size: 4})
	if err != nil {
		t.Fatalf("encodeHeader: %v", err)
	}
	if err := os.WriteFile(d.pathFor(key), append(hdr, []byte("else")...), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, _, err := d.Get(context.Background(), key); !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// --- listing ----------------------------------------------------------

func TestListPagesStablyByKey(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	want := make([]string, 0, 25)
	for i := range 25 {
		key := fmt.Sprintf("go/build/action/%02d", i)
		mustPut(t, d, key, []byte{byte(i)}, tier.Meta{Size: 1})
		want = append(want, key)
	}
	// Another front-end's keys must not leak into a prefixed page.
	for i := range 5 {
		mustPut(t, d, fmt.Sprintf("nix/nar/%02d", i), []byte{byte(i)}, tier.Meta{Size: 1})
	}

	var (
		got   []string
		token string
		pages int
	)
	for {
		entries, next, err := d.List(context.Background(), "go/build/", token, 7)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		if pages > 10 {
			t.Fatal("List never finished: the page token is not advancing")
		}
		for _, e := range entries {
			got = append(got, e.Key)
			if e.Size != 1 {
				t.Errorf("%s: Size %d, want 1", e.Key, e.Size)
			}
		}
		if next == "" {
			break
		}
		token = next
	}

	if len(got) != len(want) {
		t.Fatalf("listed %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("page order broke at %d: got %s, want %s", i, got[i], want[i])
		}
	}
}

func TestDeletePrefixReportsWhatItRemoved(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	for i := range 10 {
		mustPut(t, d, fmt.Sprintf("go/build/%02d", i), bytes.Repeat([]byte{1}, 100), tier.Meta{Size: 100})
	}
	for i := range 4 {
		mustPut(t, d, fmt.Sprintf("nix/nar/%02d", i), bytes.Repeat([]byte{2}, 100), tier.Meta{Size: 100})
	}

	entries, bytesRemoved, err := d.DeletePrefix(context.Background(), "go/")
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if entries != 10 {
		t.Errorf("entries: got %d, want 10", entries)
	}
	if want := int64(10 * (headerSize + 100)); bytesRemoved != want {
		t.Errorf("bytes: got %d, want %d", bytesRemoved, want)
	}
	if got, want := d.Entries(), int64(4); got != want {
		t.Errorf("Entries after the wipe: got %d, want %d", got, want)
	}
	if present(t, d, "go/build/00") {
		t.Error("a wiped key is still readable")
	}
	if !present(t, d, "nix/nar/00") {
		t.Error("the wipe took another front-end's objects with it")
	}
}

// --- the index --------------------------------------------------------

func TestIndexRebuildMatchesPersistedIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const objects = 10000

	// A long flush period, so that the only index written is the one Close
	// writes: a partial flush mid-run would make this test measure the
	// flusher's timing rather than the index.
	d, err := New(dir, WithFsync(false), WithIndexFlush(time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	waitWarm(t, d)
	body := bytes.Repeat([]byte{0xab}, 64)
	for i := range objects {
		mustPut(t, d, fmt.Sprintf("go/build/action/%05d", i), body, tier.Meta{Size: int64(len(body))})
	}
	wantEntries, wantUsed := d.Entries(), d.Used()
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A clean restart: the index is newer than every directory in the tree,
	// so there is nothing to walk and the tier is warm before New returns.
	loaded, err := New(dir, WithFsync(false), WithIndexFlush(time.Hour))
	if err != nil {
		t.Fatalf("New (loaded): %v", err)
	}
	defer func() { _ = loaded.Close() }()
	if loaded.IndexCold() {
		t.Error("a clean restart still walked the tree: the persisted index bought nothing")
	}
	gotEntries, gotUsed := loaded.Entries(), loaded.Used()
	if gotEntries != wantEntries || gotUsed != wantUsed {
		t.Fatalf("loaded index: %d entries / %d bytes, want %d / %d", gotEntries, gotUsed, wantEntries, wantUsed)
	}
	if !present(t, loaded, "go/build/action/00042") {
		t.Error("an object in the loaded index cannot be read")
	}

	// A dirty restart: no index, so the tree is walked. It must arrive at
	// exactly the same totals, or the two paths disagree about the volume
	// and the GC's behaviour depends on how the process last died.
	if err := os.Remove(filepath.Join(dir, indexName)); err != nil {
		t.Fatalf("remove index: %v", err)
	}
	walked := newDiskAt(t, dir, WithIndexFlush(time.Hour))
	if got, used := walked.Entries(), walked.Used(); got != wantEntries || used != wantUsed {
		t.Fatalf("walked index: %d entries / %d bytes, want %d / %d", got, used, wantEntries, wantUsed)
	}
	if !present(t, walked, "go/build/action/09999") {
		t.Error("an object the walk indexed cannot be read")
	}
}

func TestStaleIndexIsIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	d := newDiskAt(t, dir, WithIndexFlush(time.Hour))
	mustPut(t, d, "go/build/one", []byte("one"), tier.Meta{Size: 3})
	if err := d.idx.save(dir, filepath.Join(dir, indexName)); err != nil {
		t.Fatalf("save: %v", err)
	}
	// An object arrives after the index was written -- which is what every
	// crash looks like. The directory mtime moves, so the index must be
	// rejected and the tree walked, or the new object is invisible to the GC
	// forever.
	mustPut(t, d, "go/build/two", []byte("two"), tier.Meta{Size: 3})

	restarted := newDiskAt(t, dir, WithIndexFlush(time.Hour))
	if got, want := restarted.Entries(), int64(2); got != want {
		t.Fatalf("Entries: got %d, want %d -- the stale index was believed", got, want)
	}
}

func TestEvictTakesLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	d := newDisk(t, withClock(clk.Now))

	const n = 10
	body := bytes.Repeat([]byte{1}, 1024)
	unit := int64(headerSize + len(body))
	for i := range n {
		mustPut(t, d, fmt.Sprintf("go/build/%02d", i), body, tier.Meta{Size: int64(len(body))})
		clk.advance(time.Second)
	}
	// The three OLDEST objects are read, which under LRU makes them the
	// three safest and under FIFO the three next to go.
	for i := range 3 {
		mustGet(t, d, fmt.Sprintf("go/build/%02d", i))
		clk.advance(time.Second)
	}

	if freed, want := d.Evict(5*unit), 5*unit; freed != want {
		t.Fatalf("Evict freed %d, want %d", freed, want)
	}
	for i := range n {
		key := fmt.Sprintf("go/build/%02d", i)
		wantPresent := i < 3 || i >= 8
		if got := present(t, d, key); got != wantPresent {
			t.Errorf("%s: present=%v, want %v -- eviction is not LRU", key, got, wantPresent)
		}
	}
	if got, want := d.Used(), 5*unit; got != want {
		t.Errorf("Used after eviction: got %d, want %d", got, want)
	}
}

func TestFrontendAccountingAndEviction(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	body := bytes.Repeat([]byte{1}, 512)
	unit := int64(headerSize + len(body))
	for i := range 6 {
		mustPut(t, d, fmt.Sprintf("go/build/%02d", i), body, tier.Meta{Size: int64(len(body))})
	}
	for i := range 4 {
		mustPut(t, d, fmt.Sprintf("nix/nar/%02d", i), body, tier.Meta{Size: int64(len(body))})
	}

	if e, b := d.UsedBy("go"); e != 6 || b != 6*unit {
		t.Errorf("UsedBy(go): got %d/%d, want 6/%d", e, b, 6*unit)
	}
	if e, b := d.UsedBy("nix"); e != 4 || b != 4*unit {
		t.Errorf("UsedBy(nix): got %d/%d, want 4/%d", e, b, 4*unit)
	}
	if e, b := d.UsedBy("maven"); e != 0 || b != 0 {
		t.Errorf("UsedBy of a front-end with nothing on the volume: got %d/%d, want 0/0", e, b)
	}

	if freed, want := d.EvictFrontend("go", 3*unit), 3*unit; freed != want {
		t.Fatalf("EvictFrontend freed %d, want %d", freed, want)
	}
	if e, _ := d.UsedBy("go"); e != 3 {
		t.Errorf("go entries after eviction: got %d, want 3", e)
	}
	if e, _ := d.UsedBy("nix"); e != 4 {
		t.Errorf("EvictFrontend took another front-end's objects: nix has %d, want 4", e)
	}
}

// --- negative entries -------------------------------------------------

func TestNegativeEntries(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	d := newDisk(t, withClock(clk.Now))

	d.PutNegative("maven/central/org/a/a-1.0-sources.jar", time.Minute)
	d.PutNegative("maven/central/org/b/b-1.0-sources.jar", time.Minute)
	d.PutNegative("nix/nar/absent", time.Minute)

	if !d.GetNegative("maven/central/org/a/a-1.0-sources.jar") {
		t.Fatal("a negative entry was not remembered")
	}
	if d.GetNegative("maven/central/org/c/c-1.0-sources.jar") {
		t.Error("a key nobody recorded came back negative")
	}
	// The whole point of keeping them out of the byte accounting: a GC that
	// counted them would evict real objects to make room for the memory of
	// missing ones.
	if d.Used() != 0 || d.Entries() != 0 {
		t.Errorf("negative entries reached the byte totals: Used=%d Entries=%d", d.Used(), d.Entries())
	}
	if got, want := d.Negatives(), int64(3); got != want {
		t.Fatalf("Negatives: got %d, want %d", got, want)
	}

	if got, want := d.InvalidatePrefix("maven/"), int64(2); got != want {
		t.Errorf("InvalidatePrefix: got %d, want %d", got, want)
	}
	if d.GetNegative("maven/central/org/a/a-1.0-sources.jar") {
		t.Error("an invalidated negative entry is still answering")
	}
	if !d.GetNegative("nix/nar/absent") {
		t.Error("InvalidatePrefix took a negative entry from outside the prefix")
	}

	clk.advance(2 * time.Minute)
	if d.GetNegative("nix/nar/absent") {
		t.Error("an expired negative entry is still answering")
	}
}

func TestExpireNegativesSweepsWhatNobodyAsksAbout(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	d := newDisk(t, withClock(clk.Now))

	for i := range 50 {
		d.PutNegative(fmt.Sprintf("npm/registry/absent-%02d", i), time.Minute)
	}
	d.PutNegative("npm/registry/long-lived", time.Hour)

	if got := d.ExpireNegatives(); got != 0 {
		t.Fatalf("ExpireNegatives before anything expired: got %d, want 0", got)
	}
	clk.advance(2 * time.Minute)
	if got, want := d.ExpireNegatives(), int64(50); got != want {
		t.Fatalf("ExpireNegatives: got %d, want %d", got, want)
	}
	if got, want := d.Negatives(), int64(1); got != want {
		t.Fatalf("Negatives: got %d, want %d -- a live entry was swept", got, want)
	}
}

func TestPutClearsANegativeEntryForTheSameKey(t *testing.T) {
	t.Parallel()
	d := newDisk(t)
	const key = "maven/central/org/a/a-1.0.jar"
	d.PutNegative(key, time.Hour)
	mustPut(t, d, key, []byte("now it exists"), tier.Meta{Size: 13})
	if d.GetNegative(key) {
		t.Error("the object arrived and the cache still says it is missing")
	}
}

// --- a property, against a model --------------------------------------

// TestRandomOperationsMatchAModel drives the tier with a random sequence and
// checks it against a plain map.
//
// pgregory.net/rapid would express this better, but it is not in this
// module's go.sum and adding a dependency for a test is not this change's to
// make. A seeded rand does the same job: the failure it is looking for is an
// accounting drift that only shows up after a few hundred mixed operations,
// which no hand-written sequence finds.
func TestRandomOperationsMatchAModel(t *testing.T) {
	t.Parallel()
	const seed = 20260923
	rng := rand.New(rand.NewSource(seed))
	d := newDisk(t)

	type want struct {
		body   []byte
		onDisk int64
	}
	model := map[string]want{}
	frontends := []string{"go", "nix", "maven"}

	for step := range 2000 {
		key := fmt.Sprintf("%s/obj/%03d", frontends[rng.Intn(len(frontends))], rng.Intn(120))
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4, 5:
			body := make([]byte, rng.Intn(300))
			rng.Read(body)
			mustPut(t, d, key, body, tier.Meta{Size: int64(len(body))})
			model[key] = want{body: body, onDisk: headerSize + int64(len(body))}
		case 6, 7, 8:
			rc, _, err := d.Get(context.Background(), key)
			exp, ok := model[key]
			if !ok {
				if !errors.Is(err, tier.ErrNotFound) {
					t.Fatalf("step %d: Get(%s) of an absent key: %v", step, key, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("step %d: Get(%s): %v", step, key, err)
			}
			got, readErr := io.ReadAll(rc)
			_ = rc.Close()
			if readErr != nil {
				t.Fatalf("step %d: read %s: %v", step, key, readErr)
			}
			if !bytes.Equal(got, exp.body) {
				t.Fatalf("step %d: %s: got %d bytes, want %d", step, key, len(got), len(exp.body))
			}
		default:
			if err := d.Delete(context.Background(), key); err != nil {
				t.Fatalf("step %d: Delete(%s): %v", step, key, err)
			}
			delete(model, key)
		}
	}

	var wantUsed int64
	perFrontend := map[string]int64{}
	for k, v := range model {
		wantUsed += v.onDisk
		perFrontend[frontendDir(k)] += v.onDisk
	}
	if got := d.Used(); got != wantUsed {
		t.Errorf("Used: got %d, want %d", got, wantUsed)
	}
	if got, w := d.Entries(), int64(len(model)); got != w {
		t.Errorf("Entries: got %d, want %d", got, w)
	}
	for _, fe := range frontends {
		if _, got := d.UsedBy(fe); got != perFrontend[fe] {
			t.Errorf("UsedBy(%s): got %d, want %d", fe, got, perFrontend[fe])
		}
	}
	if objects, temps := countFiles(t, d.Dir()); temps != 0 || int64(objects) != int64(len(model)) {
		t.Errorf("on the volume: %d objects and %d temp files, want %d and 0", objects, temps, len(model))
	}
}

func TestFrontendDir(t *testing.T) {
	t.Parallel()
	cases := []struct{ key, want string }{
		{"go/build/action/ab/cd", "go"},
		{"nix/nar/abc", "nix"},
		{"orphan", "orphan"},
		{"", "_"},
		{"/leading-slash", "_"},
		{"../escape", "_"},
		{".hidden/x", "_"},
		{"has space/x", "_"},
		{strings.Repeat("x", 65) + "/y", "_"},
	}
	for _, tc := range cases {
		if got := frontendDir(tc.key); got != tc.want {
			t.Errorf("frontendDir(%q): got %q, want %q", tc.key, got, tc.want)
		}
	}
}
