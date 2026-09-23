// Package disk is the tier that lives on a volume.
//
// It is the tier every deployment has: a server's PVC, a runner's scratch
// space, a laptop's cache directory. Everything above it -- the chain, the
// front-ends -- is the same code whether the disk is backed by a bucket or by
// nothing, which is what lets one binary be three deployments.
//
// # Layout
//
//	<dir>/<frontend>/<xx>/<hash>
//
// where <frontend> is the key's first path segment ("go" for
// go/build/action/ab/xyz), <hash> is the hex sha256 of the WHOLE key, and
// <xx> is its first two characters. The hash keeps a key's arbitrary bytes
// out of the filesystem's namespace -- keys carry slashes, case that a
// case-insensitive volume would fold together, and lengths no path allows --
// while the two-character shard keeps any one directory to a few thousand
// entries even with millions of objects, which is where ext4's htree and a
// human's `ls` both stop coping. The front-end segment is not needed to find
// an object; it is there so that "how much of this volume is Go build cache"
// is a question the GC can answer, and so that wiping one front-end is a
// subtree rather than a scan.
//
// # Object format
//
// Every object file is a fixed 1KiB header -- one line of JSON, then padding
// -- followed by the body. The header carries the key and the tier.Meta the
// caller passed to Put, so a Get returns exactly what was stored and a
// restart can rebuild the key space from the volume alone. There is no
// sidecar file, because two files cannot be renamed into place atomically and
// a cache whose metadata can outlive its bytes is a cache that lies.
//
// ModTime survives as an instant to nanosecond precision. A time.Time's
// monotonic reading and its *Location do not, since neither can cross a
// serialisation boundary; compare with Equal, not ==.
//
// # Durability
//
// A Put writes to a temp file in the object's own directory, fsyncs it, and
// renames it over the final name. Rename within a directory is atomic on
// every filesystem this runs on, so a reader sees either the old object or
// the new one and never a partial one. The parent directory is NOT fsynced:
// after a power loss the rename itself may be lost, which costs a cache miss,
// and paying a directory fsync per object to avoid that would cost far more
// than the misses ever will.
package disk

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truvity/ci-cache/engine/tier"
)

const (
	// defaultIndexFlush is how often the index is written out. A minute of
	// lost recency information after a crash costs a slower start, nothing
	// else, and a shorter period would write a multi-megabyte file over a
	// volume that is meant to be carrying cache traffic.
	defaultIndexFlush = 60 * time.Second

	// indexName is the persisted index. It starts with a dot so that the
	// walk, which skips dotfiles, cannot mistake it for an object.
	indexName = ".index"

	// tempPattern is the prefix of an in-flight write. Also a dotfile, for
	// the same reason: a temp file left by a crash must be invisible to the
	// walk, or a half-written object would be indexed and then served.
	tempPattern = ".tmp-*"

	// defaultPageSize is what List uses when the caller does not say.
	defaultPageSize = 1000

	maxPageSize = 10000
)

// SpaceManager is what the disk needs from the garbage collector.
//
// It is an interface here, rather than the disk importing engine/gc, because
// the GC is constructed around the disk: gc.New takes a *Disk. Inverting it
// would be a cycle. It also means a test can run the tier with no GC at all,
// which is what the fake-free-space tests rely on.
type SpaceManager interface {
	// Admit is asked before a write whether the volume can take size more
	// bytes. It returns tier.ErrNoSpace when it cannot.
	Admit(size int64) error
	// Notify tells the collector that the volume grew. It must not block:
	// a Put waits for nothing.
	Notify()
}

// The three interfaces this tier satisfies, asserted here so that a change
// to a signature is a compile error in this file rather than a nil
// type-assertion in whatever wires the chain together.
var (
	_ tier.Tier    = (*Disk)(nil)
	_ tier.Lister  = (*Disk)(nil)
	_ tier.Deleter = (*Disk)(nil)
)

// Disk is a tier.Tier backed by a directory, and also a tier.Lister and a
// tier.Deleter.
type Disk struct {
	dir        string
	flushEvery time.Duration
	fsync      bool
	now        func() time.Time

	idx  *index
	cold atomic.Bool

	// sm is set after construction, because the GC is built around the disk
	// and cannot exist when the disk does.
	sm atomic.Pointer[SpaceManager]

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// Option configures a Disk.
type Option func(*Disk)

// WithIndexFlush sets how often the in-memory index is persisted. Zero turns
// persistence off, which makes every start a full walk.
func WithIndexFlush(d time.Duration) Option {
	return func(k *Disk) { k.flushEvery = d }
}

// WithFsync controls whether each object is fsynced before it is renamed
// into place.
//
// Leaving it on is the default and the right answer for a server. Turning it
// off is for a volume where the loss of a few objects to a power cut is
// cheaper than a sync per write -- a runner's scratch disk, or a test. The
// atomicity guarantee is unaffected either way: a reader never sees a partial
// object, only possibly an absent one.
func WithFsync(on bool) Option {
	return func(k *Disk) { k.fsync = on }
}

// WithSpaceManager wires the garbage collector in at construction, for a
// caller that has one already. Most callers use SetSpaceManager instead.
func WithSpaceManager(sm SpaceManager) Option {
	return func(k *Disk) { k.sm.Store(&sm) }
}

// withClock replaces the tier's idea of now. Tests use it; nothing else
// should, because a cache's recency ordering is only meaningful against a
// real clock.
func withClock(f func() time.Time) Option {
	return func(k *Disk) { k.now = f }
}

// New opens the tier at dir, creating the directory when it is absent.
//
// It returns as soon as the directory exists: the index is seeded in the
// background, and the tier serves throughout. On a large volume that walk is
// minutes of work, and a cache that refuses traffic while it counts itself is
// a cache that turns a restart into an outage.
func New(dir string, opts ...Option) (*Disk, error) {
	if dir == "" {
		return nil, errors.New("disk: dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("disk: resolve %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("disk: create %q: %w", abs, err)
	}

	d := &Disk{
		dir:        abs,
		flushEvery: defaultIndexFlush,
		fsync:      true,
		now:        time.Now,
		idx:        newIndex(),
		stop:       make(chan struct{}),
	}
	for _, o := range opts {
		o(d)
	}
	d.cold.Store(true)

	path := filepath.Join(abs, indexName)
	if indexIsFresh(abs, path) && d.idx.load(abs, path) == nil {
		// A clean shutdown left an index newer than every directory in the
		// tree, so there is nothing to discover: warm immediately.
		d.idx.finishWalk()
		d.cold.Store(false)
	} else {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.walk()
		}()
	}

	if d.flushEvery > 0 {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.flushLoop()
		}()
	}
	return d, nil
}

// Name implements tier.Tier.
func (d *Disk) Name() string { return "disk" }

// Dir is the directory the tier occupies. The GC needs it for statfs.
func (d *Disk) Dir() string { return d.dir }

// SetSpaceManager hands the tier its garbage collector. It is safe to call
// while the tier is serving: a Put either sees the collector or does not, and
// a Put that does not see it simply writes, which the collector cleans up on
// its next pass.
func (d *Disk) SetSpaceManager(sm SpaceManager) { d.sm.Store(&sm) }

func (d *Disk) spaceManager() SpaceManager {
	if p := d.sm.Load(); p != nil {
		return *p
	}
	return nil
}

// Close stops the background goroutines and persists the index.
//
// The final flush is what makes the NEXT start instant: it leaves an .index
// newer than every directory in the tree, which is the condition New checks.
func (d *Disk) Close() error {
	d.stopOnce.Do(func() { close(d.stop) })
	d.wg.Wait()
	if d.flushEvery <= 0 || d.cold.Load() {
		// An index that is still cold is incomplete. Writing it would leave
		// a file that the next start believes -- and the next start would
		// then run with a byte total missing everything the interrupted walk
		// never reached, which is a GC that never evicts.
		return nil
	}
	return d.idx.save(d.dir, filepath.Join(d.dir, indexName))
}

// IndexCold reports whether the start-up walk is still running.
//
// It matters to the admin API and to the GC: while it is true, Used() is an
// under-count of the volume, so a byte total shown to a human should say so
// and an eviction decision made from it is provisional.
func (d *Disk) IndexCold() bool { return d.cold.Load() }

// --- the tier ---------------------------------------------------------

// Get implements tier.Tier.
func (d *Disk) Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, tier.Meta{}, err
	}
	path := d.pathFor(key)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, tier.Meta{}, tier.ErrNotFound
		}
		return nil, tier.Meta{}, fmt.Errorf("disk: open %s: %w", key, err)
	}
	h, err := decodeHeader(f)
	if err != nil || h.Key != key {
		_ = f.Close()
		// Either the file is rot, or it is another key's object under this
		// name -- which only a hash collision or a hand-edited volume can
		// produce. Both are unservable, and both must leave: a file the
		// index will never hold is a byte the GC can never reclaim.
		d.discard(key, path)
		return nil, tier.Meta{}, tier.ErrNotFound
	}

	d.observe(key, h, d.now())

	// The descriptor stays open for the caller. A Delete or an eviction that
	// races this unlinks the name, and POSIX keeps the bytes alive until the
	// last descriptor closes -- so a read in progress always reaches the end
	// of the object it started on. That is also why eviction frees space
	// lazily under load, which is fine: the GC's next pass sees the space
	// come back.
	return &objectFile{Reader: io.LimitReader(f, h.Size), f: f}, metaOf(h), nil
}

// Put implements tier.Tier.
func (d *Disk) Put(ctx context.Context, key string, r io.Reader, m tier.Meta) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("disk: empty key")
	}
	dir, path := d.dirAndPathFor(key)

	if m.Immutable {
		// The contract is that an immutable key is written once, and that a
		// second writer learns so WITHOUT its body being read. A caller that
		// races to upload the same content-addressed object therefore pays
		// one stat, not one upload.
		if _, err := os.Stat(path); err == nil {
			d.idx.touch(key, d.now())
			return tier.ErrExists
		}
	}

	if sm := d.spaceManager(); sm != nil {
		// m.Size is a hint and may be zero for a streamed body. Admitting
		// the hint is still worth doing: it is what turns "this 4GB object
		// can never fit" into an immediate refusal rather than a volume
		// filled and then emptied again.
		if err := sm.Admit(headerSize + m.Size); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("disk: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return fmt.Errorf("disk: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Every failure below has to take the temp file with it. A temp file
	// that outlives its write is invisible to Get and to the index, so it is
	// a byte nothing will ever reclaim.
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	placeholder, err := encodeHeader(header{Key: key})
	if err != nil {
		return err
	}
	if _, err := tmp.Write(placeholder); err != nil {
		return fmt.Errorf("disk: write %s: %w", key, err)
	}
	n, err := io.Copy(tmp, r)
	if err != nil {
		return fmt.Errorf("disk: write %s: %w", key, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Now that the length is known, the real header goes back over the
	// placeholder. This is the whole reason the header is fixed-width: a
	// streamed body of unknown size can still be stored in one file without
	// buffering it anywhere.
	h := header{Key: key, Size: n, ModTime: m.ModTime, ContentType: m.ContentType, Immutable: m.Immutable}
	final, err := encodeHeader(h)
	if err != nil {
		return err
	}
	if _, err := tmp.WriteAt(final, 0); err != nil {
		return fmt.Errorf("disk: write header for %s: %w", key, err)
	}
	if d.fsync {
		if err := tmp.Sync(); err != nil {
			return fmt.Errorf("disk: sync %s: %w", key, err)
		}
	}
	// 0600 from CreateTemp would hide the volume from anything running as
	// another user -- a debug shell, a sidecar that reports usage.
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("disk: chmod %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("disk: close %s: %w", key, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("disk: commit %s: %w", key, err)
	}
	committed = true

	d.idx.set(&entry{
		key:        key,
		frontend:   frontendDir(key),
		size:       n,
		onDisk:     headerSize + n,
		lastAccess: d.now(),
		immutable:  m.Immutable,
	})
	if sm := d.spaceManager(); sm != nil {
		// Tell the collector and carry on. A Put never waits for a GC pass:
		// making a build wait on somebody else's eviction is how a cache
		// becomes slower than no cache.
		sm.Notify()
	}
	return nil
}

// Stat implements tier.Tier.
func (d *Disk) Stat(ctx context.Context, key string) (tier.Meta, error) {
	if err := ctx.Err(); err != nil {
		return tier.Meta{}, err
	}
	path := d.pathFor(key)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tier.Meta{}, tier.ErrNotFound
		}
		return tier.Meta{}, fmt.Errorf("disk: open %s: %w", key, err)
	}
	h, err := decodeHeader(f)
	_ = f.Close()
	if err != nil || h.Key != key {
		d.discard(key, path)
		return tier.Meta{}, tier.ErrNotFound
	}
	// A Stat is a use. The go command stats an action before it fetches the
	// output, so a Stat that did not touch would let the GC evict exactly
	// the objects a build is about to ask for.
	d.observe(key, h, d.now())
	return metaOf(h), nil
}

// Delete implements tier.Tier. Deleting an absent key is not an error.
func (d *Disk) Delete(_ context.Context, key string) error {
	d.idx.remove(key)
	if err := os.Remove(d.pathFor(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("disk: remove %s: %w", key, err)
	}
	return nil
}

// --- listing and bulk deletion ----------------------------------------

// List implements tier.Lister.
//
// The page is sorted by key so that a client paging through a large cache
// sees each key once: an unsorted page over a map would reshuffle between
// calls and both repeat and skip entries. pageToken is the last key of the
// previous page.
func (d *Disk) List(ctx context.Context, prefix, pageToken string, pageSize int) ([]tier.Entry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	switch {
	case pageSize <= 0:
		pageSize = defaultPageSize
	case pageSize > maxPageSize:
		pageSize = maxPageSize
	}

	all := d.idx.snapshot()
	kept := all[:0]
	for _, e := range all {
		if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		if pageToken != "" && e.Key <= pageToken {
			continue
		}
		kept = append(kept, e)
	}
	slices.SortFunc(kept, func(a, b tier.Entry) int { return cmp.Compare(a.Key, b.Key) })

	if len(kept) > pageSize {
		page := kept[:pageSize]
		return page, page[len(page)-1].Key, nil
	}
	return kept, "", nil
}

// DeletePrefix implements tier.Deleter.
func (d *Disk) DeletePrefix(ctx context.Context, prefix string) (entries, bytes int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	for _, e := range d.idx.snapshot() {
		if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		removed := d.idx.remove(e.Key)
		if removed == nil {
			// Somebody else got there first between the snapshot and now.
			continue
		}
		if rmErr := os.Remove(d.pathFor(e.Key)); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("disk: remove %s: %w", e.Key, rmErr))
			continue
		}
		entries++
		bytes += removed.onDisk
	}
	return entries, bytes, err
}

// --- what the GC and the admin API need -------------------------------

// Used is the bytes the indexed objects occupy, headers included, because
// that is what the volume actually spends and what the budget is about.
//
// While IndexCold is true it under-reports: the walk has not counted
// everything yet.
func (d *Disk) Used() int64 { _, used := d.idx.stats(); return used }

// Entries is how many objects the index holds.
func (d *Disk) Entries() int64 { n, _ := d.idx.stats(); return n }

// UsedBy is one front-end's share of the volume.
func (d *Disk) UsedBy(frontend string) (entries, bytes int64) { return d.idx.usedBy(frontend) }

// Snapshot is every indexed object, for List and for the admin API.
func (d *Disk) Snapshot() []tier.Entry { return d.idx.snapshot() }

// Evict removes least-recently-used objects until at least n bytes have been
// reclaimed, and returns how many it accounted for.
//
// An object being read right now may be chosen. That is deliberate and safe:
// the reader holds an open descriptor, so unlinking the name leaves its bytes
// readable to the end (see Get). The space comes back when the last reader
// closes rather than immediately, so under heavy read load a pass can free
// less than the filesystem reports -- the GC's next pass picks up the rest.
func (d *Disk) Evict(n int64) (freed int64) { return d.evict(n, "") }

// EvictFrontend is Evict restricted to one front-end, for a per-front-end
// budget.
func (d *Disk) EvictFrontend(frontend string, n int64) (freed int64) { return d.evict(n, frontend) }

func (d *Disk) evict(n int64, frontend string) int64 {
	var freed int64
	for _, e := range d.idx.takeLRU(n, frontend) {
		// The entry has already left the index, so its bytes count as freed
		// whatever the unlink says. When the unlink genuinely fails -- a
		// read-only remount, say -- the file is left behind and forgotten
		// until the next start-up walk finds it again. Counting it as freed
		// here is what stops the GC from spinning on a file it cannot remove.
		if err := os.Remove(d.pathFor(e.key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = err
		}
		freed += e.onDisk
	}
	return freed
}

// --- negative entries -------------------------------------------------

// PutNegative records that the key is known to be absent for ttl.
//
// It exists because the expensive miss is not the one that reads the disk, it
// is the one that asks an upstream. A Maven client asking for the same absent
// -sources.jar on every module of a large build turns into one upstream
// request instead of hundreds.
//
// Negative entries live only in memory, cost no bytes on the volume, and are
// not persisted across a restart -- a restart is exactly when an upstream
// deserves to be asked again.
func (d *Disk) PutNegative(key string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	d.idx.putNegative(key, d.now().Add(ttl))
}

// GetNegative reports whether a live negative entry covers the key.
func (d *Disk) GetNegative(key string) bool { return d.idx.getNegative(key, d.now()) }

// InvalidatePrefix forgets the negative entries under a prefix and returns
// how many went.
//
// It is about negatives only: the positive objects under the prefix are still
// good, and throwing them away because an upstream gained one new artefact
// would be a wipe dressed up as an invalidation. Use DeletePrefix for that.
func (d *Disk) InvalidatePrefix(prefix string) (entries int64) { return d.idx.invalidatePrefix(prefix) }

// ExpireNegatives drops every negative entry whose time is up and returns how
// many went. The GC calls it on a ticker.
func (d *Disk) ExpireNegatives() (entries int64) { return d.idx.expireNegatives(d.now()) }

// Negatives is how many negative entries are held, for the admin API.
func (d *Disk) Negatives() int64 { return d.idx.negatives() }

// --- internals --------------------------------------------------------

// frontendDir is the directory a key's objects live under.
//
// The first path segment of the key names the front-end that produced it, but
// it is a caller's string and must never be trusted as a path component: a
// key beginning "../" or "." would escape the tree or collide with .index.
// Anything that is not a plain short token is mapped to "_", which costs
// nothing -- the front-end segment is bookkeeping, and the object is found by
// its hash.
func frontendDir(key string) string {
	seg := key
	if i := strings.IndexByte(key, '/'); i >= 0 {
		seg = key[:i]
	}
	if seg == "" || len(seg) > 64 {
		return "_"
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return "_"
		}
	}
	return seg
}

func (d *Disk) pathFor(key string) string {
	_, path := d.dirAndPathFor(key)
	return path
}

func (d *Disk) dirAndPathFor(key string) (dir, path string) {
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:])
	dir = filepath.Join(d.dir, frontendDir(key), name[:2])
	return dir, filepath.Join(dir, name)
}

// observe records a hit: a touch when the index knows the key, and an insert
// when it does not, which is the case for a hit on an object the start-up
// walk has not reached yet.
func (d *Disk) observe(key string, h header, at time.Time) {
	if d.idx.touch(key, at) {
		return
	}
	d.idx.addIfAbsent(&entry{
		key:        key,
		frontend:   frontendDir(key),
		size:       h.Size,
		onDisk:     headerSize + h.Size,
		lastAccess: at,
		immutable:  h.Immutable,
	})
}

// discard removes a file that cannot be served and forgets it.
func (d *Disk) discard(key, path string) {
	d.idx.remove(key)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = err
	}
}

func metaOf(h header) tier.Meta {
	return tier.Meta{Size: h.Size, ModTime: h.ModTime, ContentType: h.ContentType, Immutable: h.Immutable}
}

// objectFile is the body of an object plus the descriptor it is read from.
// The limit is what stops a reader from running off the end of one object
// into whatever the filesystem left after it.
type objectFile struct {
	io.Reader
	f *os.File
}

func (o *objectFile) Close() error { return o.f.Close() }

// walk seeds the index from the tree.
//
// lastAccess comes from mtime, NOT atime. atime is not trusted at all: a
// volume mounted noatime never updates it, and one mounted relatime -- the
// default nearly everywhere -- updates it at most once a day, so an object
// read a thousand times this morning and one read once last night are
// indistinguishable by it. mtime at least says when the object arrived, which
// makes the first eviction after a restart approximately-oldest-first rather
// than arbitrary; the ordering repairs itself as traffic touches things.
func (d *Disk) walk() {
	defer func() {
		d.idx.finishWalk()
		d.cold.Store(false)
	}()

	_ = filepath.WalkDir(d.dir, func(path string, e fs.DirEntry, err error) error {
		select {
		case <-d.stop:
			// A Close during the walk must not keep the process alive for
			// the rest of a multi-minute scan.
			return filepath.SkipAll
		default:
		}
		if err != nil {
			// A directory that cannot be read is a part of the cache that
			// will not be accounted for. Skipping beats aborting: the rest
			// of the volume is still worth indexing.
			return nil
		}
		name := e.Name()
		if path != d.dir && strings.HasPrefix(name, ".") {
			if e.IsDir() {
				return filepath.SkipDir
			}
			// .index, and the temp files a crashed write left behind.
			return nil
		}
		if e.IsDir() {
			return nil
		}
		// Only <frontend>/<xx>/<hash> is ours. Anything else on the volume
		// belongs to somebody -- a lost+found, an operator's notes -- and is
		// left exactly where it is.
		rel, relErr := filepath.Rel(d.dir, path)
		if relErr != nil || strings.Count(rel, string(filepath.Separator)) != 2 {
			return nil
		}

		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		h, hdrErr := decodeHeader(f)
		_ = f.Close()
		if hdrErr != nil {
			// Unreadable: remove it. Left in place it would be re-examined
			// and re-rejected on every restart while occupying bytes no
			// eviction can ever reclaim.
			_ = os.Remove(path)
			return nil
		}
		info, infoErr := e.Info()
		at := time.Time{}
		if infoErr == nil {
			at = info.ModTime()
		}
		d.idx.addIfAbsent(&entry{
			key:        h.Key,
			frontend:   frontendDir(h.Key),
			size:       h.Size,
			onDisk:     headerSize + h.Size,
			lastAccess: at,
			immutable:  h.Immutable,
		})
		return nil
	})
}

func (d *Disk) flushLoop() {
	t := time.NewTicker(d.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			if d.cold.Load() {
				// Persisting a half-built index would be worse than none:
				// the next start would see a fresh-looking file, trust it,
				// and run with a byte total that is missing everything the
				// walk had not reached.
				continue
			}
			_ = d.idx.save(d.dir, filepath.Join(d.dir, indexName))
		}
	}
}

// sortEntriesByAccess orders newest-first, with the key as a tie-break so
// that two objects seeded from the same mtime -- which a fast walk produces
// by the thousand -- land in a stable order rather than a map's.
func sortEntriesByAccess(all []*entry) {
	slices.SortFunc(all, func(a, b *entry) int {
		if a.lastAccess.Equal(b.lastAccess) {
			return cmp.Compare(a.key, b.key)
		}
		if a.lastAccess.After(b.lastAccess) {
			return -1
		}
		return 1
	})
}
