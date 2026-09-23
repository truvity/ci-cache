package chain_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truvity/ci-cache/engine/chain"
	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/engine/tier/tiertest"
)

// mem is a labelled in-memory tier; the label is what Name reports, which is
// what makes a failure message name the tier that misbehaved.
func mem(label string) *tiertest.Memory {
	m := tiertest.NewMemory()
	m.Label = label
	return m
}

func read(t *testing.T, c *chain.Chain, key string) (string, tier.Meta) {
	t.Helper()
	rc, m, err := c.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return string(b), m
}

func put(t *testing.T, target interface {
	Put(context.Context, string, io.Reader, tier.Meta) error
}, key, body string, m tier.Meta) {
	t.Helper()
	m.Size = int64(len(body))
	if err := target.Put(context.Background(), key, strings.NewReader(body), m); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

// A hit at the back must leave the object in every tier in front, which is
// the one behaviour the whole cache rests on: without it a server's disk
// never warms and every read is a bucket read for ever.
func TestAHitAtTheBackFaultsForward(t *testing.T) {
	t.Parallel()

	front, mid, back := mem("front"), mem("mid"), mem("back")
	c := chain.New([]tier.Tier{front, mid, back})

	put(t, back, "go/build/output/aa/x", "compiled", tier.Meta{Immutable: true})

	got, _ := read(t, c, "go/build/output/aa/x")
	if got != "compiled" {
		t.Fatalf("read %q, want %q", got, "compiled")
	}

	for _, ti := range []*tiertest.Memory{front, mid} {
		if _, err := ti.Stat(context.Background(), "go/build/output/aa/x"); err != nil {
			t.Errorf("%s did not receive the faulted object: %v", ti.Name(), err)
		}
	}
}

// ModTime is not decoration: the Go toolchain compares the modification time
// of an output, so a fault-in that invented one would make a warm cache look
// stale to the very tool it serves.
func TestFaultingForwardCarriesTheModTime(t *testing.T) {
	t.Parallel()

	front, back := mem("front"), mem("back")
	c := chain.New([]tier.Tier{front, back})

	want := time.Date(2026, 9, 23, 11, 22, 33, 44, time.UTC)
	put(t, back, "k", "body", tier.Meta{ModTime: want, ContentType: "application/zip"})

	_, m := read(t, c, "k")
	if !m.ModTime.Equal(want) {
		t.Errorf("mod time = %s, want %s", m.ModTime, want)
	}
	if m.ContentType != "application/zip" {
		t.Errorf("content type = %q, want application/zip", m.ContentType)
	}

	fm, err := front.Stat(context.Background(), "k")
	if err != nil {
		t.Fatalf("front tier: %v", err)
	}
	if !fm.ModTime.Equal(want) {
		t.Errorf("faulted copy's mod time = %s, want %s", fm.ModTime, want)
	}
}

// Sixty-four parallel compile actions that all miss locally must become one
// read of the tier behind, not sixty-four.
func TestConcurrentMissesReadTheTierBehindOnce(t *testing.T) {
	t.Parallel()

	front, back := mem("front"), mem("back")
	var reads atomic.Int64
	release := make(chan struct{})
	back.BeforeGet = func(context.Context, string) error {
		reads.Add(1)
		<-release
		return nil
	}
	put(t, back, "k", "shared", tier.Meta{})

	c := chain.New([]tier.Tier{front, back})

	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, _, err := c.Get(context.Background(), "k")
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			if string(b) != "shared" {
				t.Errorf("read %q, want shared", b)
			}
		}()
	}
	// Let the single in-flight read finish once every caller is queued
	// behind it.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := reads.Load(); n != 1 {
		t.Errorf("the tier behind was read %d times, want 1", n)
	}
}

// A chain is only as available as its front tier makes it. A disk that has
// gone read-only must not hide an object the bucket still has.
func TestABrokenFrontTierDoesNotHideTheObject(t *testing.T) {
	t.Parallel()

	front, back := mem("front"), mem("back")
	front.BeforeGet = func(context.Context, string) error { return errors.New("disk is on fire") }
	put(t, back, "k", "still here", tier.Meta{})

	got, _ := read(t, chain.New([]tier.Tier{front, back}), "k")
	if got != "still here" {
		t.Fatalf("read %q, want %q", got, "still here")
	}
}

// Put is judged by the FIRST tier, because that is the one the caller reads
// from next. A bucket that refuses is a slower cache; a disk that refuses is
// no cache at all.
func TestPutFailsOnTheFrontTierAndToleratesTheRest(t *testing.T) {
	t.Parallel()

	front, back := mem("front"), mem("back")
	back.BeforePut = func(context.Context, string) error { return errors.New("bucket unreachable") }
	c := chain.New([]tier.Tier{front, back})

	put(t, c, "k", "body", tier.Meta{})
	if _, err := front.Stat(context.Background(), "k"); err != nil {
		t.Errorf("front tier did not get the object: %v", err)
	}

	front.BeforePut = func(context.Context, string) error { return errors.New("disk full") }
	err := c.Put(context.Background(), "k2", strings.NewReader("body"), tier.Meta{Size: 4})
	if err == nil {
		t.Error("Put succeeded although the front tier refused it")
	}
}

// An object larger than the in-memory threshold must still round-trip, and
// must not be held whole: the spill path is what keeps memory a property of
// concurrency rather than of whatever is being built.
func TestLargeObjectsRoundTripThroughTheSpillPath(t *testing.T) {
	t.Parallel()

	front, back := mem("front"), mem("back")
	body := strings.Repeat("x", 9<<20)
	put(t, back, "big", body, tier.Meta{})

	got, m := read(t, chain.New([]tier.Tier{front, back}), "big")
	if got != body {
		t.Fatalf("read %d bytes, want %d", len(got), len(body))
	}
	if m.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", m.Size, len(body))
	}
	if _, err := front.Stat(context.Background(), "big"); err != nil {
		t.Errorf("the large object was not faulted forward: %v", err)
	}
}

// Write-behind exists so a runner never waits on an upload. When the queue
// declines, the object is still cached locally and the caller still succeeds.
func TestWriteBehindTakesTheTiersBehindTheFirst(t *testing.T) {
	t.Parallel()

	front, back := mem("front"), mem("back")
	var offered atomic.Int64
	c := chain.New([]tier.Tier{front, back}, chain.WriteBehind(
		func(_ tier.Tier, _ string, _ tier.Meta, _ []byte) bool {
			offered.Add(1)
			return false // a full queue
		}))

	put(t, c, "k", "body", tier.Meta{})

	if offered.Load() != 1 {
		t.Errorf("write-behind was offered %d objects, want 1", offered.Load())
	}
	if _, err := front.Stat(context.Background(), "k"); err != nil {
		t.Errorf("the front tier did not get the object: %v", err)
	}
	if _, err := back.Stat(context.Background(), "k"); !errors.Is(err, tier.ErrNotFound) {
		t.Error("the tier behind was written inline although write-behind declined it")
	}
}

// A miss everywhere is a miss, not an error: it is the ordinary case and
// every caller has to be able to tell the two apart.
func TestAMissEverywhereIsErrNotFound(t *testing.T) {
	t.Parallel()

	c := chain.New([]tier.Tier{mem("front"), mem("back")})
	if _, _, err := c.Get(context.Background(), "absent"); !errors.Is(err, tier.ErrNotFound) {
		t.Errorf("Get of an absent key = %v, want ErrNotFound", err)
	}
	if _, err := c.Stat(context.Background(), "absent"); !errors.Is(err, tier.ErrNotFound) {
		t.Errorf("Stat of an absent key = %v, want ErrNotFound", err)
	}
}

// Delete must reach every tier: one left holding the object would serve it
// again on the next read, which is what makes a wipe look like it did not
// happen.
func TestDeleteReachesEveryTier(t *testing.T) {
	t.Parallel()

	front, back := mem("front"), mem("back")
	c := chain.New([]tier.Tier{front, back})
	put(t, c, "k", "body", tier.Meta{})

	if err := c.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, ti := range []*tiertest.Memory{front, back} {
		if _, err := ti.Stat(context.Background(), "k"); !errors.Is(err, tier.ErrNotFound) {
			t.Errorf("%s still holds the object after Delete", ti.Name())
		}
	}
}
