package storetest

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/truvity/ci-cache/engine/bucket"
	"github.com/truvity/ci-cache/engine/tier"
)

// zstdFrameHex is a real zstd frame, produced by zstd(1) and pasted here.
//
// It is a literal rather than something compressed at test time so that this
// package needs no compressor: what matters is that the bytes on the wire are
// a compressed blob of the kind the cache actually stores, not how they were
// produced. It decodes to eight copies of one line, and nothing in the test
// decodes it -- the assertion is that the bytes come back exactly.
const zstdFrameHex = "28b52ffd64280115020052040e11a0ede084ecf06b17696955e1234cbf674640" +
	"4c4e2fad32493dd33717160f9711692ffc1eb75b6daf1f4c5abb7337f88252ee" +
	"b359dd703601000538a286280cfe4a1a"

// keyShape is the part of the key that made this test necessary.
//
// A module path carries '@' before its version and a build-cache action key
// is base64 with '=' padding, so both end up in an object key routinely. They
// are what a signer, a path-style URL and a store's own key parsing each get
// to disagree about, and a probe under a tidy key proves nothing about them.
const keyShape = "storetest/github.com/truvity/ci-cache@v0.1.0/action=YWJjZGVm=="

func TestRealStore(t *testing.T) {
	cfg, ok, err := FromEnv()
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	if !ok {
		t.Skipf("no store configured: set %s (with %s, or %s=1) to run this against a real bucket", EnvBucket, EnvEndpoint, EnvAWS)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	b, err := bucket.New(ctx, cfg)
	if err != nil {
		t.Fatalf("new bucket: %v", err)
	}
	if err := b.Reachable(ctx); err != nil {
		t.Fatalf("reachable: %v", err)
	}

	body, err := hex.DecodeString(zstdFrameHex)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}

	// The key is unique per run so that two people, or two CI jobs, testing
	// against the same scratch bucket do not delete each other's object out
	// from under the assertions.
	key := fmt.Sprintf("%s/%d-%d", keyShape, os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		// A best-effort cleanup on a separate context: the test's own may
		// already have expired, and an object left behind is litter in
		// somebody's bucket.
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := b.Delete(c, key); err != nil {
			t.Logf("cleanup %s: %v", key, err)
		}
	})

	// A time with a non-zero nanosecond, because truncating it is exactly the
	// failure that makes the Go toolchain rebuild a cached output.
	mod := time.Date(2026, 9, 23, 18, 4, 5, 123456789, time.UTC)
	m := tier.Meta{Size: int64(len(body)), ModTime: mod, ContentType: "application/zstd"}

	if err := b.Put(ctx, key, bytes.NewReader(body), m); err != nil {
		t.Fatalf("put: %v", err)
	}

	rc, got, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	read, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(read, body) {
		t.Fatalf("body round trip: got %d bytes %x, want %d bytes %x", len(read), read, len(body), body)
	}
	checkMeta(t, "get", got, m)

	st, err := b.Stat(ctx, key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	checkMeta(t, "stat", st, m)

	// The key is now present, so an immutable write of it must come back as
	// ErrExists from the store's own precondition rather than as an error or,
	// worse, as a silent overwrite.
	err = b.Put(ctx, key, bytes.NewReader(body), tier.Meta{Size: int64(len(body)), ModTime: mod, Immutable: true})
	if !errors.Is(err, tier.ErrExists) {
		t.Fatalf("immutable put over an existing key: got %v, want %v", err, tier.ErrExists)
	}

	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := b.Stat(ctx, key); !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("stat after delete: got %v, want %v", err, tier.ErrNotFound)
	}
}

func checkMeta(t *testing.T, what string, got, want tier.Meta) {
	t.Helper()
	if !got.ModTime.Equal(want.ModTime) {
		t.Errorf("%s: mod time %s, want %s", what, got.ModTime.Format(time.RFC3339Nano), want.ModTime.Format(time.RFC3339Nano))
	}
	if got.ContentType != want.ContentType {
		t.Errorf("%s: content type %q, want %q", what, got.ContentType, want.ContentType)
	}
	if got.Size != want.Size {
		t.Errorf("%s: size %d, want %d", what, got.Size, want.Size)
	}
}
