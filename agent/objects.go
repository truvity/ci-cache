package agent

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// objectDir is the one thing the Go toolchain will not take from a tier.
//
// GOCACHEPROG answers a "get" with a PATH: the compiler opens the file
// itself, so a cache that can only hand back a stream has to land the bytes
// somewhere on the local filesystem first. That is what this is -- a
// materialisation of objects the chain already holds, keyed by output ID, and
// throwaway by construction: anything missing from it can be fetched again.
type objectDir struct {
	root string
}

func newObjectDir(root string) (*objectDir, error) {
	// World-readable on purpose. The compiler reads these files as whoever
	// is running the build, which is not necessarily whoever is running the
	// agent, and a cache the toolchain cannot open is a cache that misses
	// every time while looking like it works.
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("agent: object directory %s: %w", root, err)
	}
	return &objectDir{root: root}, nil
}

// path is the file an output ID lands in. IDs are sharded on their first two
// hex digits so that a long-lived cache does not end up with a directory the
// filesystem walks linearly.
func (o *objectDir) path(id string) string {
	shard := "__"
	if len(id) >= 2 {
		shard = id[:2]
	}
	return filepath.Join(o.root, shard, id)
}

// have reports an object already on disk at the expected size.
//
// Size is the whole check. Output IDs are content addresses, so a file of the
// right length under the right ID is the object; re-fetching it would cost a
// copy to prove something the name already said.
func (o *objectDir) have(id string, size int64) (string, bool) {
	if id == "" {
		return "", false
	}
	p := o.path(id)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	if size > 0 && fi.Size() != size {
		return "", false
	}
	return p, true
}

// write lands the object and returns its path and the number of bytes
// written. A size of zero or less means the length is not known in advance,
// in which case whatever the reader yields is what gets stored.
//
// The reader is always drained, even when the object turns out to be present
// already: on a "put" it is the toolchain's request body, and leaving bytes
// in it desynchronises the protocol stream for every request after it.
func (o *objectDir) write(id string, size int64, modTime time.Time, r io.Reader) (string, int64, error) {
	if p, ok := o.have(id, size); ok {
		n, err := io.Copy(io.Discard, r)
		if err != nil {
			return "", 0, fmt.Errorf("agent: drain object %s: %w", id, err)
		}
		if size > 0 {
			n = size
		}
		return p, n, nil
	}

	p := o.path(id)
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("agent: object directory %s: %w", dir, err)
	}

	// Written to a temporary name and renamed into place, because the
	// toolchain is handed a path and then opens it: a reader that arrives
	// halfway through the copy must not find a short file under the name of
	// a complete one.
	tmp, err := os.CreateTemp(dir, ".tmp-"+id+"-*")
	if err != nil {
		return "", 0, fmt.Errorf("agent: object %s: %w", id, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below has succeeded

	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, fmt.Errorf("agent: object %s: %w", id, err)
	}
	if size > 0 && n != size {
		return "", 0, fmt.Errorf("agent: object %s: stored %d bytes, want %d", id, n, size)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", 0, fmt.Errorf("agent: object %s: %w", id, err)
	}
	if !modTime.IsZero() {
		// The toolchain compares an output's modification time, so a fault
		// from another tier has to carry the original across rather than
		// stamp the object with the moment it was cached.
		_ = os.Chtimes(tmpName, time.Time{}, modTime)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return "", 0, fmt.Errorf("agent: object %s: %w", id, err)
	}
	return p, n, nil
}

// prune removes materialised objects nothing has touched in maxAge.
//
// The tiers behind this directory have their own budget and their own
// eviction; this one has neither, and on a laptop it is the copy that would
// otherwise grow until somebody notices.
//
// The age it reads is the object's own modification time, which is the
// producer's and not the moment it was cached -- so an output built a month
// ago is pruned the first time it is materialised. That is deliberate: the
// tier behind still holds it, so the mistake costs one local copy and
// nothing else, and a rule that is cheap when it is wrong is worth more here
// than a second set of bookkeeping files to make it right.
func (o *objectDir) prune(maxAge time.Duration) (removed int, freed int64) {
	if maxAge <= 0 {
		return 0, 0
	}
	cutoff := time.Now().Add(-maxAge)
	_ = filepath.WalkDir(o.root, func(path string, d fs.DirEntry, err error) error {
		// A directory this cannot walk is not a reason to stop pruning the
		// rest of them.
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			return nil
		}
		if os.Remove(path) == nil {
			removed++
			freed += fi.Size()
		}
		return nil
	})
	return removed, freed
}
