package disk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// headerSize is how many bytes every object file spends on its header before
// the body begins.
//
// It is FIXED rather than variable because Put must be able to stream a body
// of unknown length: the header is written as a placeholder, the body is
// copied while counting, and then the real header is written back over the
// first headerSize bytes. A variable-length header would mean buffering the
// whole object to learn its size first, which is exactly what a cache that
// handles 500MB Gradle entries must not do.
//
// The cost is real and bounded: 1KiB per object. On any filesystem with 4KiB
// blocks a small build-cache entry already occupies a whole block, so for the
// objects there are most of this is usually free.
const headerSize = 1024

// errCorrupt marks a file on the volume that cannot be read as an object.
//
// It is never returned to a caller -- Get turns it into tier.ErrNotFound and
// unlinks the file -- because a cache that reports its own rot to a build is
// a cache that fails builds. What matters is that the file leaves the volume:
// an unreadable file that stays is a byte the GC can never reclaim, since it
// will fail to parse on every restart's walk.
var errCorrupt = errors.New("disk: corrupt object header")

// header is the object's metadata, stored with the object rather than in a
// sidecar file.
//
// A sidecar would double the inode count and, worse, could be lost or found
// on its own: a rename can only be atomic over one file, so two files mean a
// window where the object and its metadata disagree. One file has no such
// window.
//
// Key is stored even though the path already encodes its hash. The layout
// hashes the key, so nothing on the volume can recover the key it came from,
// and List -- which answers "what is on this volume" by key prefix -- would
// be impossible after a restart. It is also what lets Get refuse a file whose
// header names a different key, which is the only defence against serving one
// object under another's name.
type header struct {
	Key         string    `json:"k"`
	Size        int64     `json:"s"`
	ModTime     time.Time `json:"mt,omitempty"`
	ContentType string    `json:"ct,omitempty"`
	Immutable   bool      `json:"im,omitempty"`
}

// encodeHeader renders h into exactly headerSize bytes: the JSON, a newline,
// then spaces.
//
// The padding is spaces rather than NULs so that `head -c 1024` on a cache
// file is readable by whoever is standing in front of a full volume at three
// in the morning.
func encodeHeader(h header) ([]byte, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("disk: encode header for %q: %w", h.Key, err)
	}
	if len(b)+1 > headerSize {
		// Refusing here beats truncating: a header that does not round-trip
		// is an object that cannot be listed or validated later.
		return nil, fmt.Errorf("disk: key %q has a %d-byte header, over the %d-byte limit", h.Key, len(b)+1, headerSize)
	}
	buf := bytes.Repeat([]byte{' '}, headerSize)
	copy(buf, b)
	buf[len(b)] = '\n'
	return buf, nil
}

// decodeHeader reads the fixed prefix from r and leaves r positioned at the
// first byte of the body, so the caller can stream straight on from it.
func decodeHeader(r io.Reader) (header, error) {
	buf := make([]byte, headerSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		// Short of a full header means the file was truncated -- a crash
		// mid-write that somehow reached its final name, or a volume that
		// filled between the write and the fsync.
		return header{}, errCorrupt
	}
	i := bytes.IndexByte(buf, '\n')
	if i <= 0 {
		return header{}, errCorrupt
	}
	var h header
	if err := json.Unmarshal(buf[:i], &h); err != nil {
		return header{}, errCorrupt
	}
	if h.Size < 0 {
		return header{}, errCorrupt
	}
	return h, nil
}
