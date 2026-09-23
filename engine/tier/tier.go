// Package tier is the cache's one abstraction: a place that maps a key to
// bytes.
//
// Everything else in this repository is either a Tier or a front-end that
// maps somebody's protocol onto keys. That is what makes three tiers the same
// code in three deployments -- a server is disk over a bucket, a runner's
// agent is disk over the network, a laptop is disk over a bucket -- rather
// than three programs that happen to look alike.
package tier

import (
	"context"
	"errors"
	"io"
	"time"
)

// Meta is what a tier knows about an object besides its bytes.
type Meta struct {
	// Size in bytes. On a Put it may be zero when the caller does not know
	// the length in advance; the tier records what it actually stored.
	Size int64
	// ModTime is when whatever produced the object made it, not when it was
	// cached. The Go toolchain compares the modification time of an output,
	// so faulting an object from one tier into another must carry it across.
	ModTime time.Time
	// ContentType is served back by the HTTP front-ends; empty for opaque
	// bytes.
	ContentType string
	// Immutable objects may be written once. A second Put returns ErrExists
	// instead of overwriting, which is what lets a content-addressed key be
	// trusted once it has been read.
	Immutable bool
}

// Tier is one place a key's bytes may live.
//
// Implementations must be safe for concurrent use: a cache's whole job is to
// be asked for the same key by everything at once.
type Tier interface {
	// Get returns the object's bytes and metadata, or ErrNotFound. The
	// caller closes the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, Meta, error)

	// Put stores the object. For an Immutable key that is already present it
	// returns ErrExists without reading the body; when the tier cannot make
	// room it returns ErrNoSpace.
	Put(ctx context.Context, key string, r io.Reader, m Meta) error

	// Stat reports the object's metadata without its bytes, or ErrNotFound.
	Stat(ctx context.Context, key string) (Meta, error)

	// Delete removes the object. Deleting an absent key is not an error: a
	// wipe that runs twice should not fail the second time.
	Delete(ctx context.Context, key string) error

	// Name identifies the tier in metrics and logs ("disk", "bucket",
	// "remote"). It is a label, not an address.
	Name() string
}

// The errors every tier speaks. Callers match with errors.Is, so an
// implementation is free to wrap one with the detail a human needs.
var (
	// ErrNotFound is a miss. It is the ordinary case, not a failure: nothing
	// should log it at anything above debug.
	ErrNotFound = errors.New("tier: not found")

	// ErrExists means an immutable key is already present. Callers that race
	// to write the same content-addressed object treat it as success.
	ErrExists = errors.New("tier: already exists")

	// ErrNoSpace means the tier cannot make room for this object. It is
	// distinct from a write failure: the cache is full, not broken, and a
	// caller may carry on without caching.
	ErrNoSpace = errors.New("tier: no space")
)

// Deleter is a Tier that can remove everything under a prefix in one call.
//
// A tier that does not implement it is walked key by key instead. The bucket
// does implement it, because deleting ten thousand objects one request at a
// time is the difference between a wipe that finishes and one that is still
// running when somebody gives up on it.
type Deleter interface {
	DeletePrefix(ctx context.Context, prefix string) (entries, bytes int64, err error)
}

// Lister is a Tier whose contents can be enumerated.
//
// Only the disk tier implements it: listing a bucket costs money and time,
// and the admin API's List exists to answer "what is on the volume", which is
// a question about the disk.
type Lister interface {
	List(ctx context.Context, prefix, pageToken string, pageSize int) (entries []Entry, next string, err error)
}

// Entry is one object as List reports it.
type Entry struct {
	Key        string
	Size       int64
	LastAccess time.Time
	Immutable  bool
}
