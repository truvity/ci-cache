// Package gomod serves the Go module proxy protocol at /go/mod.
//
// The protocol itself is goproxy's; what this package adds is the three things
// a shared CI proxy needs and a library cannot decide for you: where the
// objects live (a tier, so a runner's disk sits in front of a bucket), how
// long a moving answer may be believed, and what a client is told when the
// upstream proxy is down.
//
// That last one is the reason this is a wrapper and not a call to
// goproxy.ServeHTTP. goproxy reports an upstream that is refusing connections
// or answering 503 as 404. For a Go client a 404 is not a transient failure:
// it is the module proxy stating that the version does not exist, which the
// toolchain is entitled to remember and which turns a five-minute upstream
// outage into a day of "unknown revision" for everyone who built during it. A
// 502 says the same thing honestly and is retried.
package gomod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/goproxy/goproxy"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
)

// defaultUpstreamTimeout bounds one request's dealings with the upstream. It
// is a backstop and not a latency target: goproxy already retries a failing
// upstream with its own backoff, and this is what stops a build from waiting
// on a proxy that accepts connections and then says nothing at all.
const defaultUpstreamTimeout = 10 * time.Minute

// maxRecordedBody is how much of a failing upstream's response is kept to be
// passed on. An upstream that is broken enough to matter says so in a line or
// two; anything longer is a page nobody reads, and this runs once per failed
// request with the bytes held in memory.
const maxRecordedBody = 8 << 10

// maxGatedResponse bounds the buffer used while a cache-only attempt is in
// flight. Only the two moving answers -- the version list and @latest -- are
// ever attempted that way, and both are small; past this the response is
// committed to the client as it is, because it is no longer something worth
// holding on to in order to retry.
const maxGatedResponse = 4 << 20

// Proxy is the /go/mod front-end.
type Proxy struct {
	proxy   *goproxy.Goproxy
	cache   *cacher
	listTTL time.Duration
	timeout time.Duration
}

// Option configures a Proxy.
type Option func(*options)

type options struct {
	transport http.RoundTripper
	timeout   time.Duration
	log       *slog.Logger
}

// WithTransport sets the round tripper used for every upstream request, both
// the module proxy's and the checksum database's. An estate with an egress
// proxy or a private CA sets it here; the tests point it at a local server.
func WithTransport(rt http.RoundTripper) Option {
	return func(o *options) {
		if rt != nil {
			o.transport = rt
		}
	}
}

// WithUpstreamTimeout bounds how long one request may spend upstream.
func WithUpstreamTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// WithLogger sets the logger, which is also handed to goproxy so that its
// errors arrive in the same stream as everything else's.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.log = l
		}
	}
}

// New returns a Proxy serving cfg's upstream through the tier.
func New(t tier.Tier, cfg config.GoMod, opts ...Option) (*Proxy, error) {
	o := options{
		transport: http.DefaultTransport,
		timeout:   defaultUpstreamTimeout,
		log:       slog.Default().WithGroup("gomod"),
	}
	for _, opt := range opts {
		opt(&o)
	}

	if cfg.Upstream == "" {
		return nil, errors.New("gomod: frontends.go.mod.upstream is required")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gomod: frontends.go.mod.upstream %q is not an absolute URL", cfg.Upstream)
	}

	c := &cacher{
		tier:   t,
		legacy: strings.Trim(cfg.LegacyPrefix, "/"),
		log:    o.log,
	}

	// The transport is what sees an upstream failure as it happens: by the
	// time goproxy has turned it into an error, the status and the body are
	// gone.
	tr := &upstreamTransport{base: o.transport}

	// A checksum database may be named on its own ("sum.golang.org") or with
	// the URL to reach it at, which is how an estate points at a mirror. Only
	// the name goes to the fetcher: the URL is where this proxy fetches the
	// database from, not part of the identity it verifies against.
	sumdb := strings.TrimSpace(cfg.SumDB)
	sumdbName, _, _ := strings.Cut(sumdb, " ")
	var proxied []string
	if sumdb != "" && sumdbName != "off" {
		proxied = []string{sumdb}
	}

	p := &Proxy{
		proxy: &goproxy.Goproxy{
			// GOPROXY is a single URL with no "direct" after it, which is
			// what keeps this from ever shelling out to the go binary: a
			// server process that forks a toolchain to clone a repository is
			// a different, much larger thing than a proxy, and it is not this
			// one.
			Fetcher: &goproxy.GoFetcher{
				Env: []string{
					"GOPROXY=" + cfg.Upstream,
					"GOSUMDB=" + sumdbEnv(sumdbName),
					"GONOSUMDB=",
					"GONOPROXY=",
					"GOPRIVATE=",
				},
				Transport: tr,
			},
			ProxiedSumDBs: proxied,
			Cacher:        c,
			Transport:     tr,
			Logger:        o.log,
		},
		cache:   c,
		listTTL: cfg.ListTTL,
		timeout: o.timeout,
	}
	return p, nil
}

// Prefix is where the server mounts this front-end.
func (p *Proxy) Prefix() string { return "/" + Prefix }

// Name labels this front-end in metrics and in the GC's budgets.
func (p *Proxy) Name() string { return Prefix }

// Handler returns the handler, with the prefix already stripped.
func (p *Proxy) Handler() http.Handler { return http.HandlerFunc(p.serve) }

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	rec := &upstreamRecorder{}
	ctx = withRecorder(ctx, rec)
	req := r.Clone(ctx)

	name := strings.TrimPrefix(path.Clean(req.URL.Path), "/")

	// goproxy re-fetches a version list on every request and falls back to its
	// cache only when that fails, so the TTL cannot be enforced inside the
	// cacher: it is enforced here, by telling goproxy not to fetch when what
	// we hold is still fresh. Disable-Module-Fetch is goproxy's own documented
	// way of saying "answer from cache or not at all".
	if classify(name) == kindTTL && p.cache.fresh(ctx, name, p.listTTL) {
		req.Header.Set("Disable-Module-Fetch", "true")
		if p.serveGated(w, req, rec) {
			return
		}
		// The entry went away between the freshness check and the read. A 404
		// for a version list is an answer -- "this module has no versions" --
		// so rather than pass that on, drop the gate and let the request go
		// upstream as it would have without a cache at all.
		req.Header.Del("Disable-Module-Fetch")
		rec.reset()
	}

	p.proxy.ServeHTTP(&badGatewayWriter{ResponseWriter: w, rec: rec}, req)
}

// serveGated runs a cache-only attempt into a buffer and reports whether it
// answered. Nothing reaches the client unless it did, which is what makes the
// retry above safe.
func (p *Proxy) serveGated(w http.ResponseWriter, r *http.Request, rec *upstreamRecorder) bool {
	buf := &bufferedWriter{header: http.Header{}, limit: maxGatedResponse}
	p.proxy.ServeHTTP(buf, r)
	if buf.status == http.StatusNotFound && !buf.overflowed {
		return false
	}
	buf.flushTo(&badGatewayWriter{ResponseWriter: w, rec: rec})
	return true
}

// sumdbEnv is what the fetcher is told to verify against.
//
// Verification here is not the client's: the client checks the same sums
// through this proxy's sumdb/ paths. This one stops a compromised or simply
// confused upstream from filling the cache with bytes that do not match the
// checksum database, which every later reader would then be served from a
// cache that looks authoritative.
func sumdbEnv(sumdb string) string {
	if sumdb == "" {
		return "off"
	}
	return sumdb
}

// upstreamFailure is an upstream that could not answer, kept so that the
// client can be told what actually happened.
type upstreamFailure struct {
	host   string
	status int
	body   []byte
	err    error
}

func (f *upstreamFailure) message() []byte {
	var b strings.Builder
	if f.err != nil {
		fmt.Fprintf(&b, "ci-cache: upstream %s is unreachable: %v\n", f.host, f.err)
		return []byte(b.String())
	}
	fmt.Fprintf(&b, "ci-cache: upstream %s returned %d\n", f.host, f.status)
	if len(f.body) > 0 {
		b.Write(bytes.TrimRight(f.body, "\n"))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

type upstreamRecorder struct {
	mu      sync.Mutex
	failure *upstreamFailure
}

func (r *upstreamRecorder) record(f *upstreamFailure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The first failure is the one kept: goproxy retries, and the last
	// attempt is usually a context deadline that says nothing about why the
	// upstream was unhappy in the first place.
	if r.failure == nil {
		r.failure = f
	}
}

func (r *upstreamRecorder) take() *upstreamFailure {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.failure
	r.failure = nil
	return f
}

func (r *upstreamRecorder) reset() { r.take() }

type recorderKey struct{}

func withRecorder(ctx context.Context, r *upstreamRecorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

func recorderFrom(ctx context.Context) *upstreamRecorder {
	r, _ := ctx.Value(recorderKey{}).(*upstreamRecorder)
	return r
}

// upstreamTransport records why an upstream request failed.
//
// It rides on the request context, which works because every upstream request
// goproxy makes is built from the incoming request's context. A transport that
// kept the failure in a field instead would hand one request's outage to
// another request's client the moment two builds arrive at once.
type upstreamTransport struct {
	base http.RoundTripper
}

func (t *upstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := recorderFrom(req.Context())
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		if rec != nil {
			rec.record(&upstreamFailure{host: req.URL.Host, err: err})
		}
		return nil, err
	}

	// Only the statuses that mean "the upstream is having trouble" are
	// recorded. A 404 or a 410 is the upstream answering the question -- this
	// module or version does not exist -- and passing that through unchanged
	// is the whole reason the toolchain can cache a negative result at all.
	if rec != nil && (resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests) {
		head, rerr := io.ReadAll(io.LimitReader(resp.Body, maxRecordedBody))
		if rerr == nil {
			// The body has to be put back: goproxy reads it too, and a proxy
			// that consumed the upstream's response to log it would turn a
			// retryable failure into an empty one.
			resp.Body = &rewoundBody{Reader: io.MultiReader(bytes.NewReader(head), resp.Body), closer: resp.Body}
		}
		rec.record(&upstreamFailure{host: req.URL.Host, status: resp.StatusCode, body: head})
	}
	return resp, nil
}

type rewoundBody struct {
	io.Reader
	closer io.Closer
}

func (b *rewoundBody) Close() error { return b.closer.Close() }

// badGatewayWriter turns goproxy's verdict into an honest one when the
// recorder says the upstream, not the module, was the problem.
type badGatewayWriter struct {
	http.ResponseWriter
	rec *upstreamRecorder

	wroteHeader bool
	swallow     bool
}

func (w *badGatewayWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	// A 2xx is a success even if an upstream failed on the way: that is
	// goproxy answering from this cache, which is exactly what a cache is
	// for, so the recorded failure is dropped and the client never hears
	// about it.
	switch status {
	case http.StatusNotFound, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		w.rec.take()
		w.ResponseWriter.WriteHeader(status)
		return
	}

	f := w.rec.take()
	if f == nil {
		w.ResponseWriter.WriteHeader(status)
		return
	}

	w.swallow = true
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	// Nothing may cache this. It is a statement about a minute of this
	// upstream's life, not about the module.
	h.Set("Cache-Control", "must-revalidate, no-cache, no-store")
	h.Del("Content-Length")
	w.ResponseWriter.WriteHeader(http.StatusBadGateway)
	if _, err := w.ResponseWriter.Write(f.message()); err != nil {
		return
	}
}

func (w *badGatewayWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallow {
		// goproxy's own body for the status that was replaced. Reporting the
		// full length keeps it writing happily into the void rather than
		// treating this as a short write.
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (w *badGatewayWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// bufferedWriter holds a response until it is known to be worth sending.
type bufferedWriter struct {
	header     http.Header
	status     int
	body       bytes.Buffer
	limit      int
	overflowed bool
	committed  bool
}

func (w *bufferedWriter) Header() http.Header { return w.header }

func (w *bufferedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(p) > w.limit {
		w.overflowed = true
	}
	return w.body.Write(p)
}

func (w *bufferedWriter) flushTo(dst http.ResponseWriter) {
	if w.committed {
		return
	}
	w.committed = true
	for k, vs := range w.header {
		for _, v := range vs {
			dst.Header().Add(k, v)
		}
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	dst.WriteHeader(status)
	if _, err := dst.Write(w.body.Bytes()); err != nil {
		return
	}
}
