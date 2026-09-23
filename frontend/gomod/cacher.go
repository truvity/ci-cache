package gomod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/goproxy/goproxy"

	"github.com/truvity/ci-cache/engine/tier"
)

// Prefix is where this front-end's keys live, under which goproxy's own names
// are used unchanged: go/mod/example.com/mod/@v/v1.2.3.zip is a key a human
// can read in a bucket listing and recognise, which matters the first time
// somebody has to decide whether an object is safe to delete.
const Prefix = "go/mod"

// entryKind is how long an entry may be believed.
type entryKind int

const (
	// kindImmutable is an entry that cannot change: a file of a resolved
	// version, or a signed checksum-database record. It is written once and
	// believed forever.
	kindImmutable entryKind = iota

	// kindTTL is an entry that is a snapshot of something moving -- the list
	// of versions, the latest version, the checksum database's tree head --
	// and is believed for as long as the configured TTL.
	kindTTL
)

// cacher adapts a tier to goproxy's Cacher.
//
// # How freshness is kept
//
// A TTL entry's expiry is judged from tier.Meta.ModTime, which this package
// writes as the moment the entry was fetched. The alternative -- a second,
// tiny object beside each entry holding a fetchedAt -- was not taken: it
// doubles the objects, costs a second round trip on the read path where the
// answer has to be quick, and leaves an orphan behind whenever the entry and
// its sidecar are not evicted together, which no tier promises.
//
// Using ModTime costs one thing, and it is the safe direction. A tier that
// does not carry ModTime across a fault hands back a zero time, every TTL
// entry then looks ancient, and the proxy re-fetches a list it already had.
// That is slower and never wrong; the sidecar's failure mode -- an entry whose
// freshness record was lost, believed forever -- is the other direction.
//
// Immutable entries do not consult ModTime at all, so the only thing at stake
// here is how often a version list is refreshed.
type cacher struct {
	tier tier.Tier

	// legacy, when set, is the key prefix a go-cache-plugin module cache
	// used. Its layout is not this one: it names objects by a hash of
	// goproxy's name rather than by the name itself, so it is a separate
	// mapping and not a different root over the same one.
	legacy string

	log *slog.Logger
}

// compile-time proof that this is what goproxy asked for; the interface is
// small enough that a signature drift would otherwise only show up when the
// proxy is wired together.
var _ goproxy.Cacher = (*cacher)(nil)

// Get implements goproxy.Cacher.
//
// It never applies the TTL. Freshness is decided before the request reaches
// goproxy, because goproxy consults its cacher in two quite different
// situations: to answer from cache, and to rescue a request whose upstream
// fetch has just failed. Hiding a stale version list here would turn the
// second case into a failed build, when serving the list from an hour ago is
// exactly what the caller wants from a proxy whose upstream is down.
func (c *cacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	key, err := c.key(name)
	if err != nil {
		return nil, err
	}

	rc, m, err := c.tier.Get(ctx, key)
	if errors.Is(err, tier.ErrNotFound) && c.legacy != "" {
		rc, m, err = c.tier.Get(ctx, c.legacyKey(name))
		if err == nil {
			c.log.Debug("served from the legacy layout", "name", name)
		}
	}
	switch {
	case errors.Is(err, tier.ErrNotFound):
		return nil, fs.ErrNotExist
	case err != nil:
		// A tier that is failing is reported as a miss so that the request
		// goes upstream and the build carries on. A proxy that returns 500
		// because its own cache is sick has turned a slow day into a broken
		// one.
		c.log.Error("cache read failed, treating as a miss", "name", name, "error", err)
		return nil, fs.ErrNotExist
	}

	return &entry{ReadCloser: rc, size: m.Size, modTime: m.ModTime}, nil
}

// Put implements goproxy.Cacher.
//
// Every failure here is swallowed after being logged. goproxy turns a Cacher
// error into a 500 even when it already has the bytes in hand, so a full disk
// or a broken bucket would stop serving modules it had just fetched
// successfully. Not caching is a slow answer; refusing to answer is not.
func (c *cacher) Put(ctx context.Context, name string, content io.ReadSeeker) error {
	key, err := c.key(name)
	if err != nil {
		return err
	}

	size, err := seekSize(content)
	if err != nil {
		c.log.Error("cache write skipped, cannot size the content", "name", name, "error", err)
		return nil
	}

	kind := classify(name)
	err = c.tier.Put(ctx, key, content, tier.Meta{
		Size: size,
		// For a TTL entry this is the fetchedAt the expiry is judged from;
		// for an immutable one nothing reads it, and writing the same thing
		// in both cases keeps one rule rather than two.
		ModTime:     time.Now(),
		ContentType: contentType(name),
		Immutable:   kind == kindImmutable,
	})
	if err != nil && !errors.Is(err, tier.ErrExists) {
		c.log.Error("cache write failed", "name", name, "error", err)
	}
	// The content is handed back to goproxy to be served, and it reads it
	// from wherever the tier left the offset.
	if _, serr := content.Seek(0, io.SeekStart); serr != nil {
		return fmt.Errorf("gomod: rewind %s: %w", name, serr)
	}
	return nil
}

// fresh reports whether a TTL entry may be served without asking upstream.
//
// A ttl of zero or less means nothing is ever fresh, which is goproxy's own
// behaviour: every list and every @latest goes upstream, and the cache is only
// a fallback for when it is unreachable.
func (c *cacher) fresh(ctx context.Context, name string, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	key, err := c.key(name)
	if err != nil {
		return false
	}
	m, err := c.tier.Stat(ctx, key)
	if err != nil {
		return false
	}
	if m.ModTime.IsZero() {
		return false
	}
	return time.Since(m.ModTime) < ttl
}

// key maps goproxy's name onto this front-end's key space.
//
// goproxy hands over a cleaned URL path, but this is the boundary where a
// name becomes a key, and a name that escaped the prefix would let a module
// request address another front-end's objects.
func (c *cacher) key(name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	return Prefix + "/" + name, nil
}

// legacyKey is go-cache-plugin's module layout: the SHA-256 of goproxy's name,
// hex encoded, sharded by its first two characters. The name itself does not
// appear, which is why a legacy bucket cannot be listed for a module and why
// nothing new is written this way.
func (c *cacher) legacyKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	h := hex.EncodeToString(sum[:])
	return c.legacy + "/" + h[:2] + "/" + h
}

// classify decides how long an entry may be believed.
func classify(name string) entryKind {
	if strings.HasPrefix(name, "sumdb/") {
		// A lookup or a tile is a record in a signed, append-only tree: it is
		// immutable by construction. The tree HEAD is not -- /latest is a
		// signed statement about a tree that grows -- so it expires like any
		// other moving target, or a proxy left alone over a weekend would
		// keep announcing Friday's tree.
		if strings.HasSuffix(name, "/latest") {
			return kindTTL
		}
		return kindImmutable
	}

	if strings.HasSuffix(name, "/@latest") || strings.HasSuffix(name, "/@v/list") {
		return kindTTL
	}

	base, ok := cutModuleFile(name)
	if !ok {
		return kindTTL
	}
	ext := path.Ext(base)
	switch ext {
	case ".info", ".mod", ".zip":
	default:
		return kindTTL
	}
	// Only a canonical version names a fixed set of bytes. The same path with
	// a branch or a tag in it -- example.com/m/@v/main.info -- is a query
	// whose answer moves every time somebody pushes, and caching that forever
	// would pin a repository to whatever it happened to be when this proxy
	// first saw it.
	if !canonicalVersion(strings.TrimSuffix(base, ext)) {
		return kindTTL
	}
	return kindImmutable
}

// cutModuleFile returns the file part of a <module>/@v/<file> name.
func cutModuleFile(name string) (string, bool) {
	i := strings.LastIndex(name, "/@v/")
	if i < 0 {
		return "", false
	}
	file := name[i+len("/@v/"):]
	if file == "" || strings.Contains(file, "/") {
		return "", false
	}
	return file, true
}

// canonicalVersion reports whether v is a canonical module version:
// vMAJOR.MINOR.PATCH with an optional pre-release, optionally marked
// +incompatible.
//
// This is deliberately a local check rather than a call into x/mod's semver.
// The only decision it feeds is "may this be believed forever", so it is
// written to say no when unsure, and a dependency pulled in to be that strict
// would be a dependency on the semantics of a version string in a path.
func canonicalVersion(v string) bool {
	v = strings.TrimSuffix(v, "+incompatible")
	if !strings.HasPrefix(v, "v") {
		return false
	}
	v = v[1:]
	if pre := strings.IndexByte(v, '-'); pre >= 0 {
		if pre == len(v)-1 {
			return false
		}
		v = v[:pre]
	}
	if strings.Contains(v, "+") {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// contentType is what the object is stored as, for the front-ends that serve
// tier objects straight back to a client. goproxy sets its own header on the
// way out, so this only matters to anything else reading the bucket.
func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".zip"):
		return "application/zip"
	case strings.HasSuffix(name, ".info"), strings.HasSuffix(name, "/@latest"):
		return "application/json; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}

// validName refuses a name that must not become a key.
func validName(name string) error {
	if name == "" {
		return fmt.Errorf("gomod: empty cache name")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("gomod: cache name %q is not relative", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("gomod: cache name %q has a %q segment", name, seg)
		}
	}
	return nil
}

// seekSize measures a ReadSeeker and leaves it at the start. The tiers want a
// size in advance -- the chain uses it to decide whether an object is held in
// memory or spilled to a file -- and every content goproxy caches is already
// seekable, so nothing has to be buffered to learn it.
func seekSize(content io.ReadSeeker) (int64, error) {
	size, err := content.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return size, nil
}

// entry is what goproxy is handed for a hit. The two extra methods are
// goproxy's optional interfaces: without Size there is no Content-Length, and
// without ModTime no Last-Modified, and a Go client that cannot see a length
// cannot show a progress bar or spot a truncated zip.
type entry struct {
	io.ReadCloser
	size    int64
	modTime time.Time
}

func (e *entry) Size() int64 { return e.size }

func (e *entry) ModTime() time.Time { return e.modTime }
