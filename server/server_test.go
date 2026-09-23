package server_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/engine/tier/tiertest"
	"github.com/truvity/ci-cache/server"
)

// testFrontend is the smallest thing that satisfies server.Frontend, so that
// the mux's behaviour can be asserted without any real protocol in the way.
type testFrontend struct {
	prefix string
	name   string
	h      http.Handler
}

func (f *testFrontend) Prefix() string        { return f.prefix }
func (f *testFrontend) Name() string          { return f.name }
func (f *testFrontend) Handler() http.Handler { return f.h }

// echoPath answers with the path it was given, which is how a test sees
// whether the prefix was stripped.
func echoPath() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	})
}

// gate is a front-end that can be held open on demand.
//
// Requests other than probePath announce themselves on entered and then block,
// so a test can know the concurrency gate is full instead of guessing. Polling
// from the client side raced: whichever request reached the server first took
// the slot, and it was not always the one the test meant to be holding it.
type gate struct {
	entered chan string
	release chan struct{}
	once    sync.Once
}

func newGate() *gate {
	return &gate{entered: make(chan string, 8), release: make(chan struct{})}
}

func (g *gate) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// probePath is answered immediately. A probe served by the blocking
		// path would take the very slot it was sent to ask about, and hang.
		if r.URL.Path == probePath {
			_, _ = io.WriteString(w, "free")
			return
		}
		g.entered <- r.URL.Path
		select {
		case <-g.release:
		case <-r.Context().Done():
		}
	})
}

// hold fires n requests and returns once every one of them is inside the
// handler, holding a slot.
func (g *gate) hold(t *testing.T, base string, n int) *sync.WaitGroup {
	t.Helper()
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = getQuiet(fmt.Sprintf("%s/slow/held-%d", base, i))
		}()
	}
	for range n {
		select {
		case <-g.entered:
		case <-time.After(15 * time.Second):
			g.free()
			wg.Wait()
			t.Fatal("the blocking requests never reached the handler")
		}
	}
	return &wg
}

func (g *gate) free() { g.once.Do(func() { close(g.release) }) }

// probePath is the path blockUntil answers straight away, as the front-end
// sees it -- that is, with the mount prefix already stripped.
const probePath = "/probe"

// start brings up a server on a loopback port and tears it down with the test.
//
// It uses a real listener rather than httptest because /healthz only answers
// once the listener is up, and because the same shape has to serve the h2c
// round-trip tests, where httptest's HTTP/1.1 server would not do.
func start(t *testing.T, cfg config.Config, chain tier.Tier, opts ...server.Option) string {
	t.Helper()

	srv, err := server.New(cfg, chain, opts...)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
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
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after the context was cancelled")
		}
	})

	return "http://" + ln.Addr().String()
}

// getQuiet is safe to call from a goroutine that is not the test's: it reports
// rather than fails, because t.Fatalf outside the test goroutine does not stop
// the test and panics once it has finished.
func getQuiet(url string) (int, string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	code, body, err := getQuiet(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return code, body
}

func TestHealthzAnswersOnceServing(t *testing.T) {
	base := start(t, config.Default(), tiertest.NewMemory())

	code, body := get(t, base+"/healthz")
	if code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 (body %q)", code, body)
	}
}

func TestReadyzRunsBothChecks(t *testing.T) {
	var disk, bucket atomic.Int32
	base := start(t, config.Default(), tiertest.NewMemory(),
		server.WithDiskCheck(func(context.Context) error { disk.Add(1); return nil }),
		server.WithBucketCheck(func(context.Context) error { bucket.Add(1); return nil }),
	)

	if code, body := get(t, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200 (body %q)", code, body)
	}
	if disk.Load() != 1 || bucket.Load() != 1 {
		t.Fatalf("checks ran disk=%d bucket=%d, want 1 and 1", disk.Load(), bucket.Load())
	}
}

// A failing bucket must take the replica out of the load balancer, and the
// recovery must not be visible until the cached answer expires -- otherwise
// the cache is not doing anything and the bucket is probed on every kubelet
// tick, which is billed per request.
func TestReadyzFlipsOnBucketFailureAndBackAfterTheCacheWindow(t *testing.T) {
	const ttl = 150 * time.Millisecond

	var broken atomic.Bool
	broken.Store(true)
	base := start(t, config.Default(), tiertest.NewMemory(),
		server.WithDiskCheck(func(context.Context) error { return nil }),
		server.WithBucketCheck(func(context.Context) error {
			if broken.Load() {
				return errors.New("bucket is on fire")
			}
			return nil
		}),
		server.WithReadyCacheTTL(ttl),
	)

	code, body := get(t, base+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with a broken bucket = %d, want 503 (body %q)", code, body)
	}

	// Repaired, but inside the cache window: the answer must not change yet.
	broken.Store(false)
	if code, _ := get(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz inside the cache window = %d, want the cached 503", code)
	}

	waitForStatus(t, base+"/readyz", http.StatusOK, ttl)

	// And it flips the other way once the window has passed again.
	broken.Store(true)
	waitForStatus(t, base+"/readyz", http.StatusServiceUnavailable, ttl)
}

func waitForStatus(t *testing.T, url string, want int, poll time.Duration) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(poll / 2)
		code, body := get(t, url)
		if code == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s never reached %d: last %d (%q)", url, want, code, body)
		}
	}
}

// The probes must stay outside the concurrency gate. A cache that reports
// itself unready the moment it is busy gets pulled from the load balancer at
// its peak, which moves the load onto its siblings and takes them down too.
func TestProbesAreOutsideTheConcurrencyLimit(t *testing.T) {
	g := newGate()
	defer g.free()

	cfg := config.Default()
	cfg.Server.Concurrency = 1
	base := start(t, cfg, tiertest.NewMemory(),
		server.WithFrontends(&testFrontend{prefix: "/slow", name: "slow", h: g.Handler()}),
	)

	wg := g.hold(t, base, 1)
	if code, _ := get(t, base+"/slow"+probePath); code != http.StatusServiceUnavailable {
		t.Fatalf("an ordinary request while saturated = %d, want 503", code)
	}
	if code, _ := get(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz while saturated = %d, want 200", code)
	}
	if code, _ := get(t, base+"/readyz"); code != http.StatusOK {
		t.Errorf("/readyz while saturated = %d, want 200", code)
	}

	g.free()
	wg.Wait()
}

func TestOverConcurrencyLimitIs503(t *testing.T) {
	g := newGate()
	defer g.free()

	cfg := config.Default()
	cfg.Server.Concurrency = 2
	base := start(t, cfg, tiertest.NewMemory(),
		server.WithFrontends(&testFrontend{prefix: "/slow", name: "slow", h: g.Handler()}),
	)

	wg := g.hold(t, base, 2)

	code, body := get(t, base+"/slow"+probePath)
	if code != http.StatusServiceUnavailable {
		t.Errorf("request over the limit = %d, want 503 (body %q)", code, body)
	}
	if retry := headerOf(t, base+"/slow"+probePath, "Retry-After"); retry == "" {
		t.Error("a refusal carried no Retry-After; a client has nothing to back off by")
	}

	g.free()
	wg.Wait()
}

func headerOf(t *testing.T, url, name string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Header.Get(name)
}

func TestFrontendPrefixIsStripped(t *testing.T) {
	base := start(t, config.Default(), tiertest.NewMemory(),
		server.WithFrontends(&testFrontend{prefix: "/go/mod", name: "go/mod", h: echoPath()}),
	)

	code, body := get(t, base+"/go/mod/github.com/x/y/@v/list")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if want := "/github.com/x/y/@v/list"; body != want {
		t.Fatalf("handler saw %q, want %q", body, want)
	}

	// The bare prefix reaches the front-end too: answering 404 because of a
	// missing trailing slash is a bug that costs somebody an afternoon.
	if code, _ := get(t, base+"/go/mod"); code != http.StatusOK {
		t.Fatalf("bare prefix = %d, want 200", code)
	}
}

func TestDuplicatePrefixIsRefused(t *testing.T) {
	_, err := server.New(config.Default(), tiertest.NewMemory(),
		server.WithFrontends(
			&testFrontend{prefix: "/nix", name: "nix", h: echoPath()},
			&testFrontend{prefix: "/nix", name: "nix-again", h: echoPath()},
		),
	)
	if err == nil {
		t.Fatal("New accepted two front-ends on one prefix; ServeMux would have panicked at start-up")
	}
}

type testConnectFrontend struct{ name, path string }

func (f *testConnectFrontend) Name() string { return f.name }
func (f *testConnectFrontend) ConnectHandler() (string, http.Handler) {
	return f.path, echoPath()
}

// A ConnectFrontend's path is the service's own, so it must arrive at the
// handler unaltered: a generated client sends exactly that path, and stripping
// anything from it breaks every one of them.
func TestConnectFrontendPathIsNotStripped(t *testing.T) {
	base := start(t, config.Default(), tiertest.NewMemory(),
		server.WithConnectFrontends(&testConnectFrontend{name: "admin", path: "/admin.v1.Admin/"}),
	)

	code, body := get(t, base+"/admin.v1.Admin/Stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if want := "/admin.v1.Admin/Stats"; body != want {
		t.Fatalf("handler saw %q, want %q", body, want)
	}
}

func TestNewRefusesNilChain(t *testing.T) {
	if _, err := server.New(config.Default(), nil); err == nil {
		t.Fatal("New accepted a nil chain")
	}
}

// Shutdown must let a request that is already running finish. A cache that
// drops an upload at the last byte of a rollout has cost the build exactly the
// object it was about to be able to reuse.
func TestShutdownWaitsForInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})

	cfg := config.Default()
	cfg.Server.DrainTimeout = 5 * time.Second
	srv, err := server.New(cfg, tiertest.NewMemory(),
		server.WithFrontends(&testFrontend{
			prefix: "/slow",
			name:   "slow",
			h: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				<-finish
				_, _ = io.WriteString(w, "done")
			}),
		}),
	)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		_, body, err := getQuiet("http://" + ln.Addr().String() + "/slow/x")
		res <- result{body, err}
	}()
	<-started

	cancel()
	// The handler is still holding the request, so Serve must not have
	// returned: that is the difference between a drain and a hang-up.
	select {
	case err := <-done:
		t.Fatalf("Serve returned during the drain: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(finish)
	got := <-res
	if got.err != nil {
		t.Fatalf("the in-flight request failed during the drain: %v", got.err)
	}
	if got.body != "done" {
		t.Fatalf("client got %q, want the complete body", got.body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the drain")
	}
}

// Reflection must be reachable both at the root and under the cache service's
// prefix. A client is given the prefixed base URL, and grpcurl looks for
// reflection relative to whatever base it was given: mounted only at the root,
// it would report a server with no services at all.
func TestGRPCReflectionIsMountedAtBothBases(t *testing.T) {
	base := start(t, config.Default(), tiertest.NewMemory())

	for _, path := range []string{
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
		server.CachePrefix + "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		server.CachePrefix + "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		// A GET is the wrong method for a streaming procedure, so the handler
		// refuses it -- but only a handler that is mounted can refuse it. A 404
		// would mean the route is not there at all.
		code, _ := get(t, base+path)
		if code == http.StatusNotFound {
			t.Errorf("%s is not mounted", path)
		}
	}
}
