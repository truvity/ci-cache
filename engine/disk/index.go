package disk

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/truvity/ci-cache/engine/tier"
)

// indexVersion is bumped whenever the persisted record changes shape. A
// version that is not this one is not read: falling back to the walk costs a
// slow start, and guessing at an old layout costs wrong byte totals, which
// the GC would then act on.
const indexVersion = 1

// errStaleIndex says the persisted index cannot be trusted and the tree must
// be walked instead.
var errStaleIndex = errors.New("disk: persisted index is stale")

// entry is one object as the index knows it, and one node of the LRU list.
//
// The list is intrusive -- prev and next live in the entry itself -- so that
// touching an object on a Get is a few pointer writes with no allocation.
// A cache serves far more hits than misses, so the hit path is the one that
// has to be free.
type entry struct {
	key        string
	frontend   string
	size       int64 // the object's own bytes, which is what a caller sees
	onDisk     int64 // headerSize + size, which is what the volume spends
	lastAccess time.Time
	immutable  bool

	prev, next *entry
}

// frontendUse is one front-end's share of the volume, kept incrementally
// because the GC asks for it on every pass and walking the whole map each
// time would make the GC's cost grow with the cache's size.
type frontendUse struct {
	entries int64
	bytes   int64
}

// index is the in-memory map of the volume plus its LRU ordering.
//
// It is the heart of the tier: the files on disk are the truth about bytes,
// but nothing on disk is the truth about USE. atime is not trusted -- volumes
// are mounted relatime or noatime, and under relatime a file read a thousand
// times in an hour has an atime that never moved -- so recency is something
// this process observes and remembers, and rebuilds from mtime when it has
// forgotten.
type index struct {
	mu   sync.Mutex
	m    map[string]*entry
	head *entry // most recently used
	tail *entry // least recently used
	used int64
	n    int64
	fe   map[string]*frontendUse

	// cold is true while the start-up walk is still running.
	cold bool
	// tombs remembers keys deleted while cold. The walk reads a directory
	// snapshot that may be minutes old by the time it gets there, so without
	// this a Delete racing the walk is undone by it: the walk would re-add an
	// entry for a file that is already gone, and the index would carry a
	// phantom that never leaves.
	tombs map[string]struct{}

	// neg holds negative entries: keys an upstream has said it does not
	// have, mapped to when that answer expires. They are deliberately a
	// separate map. They have no file, so they must never reach the LRU list
	// or the byte totals -- a GC that counted them would evict real objects
	// to make room for the memory of missing ones.
	neg map[string]time.Time
}

func newIndex() *index {
	return &index{
		m:     make(map[string]*entry),
		fe:    make(map[string]*frontendUse),
		tombs: make(map[string]struct{}),
		neg:   make(map[string]time.Time),
		cold:  true,
	}
}

// linkFront puts e at the most-recently-used end. Callers hold mu.
func (ix *index) linkFront(e *entry) {
	e.prev = nil
	e.next = ix.head
	if ix.head != nil {
		ix.head.prev = e
	}
	ix.head = e
	if ix.tail == nil {
		ix.tail = e
	}
}

// unlink removes e from the list. Callers hold mu.
func (ix *index) unlink(e *entry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		ix.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		ix.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

// account adds delta entries and bytes to a front-end's running totals,
// dropping the record when it reaches zero so that a front-end that is wiped
// stops appearing in the admin API's breakdown.
func (ix *index) account(frontend string, entries, bytes int64) {
	u := ix.fe[frontend]
	if u == nil {
		u = &frontendUse{}
		ix.fe[frontend] = u
	}
	u.entries += entries
	u.bytes += bytes
	if u.entries <= 0 && u.bytes <= 0 {
		delete(ix.fe, frontend)
	}
}

// set records an object that is now on disk, replacing whatever the index
// held for the key.
func (ix *index) set(e *entry) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if old, ok := ix.m[e.key]; ok {
		ix.unlink(old)
		ix.used -= old.onDisk
		ix.n--
		ix.account(old.frontend, -1, -old.onDisk)
	}
	ix.m[e.key] = e
	ix.linkFront(e)
	ix.used += e.onDisk
	ix.n++
	ix.account(e.frontend, 1, e.onDisk)
	// A key being written again is a key that exists: forget that it was
	// ever deleted, or the walk still running behind us would skip the file
	// we just created.
	delete(ix.tombs, e.key)
	delete(ix.neg, e.key)
}

// addIfAbsent records an object the index did not know about, and reports
// whether it took it.
//
// Both the start-up walk and a Get that hits a file the walk has not reached
// yet come through here. It must not overwrite an existing entry: the walk's
// lastAccess comes from mtime, which is strictly worse information than the
// access this process has already observed.
func (ix *index) addIfAbsent(e *entry) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if _, ok := ix.m[e.key]; ok {
		return false
	}
	if _, dead := ix.tombs[e.key]; dead {
		return false
	}
	ix.m[e.key] = e
	// Seeded entries go in at the front even though their lastAccess may be
	// old. Order is repaired below by sortByAccess when the walk finishes;
	// until then the cache is serving, and an eviction during the walk that
	// picks a slightly wrong victim costs one re-fetch.
	ix.linkFront(e)
	ix.used += e.onDisk
	ix.n++
	ix.account(e.frontend, 1, e.onDisk)
	return true
}

// touch marks the key as used now and returns whether the index had it.
func (ix *index) touch(key string, at time.Time) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	e, ok := ix.m[key]
	if !ok {
		return false
	}
	e.lastAccess = at
	ix.unlink(e)
	ix.linkFront(e)
	return true
}

// remove drops the key from the index and returns what it held, or nil.
func (ix *index) remove(key string) *entry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.removeLocked(key)
}

func (ix *index) removeLocked(key string) *entry {
	if ix.cold {
		// While the walk is running, a deletion has to be remembered rather
		// than merely applied: see tombs.
		ix.tombs[key] = struct{}{}
	}
	e, ok := ix.m[key]
	if !ok {
		return nil
	}
	delete(ix.m, key)
	ix.unlink(e)
	ix.used -= e.onDisk
	ix.n--
	ix.account(e.frontend, -1, -e.onDisk)
	return e
}

// takeLRU removes entries from the least-recently-used end until at least n
// bytes have been accounted for, and returns them for the caller to unlink
// from the volume.
//
// The entries leave the index BEFORE their files leave the disk, on purpose:
// the unlink is a syscall per victim and holding the index lock across a few
// thousand of them would stall every reader on the box. The cost is that the
// index briefly under-reports usage, which only ever makes the GC evict less
// than it could.
//
// frontend, when non-empty, restricts the victims to that front-end.
func (ix *index) takeLRU(n int64, frontend string) []*entry {
	if n <= 0 {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var (
		victims []*entry
		freed   int64
	)
	for e := ix.tail; e != nil && freed < n; {
		prev := e.prev
		if frontend == "" || e.frontend == frontend {
			freed += e.onDisk
			victims = append(victims, e)
			ix.removeLocked(e.key)
		}
		e = prev
	}
	return victims
}

// snapshot copies every live entry out as a tier.Entry.
//
// It copies rather than exposing the map because the alternative is holding
// the index lock for the whole of a paged List, and List is an admin-API call
// that a human may leave open. O(n) and an allocation is the right price for
// never letting the UI block a build.
func (ix *index) snapshot() []tier.Entry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]tier.Entry, 0, len(ix.m))
	for _, e := range ix.m {
		out = append(out, tier.Entry{
			Key:        e.key,
			Size:       e.size,
			LastAccess: e.lastAccess,
			Immutable:  e.immutable,
		})
	}
	return out
}

func (ix *index) stats() (entries, used int64) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.n, ix.used
}

func (ix *index) usedBy(frontend string) (entries, bytes int64) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if u := ix.fe[frontend]; u != nil {
		return u.entries, u.bytes
	}
	return 0, 0
}

// finishWalk marks the index warm and repairs the LRU order.
//
// The order matters: entries seeded by the walk went in at the front in
// directory order, which is meaningless. Sorting by lastAccess here is what
// makes the first eviction after a restart an LRU eviction rather than a
// readdir-order one.
func (ix *index) finishWalk() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.sortByAccessLocked()
	ix.cold = false
	ix.tombs = make(map[string]struct{})
}

// sortByAccessLocked rebuilds the list newest-first. Callers hold mu.
func (ix *index) sortByAccessLocked() {
	all := make([]*entry, 0, len(ix.m))
	for _, e := range ix.m {
		all = append(all, e)
	}
	sortEntriesByAccess(all)
	ix.head, ix.tail = nil, nil
	for i := len(all) - 1; i >= 0; i-- {
		all[i].prev, all[i].next = nil, nil
		ix.linkFront(all[i])
	}
}

// --- negative entries -------------------------------------------------

// putNegative records that the key is known to be absent until now+ttl.
func (ix *index) putNegative(key string, until time.Time) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.neg[key] = until
}

// getNegative reports whether a live negative entry covers the key, dropping
// it when it has expired so that a key nobody asks about twice does not sit
// in memory until the expiry ticker comes round.
func (ix *index) getNegative(key string, now time.Time) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	until, ok := ix.neg[key]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(ix.neg, key)
		return false
	}
	return true
}

// expireNegatives drops every negative entry that has run out, and returns
// how many went.
func (ix *index) expireNegatives(now time.Time) int64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var n int64
	for k, until := range ix.neg {
		if !now.Before(until) {
			delete(ix.neg, k)
			n++
		}
	}
	return n
}

// invalidatePrefix forgets every negative entry under the prefix.
func (ix *index) invalidatePrefix(prefix string) int64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var n int64
	for k := range ix.neg {
		if strings.HasPrefix(k, prefix) {
			delete(ix.neg, k)
			n++
		}
	}
	return n
}

func (ix *index) negatives() int64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return int64(len(ix.neg))
}

// --- persistence ------------------------------------------------------

type indexRecord struct {
	Key        string    `json:"k"`
	Size       int64     `json:"s"`
	OnDisk     int64     `json:"d"`
	LastAccess time.Time `json:"a"`
	Immutable  bool      `json:"im,omitempty"`
}

type indexFile struct {
	Version int    `json:"version"`
	Dir     string `json:"dir"`
	// Entries are stored most-recently-used first, so that loading restores
	// the LRU order without having to sort.
	Entries []indexRecord `json:"entries"`
}

// save writes the index beside the tree, atomically.
//
// Negative entries are deliberately NOT persisted. They are a memory of what
// an upstream said, and a restart is exactly the moment to ask again -- a
// negative that survives a restart turns a transient upstream failure into a
// cached one.
func (ix *index) save(dir, path string) error {
	ix.mu.Lock()
	recs := make([]indexRecord, 0, len(ix.m))
	for e := ix.head; e != nil; e = e.next {
		recs = append(recs, indexRecord{
			Key:        e.key,
			Size:       e.size,
			OnDisk:     e.onDisk,
			LastAccess: e.lastAccess,
			Immutable:  e.immutable,
		})
	}
	ix.mu.Unlock()

	b, err := json.Marshal(indexFile{Version: indexVersion, Dir: dir, Entries: recs})
	if err != nil {
		return fmt.Errorf("disk: encode index: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".index-*")
	if err != nil {
		return fmt.Errorf("disk: create index temp: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("disk: write index: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("disk: sync index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("disk: close index: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("disk: rename index: %w", err)
	}
	return nil
}

// load replaces the index's contents from the persisted file.
//
// It trusts the file completely rather than checking that each object is
// still there, because the caller has already established that no directory
// in the tree has changed since the file was written. Stat-ing ten thousand
// files to re-confirm that would cost most of what the persisted index was
// meant to save.
func (ix *index) load(dir, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f indexFile
	if err := json.Unmarshal(b, &f); err != nil {
		return errStaleIndex
	}
	if f.Version != indexVersion {
		return errStaleIndex
	}
	if f.Dir != "" && f.Dir != dir {
		// A volume that was moved or a copied index: the paths in it would
		// point at another tree's objects.
		return errStaleIndex
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.m = make(map[string]*entry, len(f.Entries))
	ix.fe = make(map[string]*frontendUse)
	ix.head, ix.tail = nil, nil
	ix.used, ix.n = 0, 0
	for i := len(f.Entries) - 1; i >= 0; i-- {
		r := f.Entries[i]
		if r.Key == "" || r.OnDisk <= 0 {
			continue
		}
		e := &entry{
			key:        r.Key,
			frontend:   frontendDir(r.Key),
			size:       r.Size,
			onDisk:     r.OnDisk,
			lastAccess: r.LastAccess,
			immutable:  r.Immutable,
		}
		ix.m[e.key] = e
		ix.linkFront(e)
		ix.used += e.onDisk
		ix.n++
		ix.account(e.frontend, 1, e.onDisk)
	}
	return nil
}

// indexIsFresh reports whether the persisted index can be believed.
//
// The question it answers is "has anything changed in the tree since that
// file was written", and it answers it from directory mtimes: a file created,
// renamed into or unlinked from a directory moves that directory's mtime. A
// couple of hundred stats settles it, against a walk that opens every object.
//
// The tree's ROOT is excluded on purpose. Writing .index into it moves the
// root's mtime to after the index's own, so including it would make the fast
// path unreachable forever. Nothing else writes to the root except the
// creation of a front-end directory, and that directory's own mtime is new,
// so the check still catches it.
func indexIsFresh(dir, path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	newest, err := newestSubdirMTime(dir)
	if err != nil {
		return false
	}
	return !st.ModTime().Before(newest)
}

func newestSubdirMTime(dir string) (time.Time, error) {
	var newest time.Time
	fronts, err := os.ReadDir(dir)
	if err != nil {
		return newest, err
	}
	for _, f := range fronts {
		if !f.IsDir() || strings.HasPrefix(f.Name(), ".") {
			continue
		}
		fi, err := f.Info()
		if err != nil {
			return newest, err
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		shards, err := os.ReadDir(filepath.Join(dir, f.Name()))
		if err != nil {
			return newest, err
		}
		for _, s := range shards {
			if !s.IsDir() {
				continue
			}
			si, err := s.Info()
			if err != nil {
				return newest, err
			}
			if si.ModTime().After(newest) {
				newest = si.ModTime()
			}
		}
	}
	return newest, nil
}
