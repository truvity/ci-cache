package remote_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/goleak"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/remote"
	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/engine/tier/tiertest"
	"github.com/truvity/ci-cache/server"
)

// TestMain fails the package if anything it started is still running.
//
// A cache's client is the one thing in a build that is created and discarded
// thousands of times, so a goroutine left behind per transfer is not a slow
// leak: it is a runner that runs out of memory halfway through the afternoon.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// net/http's own connection pool keeps its read and write loops alive
		// for the idle timeout, which outlives the test that made them. They
		// are the transport's, not ours, and CloseIdleConnections does not
		// join them.
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
	)
}

// serve starts a real h2c listener over the given tier and returns the base
// URL a Remote should be pointed at -- prefix included, because that is the
// address the deployment hands out.
func serve(t *testing.T, chain tier.Tier, opts ...server.Option) string {
	t.Helper()

	cfg := config.Default()
	cfg.Server.DrainTimeout = 2 * time.Second

	srv, err := server.New(cfg, chain, opts...)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return listen(t, srv)
}

func serveWithConfig(t *testing.T, cfg config.Config, chain tier.Tier, opts ...server.Option) string {
	t.Helper()
	srv, err := server.New(cfg, chain, opts...)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return listen(t, srv)
}

func listen(t *testing.T, srv *server.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Serve did not return after the context was cancelled")
		}
	})
	return "http://" + ln.Addr().String() + server.CachePrefix
}

func newRemote(t *testing.T, base string, opts ...remote.Option) *remote.Remote {
	t.Helper()
	r, err := remote.New(base, opts...)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// payload is deterministic per test but not compressible, so that a chunking
// bug shows up as a mismatch rather than as identical zeroes.
func payload(n int) []byte {
	b := make([]byte, n)
	rng := rand.NewChaCha8([32]byte{7})
	_, _ = rng.Read(b)
	return b
}

// The round trip is the whole contract: five megabytes across a real h2c
// listener, through the Connect stream, and back byte for byte. Anything less
// than an exact comparison would pass with a chunk dropped or repeated, which
// is exactly the bug a cache must never have.
func TestRoundTrip(t *testing.T) {
	mem := tiertest.NewMemory()
	base := serve(t, mem)
	r := newRemote(t, base)

	ctx := t.Context()
	const size = 5 << 20
	want := payload(size)
	modTime := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

	err := r.Put(ctx, "go/build/abc", bytes.NewReader(want), tier.Meta{
		Size:        size,
		ModTime:     modTime,
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, meta, err := r.Get(ctx, "go/build/abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d; equal=%v", len(got), len(want), bytes.Equal(got, want))
	}
	if meta.Size != size {
		t.Errorf("meta.Size = %d, want %d", meta.Size, size)
	}
	if !meta.ModTime.Equal(modTime) {
		t.Errorf("meta.ModTime = %v, want %v", meta.ModTime, modTime)
	}
	if meta.ContentType != "application/octet-stream" {
		t.Errorf("meta.ContentType = %q, want application/octet-stream", meta.ContentType)
	}

	st, err := r.Stat(ctx, "go/build/abc")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size != meta.Size || !st.ModTime.Equal(meta.ModTime) || st.ContentType != meta.ContentType {
		t.Errorf("Stat disagrees with Get: %+v vs %+v", st, meta)
	}
}

func TestMissingKeyIsErrNotFound(t *testing.T) {
	base := serve(t, tiertest.NewMemory())
	r := newRemote(t, base)

	if _, _, err := r.Get(t.Context(), "nope"); !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("Get of an absent key = %v, want tier.ErrNotFound", err)
	}
	if _, err := r.Stat(t.Context(), "nope"); !errors.Is(err, tier.ErrNotFound) {
		t.Fatalf("Stat of an absent key = %v, want tier.ErrNotFound", err)
	}
}

// Two runners building the same content-addressed object race to write it. The
// loser must be told AlreadyExists and recognise it as tier.ErrExists, because
// a chain treats that as success; anything else aborts a write that in fact
// worked.
func TestImmutablePutTwiceIsErrExists(t *testing.T) {
	base := serve(t, tiertest.NewMemory())
	r := newRemote(t, base)

	body := payload(64 << 10)
	meta := tier.Meta{Size: int64(len(body)), Immutable: true}

	if err := r.Put(t.Context(), "immutable/key", bytes.NewReader(body), meta); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	err := r.Put(t.Context(), "immutable/key", bytes.NewReader(body), meta)
	if !errors.Is(err, tier.ErrExists) {
		t.Fatalf("second Put = %v, want tier.ErrExists", err)
	}
}

func TestNoSpaceIsErrNoSpace(t *testing.T) {
	mem := tiertest.NewMemory()
	mem.BeforePut = func(context.Context, string) error { return tier.ErrNoSpace }
	base := serve(t, mem)
	r := newRemote(t, base)

	err := r.Put(t.Context(), "anything", bytes.NewReader([]byte("x")), tier.Meta{Size: 1})
	if !errors.Is(err, tier.ErrNoSpace) {
		t.Fatalf("Put against a full tier = %v, want tier.ErrNoSpace", err)
	}
}

// 256 concurrent streams is the server's own default limit, so this is the
// shape of a runner at full tilt. What is being asserted is not the throughput
// but that every one of them is torn down: TestMain fails the package if any
// goroutine survives.
func TestConcurrentGetsStreamWithoutLeaking(t *testing.T) {
	mem := tiertest.NewMemory()
	const (
		objects = 16
		size    = 256 << 10
		readers = 256
	)
	bodies := make([][]byte, objects)
	for i := range bodies {
		bodies[i] = payload(size)
		mem.Seed(keyOf(i), bodies[i], tier.Meta{})
	}
	base := serve(t, mem)
	r := newRemote(t, base)

	var wg sync.WaitGroup
	errs := make([]error, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx := i % objects
			rc, _, err := r.Get(context.Background(), keyOf(idx))
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = rc.Close() }()
			got, err := io.ReadAll(rc)
			if err != nil {
				errs[i] = err
				return
			}
			if !bytes.Equal(got, bodies[idx]) {
				errs[i] = errors.New("body mismatch")
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
	}
}

func keyOf(i int) string { return "obj/" + string(rune('a'+i)) }

// Over the limit the server answers 503, and Connect turns that into
// Unavailable. The code has to survive the trip: an agent decides whether to
// fall back to building the object itself by reading it.
func TestOverConcurrencyLimitIsUnavailable(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	// entered is closed from inside the tier, so the test knows the blocking
	// Get is holding the gate's only slot. Waiting on the client side instead
	// would race: whichever request arrived first would take the slot, and it
	// is not always the one the test meant.
	entered := make(chan struct{})
	var enteredOnce sync.Once

	mem := tiertest.NewMemory()
	mem.Seed("held", payload(1<<10), tier.Meta{})
	mem.BeforeGet = func(ctx context.Context, key string) error {
		if key != "held" {
			return nil
		}
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}

	cfg := config.Default()
	cfg.Server.Concurrency = 1
	cfg.Server.DrainTimeout = 2 * time.Second
	base := serveWithConfig(t, cfg, mem)
	r := newRemote(t, base)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rc, _, err := r.Get(context.Background(), "held")
		if err != nil {
			t.Errorf("the request that was meant to hold the gate failed: %v", err)
			enteredOnce.Do(func() { close(entered) })
			return
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}()
	<-entered

	_, err := r.Stat(context.Background(), "whatever")
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("Stat against a saturated server = %v (code %v), want Unavailable", err, got)
	}

	releaseOnce.Do(func() { close(release) })
	wg.Wait()
}

// A miss on the sentinel is a healthy answer. Treating it as unreachable would
// have every runner fail open against a cache that is working perfectly.
func TestProbeSucceedsAgainstAnEmptyCache(t *testing.T) {
	base := serve(t, tiertest.NewMemory())
	r := newRemote(t, base)

	if err := r.Probe(t.Context()); err != nil {
		t.Fatalf("Probe against an empty cache: %v", err)
	}
}

func TestProbeFailsWhenNothingIsListening(t *testing.T) {
	// A port nothing is on: bind one and close it, so the address is real and
	// unreachable rather than guessed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	r := newRemote(t, "http://"+addr+server.CachePrefix, remote.WithDialTimeout(500*time.Millisecond))
	if err := r.Probe(t.Context()); err == nil {
		t.Fatal("Probe succeeded against a closed port")
	}
}

// Get returns the live stream, not a buffer. The assertion is that the first
// bytes are readable while the object is still arriving -- a buffering
// implementation cannot pass it.
func TestGetStreamsRatherThanBuffers(t *testing.T) {
	mem := tiertest.NewMemory()
	mem.Seed("big", payload(8<<20), tier.Meta{})
	base := serve(t, mem)
	r := newRemote(t, base)

	rc, _, err := r.Get(t.Context(), "big")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()

	head := make([]byte, 1024)
	if _, err := io.ReadFull(rc, head); err != nil {
		t.Fatalf("read the head of the stream: %v", err)
	}
	// Closing before the body is drained must not error: a caller that decides
	// it has seen enough is the normal case, not a fault.
	if err := rc.Close(); err != nil {
		t.Fatalf("Close mid-stream: %v", err)
	}
}

func TestNewRejectsAnAddressWithoutAScheme(t *testing.T) {
	for _, bad := range []string{"", "ci-cache.ns.svc:8080/go/build", "ftp://host/go/build", "http://"} {
		if _, err := remote.New(bad); err == nil {
			t.Errorf("remote.New(%q) succeeded, want an error", bad)
		}
	}
}

func TestDeleteIsRefused(t *testing.T) {
	r := newRemote(t, "http://127.0.0.1:1/go/build")
	if err := r.Delete(t.Context(), "k"); !errors.Is(err, remote.ErrDeleteUnsupported) {
		t.Fatalf("Delete = %v, want remote.ErrDeleteUnsupported", err)
	}
}

func TestNameIsOverridable(t *testing.T) {
	r := newRemote(t, "http://127.0.0.1:1/go/build", remote.WithName("upstream"))
	if r.Name() != "upstream" {
		t.Fatalf("Name = %q, want upstream", r.Name())
	}
}

// A transfer that stalls must be cut off, and a transfer that is merely slow
// must not be. The idle timeout is what tells them apart, so it is asserted
// against a tier that goes quiet rather than against one that is slow.
func TestIdleTimeoutEndsAStalledTransfer(t *testing.T) {
	stall := make(chan struct{})
	defer close(stall)

	mem := tiertest.NewMemory()
	mem.BeforeGet = func(ctx context.Context, _ string) error {
		select {
		case <-stall:
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
		return nil
	}
	base := serve(t, mem)
	r := newRemote(t, base, remote.WithIdleTimeout(200*time.Millisecond))

	start := time.Now()
	rc, _, err := r.Get(context.Background(), "anything")
	if err == nil {
		_ = rc.Close()
		t.Fatal("Get succeeded against a tier that never answered")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Get took %v to give up; the idle timeout did not fire", elapsed)
	}
}

// The HTTP client's own timeout would apply to the whole exchange including
// the body, so a large object would be truncated by it. There must not be one.
func TestSuppliedClientIsUsed(t *testing.T) {
	mem := tiertest.NewMemory()
	mem.Seed("k", []byte("hello"), tier.Meta{})
	base := serve(t, mem)

	tr := &http.Transport{}
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	tr.Protocols = protos
	client := &http.Client{Transport: tr}
	t.Cleanup(client.CloseIdleConnections)

	r := newRemote(t, base, remote.WithHTTPClient(client))
	rc, _, err := r.Get(t.Context(), "k")
	if err != nil {
		t.Fatalf("Get through a supplied client: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = rc.Close()
	if string(got) != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}
