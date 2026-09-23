package gomod

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/engine/tier/tiertest"
)

const (
	testModule  = "example.com/m"
	testVersion = "v1.0.0"
	zipName     = testModule + "/@v/" + testVersion + ".zip"
	listName    = testModule + "/@v/list"
)

// frontend is the shape server/ mounts. It is restated here rather than
// imported so that these tests do not wait on that package, and so that a
// change to either side shows up as a compile failure here.
type frontend interface {
	Prefix() string
	Handler() http.Handler
	Name() string
}

var _ frontend = (*Proxy)(nil)

func TestFrontendShape(t *testing.T) {
	p := newProxy(t, tiertest.NewMemory(), config.GoMod{Upstream: "https://proxy.golang.org", SumDB: "off"})
	if got, want := p.Prefix(), "/go/mod"; got != want {
		t.Errorf("Prefix() = %q, want %q", got, want)
	}
	if got, want := p.Name(), "go/mod"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if p.Handler() == nil {
		t.Error("Handler() = nil")
	}
}

func TestNewRejectsUnusableUpstream(t *testing.T) {
	for _, upstream := range []string{"", "proxy.golang.org", "/local"} {
		if _, err := New(tiertest.NewMemory(), config.GoMod{Upstream: upstream}); err == nil {
			t.Errorf("New with upstream %q: want an error", upstream)
		}
	}
}

// TestModuleFileIsCachedForever is the point of the whole front-end: the
// second build of the day must not touch the upstream proxy at all.
func TestModuleFileIsCachedForever(t *testing.T) {
	up := newUpstream(t)
	mem := tiertest.NewMemory()
	h := newProxy(t, mem, config.GoMod{Upstream: up.URL(), SumDB: "off", ListTTL: time.Minute}).Handler()

	body, status := do(t, h, "/"+zipName)
	if status != http.StatusOK {
		t.Fatalf("first fetch: status = %d, body = %q", status, body)
	}
	if !bytes.Equal(body, up.zip) {
		t.Fatalf("first fetch: body = %d bytes, want the upstream zip (%d bytes)", len(body), len(up.zip))
	}
	// One download fetches all three files of the version, which is goproxy
	// validating the zip against the go.mod it belongs to.
	if n := up.count("/" + zipName); n != 1 {
		t.Fatalf("upstream zip requests = %d, want 1", n)
	}

	before := up.total()
	body2, status2 := do(t, h, "/"+zipName)
	if status2 != http.StatusOK || !bytes.Equal(body2, up.zip) {
		t.Fatalf("second fetch: status = %d, %d bytes", status2, len(body2))
	}
	if after := up.total(); after != before {
		t.Errorf("second fetch made %d upstream requests, want none", after-before)
	}

	// The .info and .mod of the same version were cached by the same
	// download, so they are free too.
	before = up.total()
	if _, status := do(t, h, "/"+testModule+"/@v/"+testVersion+".mod"); status != http.StatusOK {
		t.Errorf(".mod status = %d", status)
	}
	if after := up.total(); after != before {
		t.Errorf(".mod made %d upstream requests, want none", after-before)
	}
}

// TestListRefetchesAfterTTL is the other half: a version list is a snapshot of
// something that moves, so it is believed for the TTL and no longer.
func TestListRefetchesAfterTTL(t *testing.T) {
	up := newUpstream(t)
	mem := tiertest.NewMemory()
	const ttl = 150 * time.Millisecond
	h := newProxy(t, mem, config.GoMod{Upstream: up.URL(), SumDB: "off", ListTTL: ttl}).Handler()

	if body, status := do(t, h, "/"+listName); status != http.StatusOK || !strings.Contains(string(body), testVersion) {
		t.Fatalf("first list: status = %d, body = %q", status, body)
	}
	if n := up.count("/" + listName); n != 1 {
		t.Fatalf("upstream list requests = %d, want 1", n)
	}

	if _, status := do(t, h, "/"+listName); status != http.StatusOK {
		t.Fatalf("second list: status = %d", status)
	}
	if n := up.count("/" + listName); n != 1 {
		t.Errorf("upstream list requests = %d while the entry was fresh, want 1", n)
	}

	time.Sleep(ttl + 50*time.Millisecond)
	if _, status := do(t, h, "/"+listName); status != http.StatusOK {
		t.Fatalf("third list: status = %d", status)
	}
	if n := up.count("/" + listName); n != 2 {
		t.Errorf("upstream list requests = %d after the TTL expired, want 2", n)
	}
}

// TestZeroTTLAlwaysRefetches documents what an unset TTL means: goproxy's own
// behaviour, where the cache is a fallback and not an answer.
func TestZeroTTLAlwaysRefetches(t *testing.T) {
	up := newUpstream(t)
	h := newProxy(t, tiertest.NewMemory(), config.GoMod{Upstream: up.URL(), SumDB: "off"}).Handler()

	do(t, h, "/"+listName)
	do(t, h, "/"+listName)
	if n := up.count("/" + listName); n != 2 {
		t.Errorf("upstream list requests = %d, want 2", n)
	}
}

// TestUpstreamDownCachedResolves covers the outage: what is cached is served,
// and what is not says so with a status the toolchain retries.
func TestUpstreamDownCachedResolves(t *testing.T) {
	up := newUpstream(t)
	mem := tiertest.NewMemory()
	h := newProxy(t, mem, config.GoMod{Upstream: up.URL(), SumDB: "off", ListTTL: time.Minute}).Handler()

	if _, status := do(t, h, "/"+zipName); status != http.StatusOK {
		t.Fatalf("warming the cache: status = %d", status)
	}
	up.Close()

	body, status := do(t, h, "/"+zipName)
	if status != http.StatusOK || !bytes.Equal(body, up.zip) {
		t.Errorf("cached zip with the upstream down: status = %d, %d bytes", status, len(body))
	}

	body, status = do(t, h, "/example.com/other/@v/v1.0.0.zip")
	if status != http.StatusBadGateway {
		t.Errorf("uncached module with the upstream down: status = %d, want 502; body = %q", status, body)
	}
	if !strings.Contains(string(body), "unreachable") {
		t.Errorf("body = %q, want it to name the upstream failure", body)
	}
	if cc := "no-store"; !strings.Contains(headerOf(t, h, "/example.com/other/@v/v1.0.0.zip", "Cache-Control"), cc) {
		t.Errorf("a 502 must not be cacheable")
	}
}

// TestStaleListSurvivesAnOutage is why the TTL is enforced before goproxy and
// not inside the cacher: once the upstream cannot be asked, yesterday's list
// is a far better answer than none.
func TestStaleListSurvivesAnOutage(t *testing.T) {
	up := newUpstream(t)
	const ttl = 50 * time.Millisecond
	h := newProxy(t, tiertest.NewMemory(), config.GoMod{Upstream: up.URL(), SumDB: "off", ListTTL: ttl}).Handler()

	if _, status := do(t, h, "/"+listName); status != http.StatusOK {
		t.Fatalf("warming the cache: status = %d", status)
	}
	time.Sleep(ttl + 20*time.Millisecond)
	up.Close()

	body, status := do(t, h, "/"+listName)
	if status != http.StatusOK || !strings.Contains(string(body), testVersion) {
		t.Errorf("stale list with the upstream down: status = %d, body = %q", status, body)
	}
}

// TestUpstreamServerErrorBecomesBadGateway carries the upstream's own words
// through, because "503 scheduled maintenance" is the thing a person needs to
// see in a failed build's log.
func TestUpstreamServerErrorBecomesBadGateway(t *testing.T) {
	up := newUpstream(t)
	up.fail(http.StatusServiceUnavailable, "the module mirror is down for maintenance")
	h := newProxy(t, tiertest.NewMemory(), config.GoMod{Upstream: up.URL(), SumDB: "off"}).Handler()

	body, status := do(t, h, "/"+zipName)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %q", status, body)
	}
	if !strings.Contains(string(body), "maintenance") {
		t.Errorf("body = %q, want the upstream's own message", body)
	}
	if !strings.Contains(string(body), "503") {
		t.Errorf("body = %q, want the upstream's status", body)
	}
}

// TestUpstreamNotFoundStaysNotFound is the invariant the 502 rewriting must
// not break. A 404 here is the upstream answering the question, and the
// toolchain is entitled to remember it.
func TestUpstreamNotFoundStaysNotFound(t *testing.T) {
	up := newUpstream(t)
	h := newProxy(t, tiertest.NewMemory(), config.GoMod{Upstream: up.URL(), SumDB: "off"}).Handler()

	body, status := do(t, h, "/example.com/nothing/@v/v1.0.0.info")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %q", status, body)
	}
}

// TestLegacyPrefix serves an object written by the tool this replaces, whose
// module cache named objects by a hash of goproxy's name.
func TestLegacyPrefix(t *testing.T) {
	up := newUpstream(t)
	mem := tiertest.NewMemory()
	sum := sha256.Sum256([]byte(zipName))
	h := hex.EncodeToString(sum[:])
	seed := []byte("pretend this is a module zip")
	mustPut(t, mem, "old/modcache/"+h[:2]+"/"+h, seed)

	handler := newProxy(t, mem, config.GoMod{
		Upstream:     up.URL(),
		SumDB:        "off",
		LegacyPrefix: "old/modcache",
	}).Handler()

	body, status := do(t, handler, "/"+zipName)
	if status != http.StatusOK || !bytes.Equal(body, seed) {
		t.Fatalf("legacy hit: status = %d, body = %q", status, body)
	}
	if n := up.total(); n != 0 {
		t.Errorf("upstream requests = %d, want none", n)
	}
	// The read path does not copy into the natural layout, for the same
	// reason the build cache's does not: a migration that moves bytes is a
	// job for the admin API, not for a request.
	if _, err := mem.Stat(context.Background(), Prefix+"/"+zipName); err == nil {
		t.Error("the read path wrote the natural key; reads must not write")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		want entryKind
	}{
		{"example.com/m/@v/v1.0.0.zip", kindImmutable},
		{"example.com/m/@v/v1.0.0.info", kindImmutable},
		{"example.com/m/@v/v1.0.0.mod", kindImmutable},
		{"example.com/m/@v/v0.0.0-20230101000000-abcdefabcdef.info", kindImmutable},
		{"example.com/m/@v/v2.1.0+incompatible.zip", kindImmutable},
		{"example.com/m/@v/list", kindTTL},
		{"example.com/m/@latest", kindTTL},
		{"example.com/m/@v/main.info", kindTTL},
		{"example.com/m/@v/v1.info", kindTTL},
		{"sumdb/sum.golang.org/lookup/example.com/m@v1.0.0", kindImmutable},
		{"sumdb/sum.golang.org/tile/8/0/x123/456", kindImmutable},
		{"sumdb/sum.golang.org/latest", kindTTL},
	}
	for _, tc := range cases {
		if got := classify(tc.name); got != tc.want {
			t.Errorf("classify(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"", "/abs", "a//b", "a/../b", "trailing/"} {
		if err := validName(name); err == nil {
			t.Errorf("validName(%q) = nil, want an error", name)
		}
	}
	if err := validName("example.com/m/@v/v1.0.0.zip"); err != nil {
		t.Errorf("validName of an ordinary name: %v", err)
	}
}

// TestCacheFailureIsAMiss: a sick tier must cost a round trip, not a build.
func TestCacheFailureIsAMiss(t *testing.T) {
	up := newUpstream(t)
	mem := tiertest.NewMemory()
	mem.BeforeGet = func(_ context.Context, _ string) error {
		return errors.New("the volume is gone")
	}
	h := newProxy(t, mem, config.GoMod{Upstream: up.URL(), SumDB: "off"}).Handler()

	if body, status := do(t, h, "/"+zipName); status != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %q", status, body)
	}
}

func newProxy(t *testing.T, mem tier.Tier, cfg config.GoMod) *Proxy {
	t.Helper()
	p, err := New(mem, cfg,
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		// A dead upstream is retried by goproxy with its own backoff; without
		// a bound each of these tests would spend seconds proving it.
		WithUpstreamTimeout(300*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func do(t *testing.T, h http.Handler, target string) ([]byte, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Body.Bytes(), rec.Code
}

func headerOf(t *testing.T, h http.Handler, target, key string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Header().Get(key)
}

func mustPut(t *testing.T, mem *tiertest.Memory, key string, body []byte) {
	t.Helper()
	mem.Seed(key, body, tier.Meta{})
}

// upstream is a module proxy holding exactly one version of one module, which
// is all it takes to tell a cached answer from a fetched one.
type upstream struct {
	srv *httptest.Server
	zip []byte

	mu       sync.Mutex
	requests map[string]int
	status   int
	body     string
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{requests: map[string]int{}, zip: moduleZip(t)}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) URL() string { return u.srv.URL }

func (u *upstream) Close() { u.srv.Close() }

// fail makes every later request answer with a status that means the upstream,
// not the module, is the problem.
func (u *upstream) fail(status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.body = status, body
}

func (u *upstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[path]
}

func (u *upstream) total() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	var n int
	for _, c := range u.requests {
		n += c
	}
	return n
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.requests[r.URL.Path]++
	status, body := u.status, u.body
	u.mu.Unlock()

	if status != 0 {
		http.Error(w, body, status)
		return
	}

	switch r.URL.Path {
	case "/" + listName:
		_, _ = fmt.Fprintf(w, "%s\n", testVersion)
	case "/" + testModule + "/@latest", "/" + testModule + "/@v/" + testVersion + ".info":
		_, _ = fmt.Fprintf(w, `{"Version":%q,"Time":"2023-01-01T00:00:00Z"}`, testVersion)
	case "/" + testModule + "/@v/" + testVersion + ".mod":
		_, _ = fmt.Fprintf(w, "module %s\n\ngo 1.21\n", testModule)
	case "/" + zipName:
		_, _ = w.Write(u.zip)
	default:
		http.Error(w, "not found: unknown module", http.StatusNotFound)
	}
}

// moduleZip builds the smallest thing goproxy will accept as a module: every
// file under <module>@<version>/, and a go.mod that names the module.
func moduleZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := testModule + "@" + testVersion + "/"
	files := map[string]string{
		prefix + "go.mod":   fmt.Sprintf("module %s\n\ngo 1.21\n", testModule),
		prefix + "hello.go": "package m\n\nfunc Hello() string { return \"hello\" }\n",
	}
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestSumDBIsProxiedAndStoredImmutably covers the checksum database: goproxy
// re-fetches a sumdb path every time, so what the cache buys is the outage --
// a lookup already seen still resolves when the database cannot be reached.
func TestSumDBIsProxiedAndStoredImmutably(t *testing.T) {
	const lookup = "sumdb/sum.golang.org/lookup/example.com/m@v1.0.0"
	const record = "example.com/m v1.0.0 h1:deadbeef=\n"

	sumdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lookup/example.com/m@v1.0.0" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, record)
	}))
	t.Cleanup(sumdb.Close)

	up := newUpstream(t)
	mem := tiertest.NewMemory()
	h := newProxy(t, mem, config.GoMod{
		Upstream: up.URL(),
		SumDB:    "sum.golang.org " + sumdb.URL,
	}).Handler()

	body, status := do(t, h, "/"+lookup)
	if status != http.StatusOK || string(body) != record {
		t.Fatalf("lookup: status = %d, body = %q", status, body)
	}

	m, err := mem.Stat(context.Background(), Prefix+"/"+lookup)
	if err != nil {
		t.Fatalf("lookup was not cached: %v", err)
	}
	if !m.Immutable {
		t.Error("a signed tree record was stored as mutable")
	}

	sumdb.Close()
	body, status = do(t, h, "/"+lookup)
	if status != http.StatusOK || string(body) != record {
		t.Errorf("cached lookup with the database down: status = %d, body = %q", status, body)
	}
}
