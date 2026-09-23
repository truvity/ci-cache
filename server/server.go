// Package server is the one listener every front-end hangs off.
//
// There is deliberately no web framework here. The cache's front-ends are
// net/http handlers and nothing else will do: ConnectRPC's generated handlers,
// the Go module proxy's goproxy.Server and every upstream proxy built on
// httputil.ReverseProxy all implement http.Handler, and a faster router that
// cannot host them would mean reimplementing all three. fasthttp is the usual
// suggestion and is the wrong one twice over -- it has its own request type,
// so none of the above can be mounted on it, and it speaks neither HTTP/2 nor
// streaming response bodies. Those are the two properties a byte-serving cache
// needs most: gRPC over h2c between an agent and this service, and a response
// that starts arriving before the object has been read. Routing was never the
// bottleneck; copying bytes is.
//
// One listener carries all three protocols. net/http in Go 1.24 and later
// accepts unencrypted HTTP/2 itself, by sniffing the client preface before it
// commits to HTTP/1.1, so the h2c wrapper that used to be mandatory is not
// imported here.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/gen/cache/v1/cachev1connect"
)

// CachePrefix is where the cache.v1.Cache service is mounted.
//
// Connect already puts the service name in the path, so the prefix merely
// precedes it and the full route is /go/build/cache.v1.Cache/<Method>. It is
// exported because an agent's remote tier has to be pointed at exactly this
// string and guessing it in two places is how they drift apart.
const CachePrefix = "/go/build"

// Frontend is a protocol served under a path prefix.
//
// The server knows nothing about what a front-end does; it knows where to
// mount it, what to call it in metrics, and that the handler will be given
// paths with the prefix already removed. That last part is what lets a
// front-end be written as though it owned the root, and tested that way too.
type Frontend interface {
	// Prefix is the path this front-end is mounted at, e.g. "/go/mod".
	Prefix() string
	// Handler serves requests with the prefix already stripped.
	Handler() http.Handler
	// Name is the metrics label, e.g. "go/mod".
	Name() string
}

// ConnectFrontend is a front-end that serves a Connect or gRPC service.
//
// It is separate from Frontend because an RPC service's path is not a
// deployment choice: it is the fully-qualified service name, and a client
// generated from the same proto will send exactly that. Stripping a prefix
// from it, as Frontend's contract does, would break every generated client.
type ConnectFrontend interface {
	Name() string
	ConnectHandler() (path string, h http.Handler)
}

// Option configures a Server.
type Option func(*Server)

// WithFrontends mounts path-prefixed front-ends.
func WithFrontends(fs ...Frontend) Option {
	return func(s *Server) { s.frontends = append(s.frontends, fs...) }
}

// WithConnectFrontends mounts RPC services at their own canonical paths.
func WithConnectFrontends(fs ...ConnectFrontend) Option {
	return func(s *Server) { s.connectFrontends = append(s.connectFrontends, fs...) }
}

// WithDiskCheck registers the readiness probe's disk half.
//
// It is a function rather than a *disk.Tier so that this package does not
// import the disk tier -- or the bucket. The server would then depend on every
// storage backend in the repository in order to answer one HTTP route, and a
// test of the route would have to construct a bucket.
func WithDiskCheck(f func(context.Context) error) Option {
	return func(s *Server) { s.ready.disk = f }
}

// WithBucketCheck registers the readiness probe's bucket half.
func WithBucketCheck(f func(context.Context) error) Option {
	return func(s *Server) { s.ready.bucket = f }
}

// WithReadyCacheTTL overrides how long a readiness result is reused.
//
// The default is ReadyCacheTTL. It is adjustable because a test that waits ten
// seconds to watch a probe recover is a test nobody runs.
func WithReadyCacheTTL(d time.Duration) Option {
	return func(s *Server) { s.ready.ttl = d }
}

// WithReadyTimeout overrides the budget the two readiness checks share.
func WithReadyTimeout(d time.Duration) Option {
	return func(s *Server) { s.ready.budget = d }
}

// WithLogger sets the logger. The default discards, so that a test does not
// have to silence the package to read its own output.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.log = l }
}

// WithConnectOptions adds handler options to the cache service -- an
// interceptor for tracing, say. They apply to the service this package mounts,
// not to a ConnectFrontend, which builds its own handler.
func WithConnectOptions(opts ...connect.HandlerOption) Option {
	return func(s *Server) { s.connectOpts = append(s.connectOpts, opts...) }
}

const (
	// ReadyCacheTTL is how long a readiness answer is reused. Kubernetes
	// probes every few seconds and there may be several replicas; without a
	// cache, a liveness regime turns into a steady stream of HEAD requests at
	// the bucket, which is billed per request.
	ReadyCacheTTL = 10 * time.Second

	// ReadyTimeout is the budget the disk and bucket checks share. It is below
	// any sane probe timeout on purpose: a readiness handler that hangs is
	// reported as a failure by the kubelet anyway, but only after the kubelet's
	// own timeout, and in the meantime the goroutines pile up.
	ReadyTimeout = 2 * time.Second

	// defaultConcurrency matches config.Default. It is repeated here because
	// New is also called with a zero Config in tests, and a zero limit would
	// mean the server refuses everything.
	defaultConcurrency = 256

	// defaultDrainTimeout matches config.Default, for the same reason.
	defaultDrainTimeout = 30 * time.Second
)

// Server is the data listener: the mux, its middleware and the graceful
// shutdown around it.
type Server struct {
	cfg   config.Config
	chain tier.Tier
	log   *slog.Logger

	frontends        []Frontend
	connectFrontends []ConnectFrontend
	connectOpts      []connect.HandlerOption

	handler http.Handler
	sem     chan struct{}
	ready   readiness

	// serving is closed once the listener is up. /healthz answers 200 from
	// that moment and not before: a probe that passes while the port is still
	// closed teaches the orchestrator to route traffic at nothing.
	serving  chan struct{}
	servedMu sync.Mutex

	mu       sync.Mutex
	httpSrv  *http.Server
	listener net.Listener
}

// New builds the server. It does not listen; ListenAndServe does.
func New(cfg config.Config, chain tier.Tier, opts ...Option) (*Server, error) {
	if chain == nil {
		return nil, errors.New("server: nil chain")
	}
	s := &Server{
		cfg:     cfg,
		chain:   chain,
		log:     slog.New(slog.DiscardHandler),
		serving: make(chan struct{}),
		ready:   readiness{ttl: ReadyCacheTTL, budget: ReadyTimeout},
	}
	for _, o := range opts {
		o(s)
	}

	s.sem = make(chan struct{}, concurrencyLimit(cfg))

	h, err := s.buildHandler()
	if err != nil {
		return nil, err
	}
	s.handler = h
	return s, nil
}

// Handler returns the mux with its middleware, for a caller that wants to
// drive the listener itself.
//
// Note that HTTP/2 without TLS is a property of the http.Server, not of the
// handler: a caller that serves this on its own must set Protocols with
// SetUnencryptedHTTP2, or gRPC clients will fail the handshake against an
// HTTP/1.1-only listener. ListenAndServe does it for you.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) buildHandler() (http.Handler, error) {
	mux := http.NewServeMux()

	// Health first, so that a front-end registered at "/" cannot swallow the
	// probes. ServeMux prefers the more specific pattern, but the ordering
	// also says what is deliberate.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	mounted := map[string]string{
		"/healthz": "health",
		"/readyz":  "health",
	}
	claim := func(path, by string) error {
		if prev, ok := mounted[path]; ok {
			return fmt.Errorf("server: %q is claimed by both %s and %s", path, prev, by)
		}
		mounted[path] = by
		return nil
	}

	// The chain is served over the wire before any front-end is considered:
	// this service is what makes a runner's agent a tier of this server's
	// chain, and it exists whether or not the HTTP-facing Go front-ends are
	// switched on for this installation.
	svcPath, svcHandler := cachev1connect.NewCacheServiceHandler(
		&cacheService{chain: s.chain, log: s.log},
		s.connectOpts...,
	)
	full := CachePrefix + svcPath
	if err := claim(full, cachev1connect.CacheServiceName); err != nil {
		return nil, err
	}
	mux.Handle(full, http.StripPrefix(CachePrefix, svcHandler))
	services := []string{cachev1connect.CacheServiceName}

	for _, f := range s.connectFrontends {
		path, h := f.ConnectHandler()
		if path == "" || !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("server: connect front-end %s returned path %q, want an absolute path", f.Name(), path)
		}
		if err := claim(path, f.Name()); err != nil {
			return nil, err
		}
		mux.Handle(path, h)
		// The canonical path IS the service name between slashes, which is
		// what reflection has to advertise; the front-end's Name is a metrics
		// label and may be anything.
		if name := strings.Trim(path, "/"); strings.Contains(name, ".") {
			services = append(services, name)
		}
	}

	for _, f := range s.frontends {
		prefix := strings.TrimSuffix(f.Prefix(), "/")
		if prefix == "" || !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("server: front-end %s returned prefix %q, want something like \"/go/mod\"", f.Name(), prefix)
		}
		// Both the bare prefix and everything under it: a client that asks for
		// /nix without the slash is asking this front-end, and answering 404
		// because of a trailing character is the kind of bug that costs an
		// afternoon.
		if err := claim(prefix+"/", f.Name()); err != nil {
			return nil, err
		}
		h := http.StripPrefix(prefix, f.Handler())
		mux.Handle(prefix+"/", h)
		mux.Handle(prefix, h)
	}

	// gRPC reflection, so that grpcurl and the like can talk to this service
	// without a copy of the protos. It is registered both at the root and under
	// the cache service's prefix: a client pointed at the prefixed base URL --
	// which is the address anything speaking to this cache is given -- looks for
	// reflection relative to that base and finds nothing at the root.
	if err := s.mountReflection(mux, claim, services); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(mounted))
	for path, by := range mounted {
		names = append(names, path+" -> "+by)
	}
	sort.Strings(names)
	s.log.Debug("routes mounted", "routes", names)

	return s.limit(mux), nil
}

// mountReflection registers the gRPC reflection service for everything mounted.
//
// Both the v1 and the v1alpha handlers are registered because the two are
// still both in use: grpcurl asks for v1 and falls back, while several older
// tools only know v1alpha, and a server that offers one of them looks to the
// other like a server with no reflection at all.
func (s *Server) mountReflection(mux *http.ServeMux, claim func(path, by string) error, services []string) error {
	reflector := grpcreflect.NewStaticReflector(services...)
	v1Path, v1Handler := grpcreflect.NewHandlerV1(reflector)
	alphaPath, alphaHandler := grpcreflect.NewHandlerV1Alpha(reflector)

	for _, prefix := range []string{"", CachePrefix} {
		for path, h := range map[string]http.Handler{
			prefix + v1Path:    http.StripPrefix(prefix, v1Handler),
			prefix + alphaPath: http.StripPrefix(prefix, alphaHandler),
		} {
			if err := claim(path, "grpc reflection"); err != nil {
				return err
			}
			mux.Handle(path, h)
		}
	}
	return nil
}

// limit is the concurrency gate.
//
// Refusing with 503 rather than queueing is the point: a cache that queues
// turns a slow bucket into a slow build, and the client -- whose whole reason
// to exist is that it can rebuild the object itself -- would rather be told no
// immediately. ConnectRPC maps HTTP 503 to CodeUnavailable for the Connect,
// gRPC and gRPC-Web protocols alike, so one plain HTTP response is correct for
// an RPC caller and a curl alike, and no per-protocol error encoding is needed
// here.
func (s *Server) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The probes are outside the limit. A saturated cache is healthy and
		// ready; reporting otherwise would have the orchestrator pull the
		// replica out of the load balancer exactly when it is busiest, which
		// moves the load to its siblings and takes them out too.
		switch r.URL.Path {
		case "/healthz", "/readyz":
			next.ServeHTTP(w, r)
			return
		}

		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "ci-cache: over concurrency limit", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	select {
	case <-s.serving:
	default:
		// Before the listener is up this route is unreachable from outside, so
		// this only fires for an in-process caller holding Handler(). Saying
		// so beats a 200 that means nothing.
		http.Error(w, "not serving", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.ready.check(r.Context()); err != nil {
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

// ListenAndServe serves until ctx is cancelled, then drains.
func (s *Server) ListenAndServe(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.cfg.Service.Data)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on an existing listener until ctx is cancelled, then drains.
//
// It is exported for the same reason httptest exists: a test that binds port
// zero and asks the kernel which port it got has no race with anything, and a
// test that waits for a fixed port to open has one every time.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler: s.handler,
		// No WriteTimeout and no ReadTimeout. Both are wall-clock deadlines on
		// a whole request, and this server's whole job is requests that take
		// as long as the object is big; the idle timeouts below bound a
		// connection that has stopped making progress, which is the thing we
		// actually want bounded.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// The serve context's VALUES reach every handler, but not its
		// cancellation: a request context that is cancelled the instant the
		// shutdown signal arrives would abort exactly the in-flight uploads the
		// drain below exists to let finish.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		ErrorLog:    slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	// One listener, three protocols. HTTP/1.1 for the proxying front-ends, and
	// HTTP/2 both with and without TLS -- the cleartext half is what gRPC
	// between a runner's agent and this service rides on, since inside a
	// cluster there is no certificate on the data port.
	protos := new(http.Protocols)
	protos.SetHTTP1(true)
	protos.SetHTTP2(true)
	protos.SetUnencryptedHTTP2(true)
	srv.Protocols = protos
	srv.HTTP2 = &http.HTTP2Config{
		// The default of 250 is below the concurrency limit, and an agent
		// multiplexes every one of a build's parallel actions onto one
		// connection. Streams over the limit would silently queue in the
		// HTTP/2 layer instead of being refused by the gate above, which turns
		// a refusal the client knows how to handle into a stall it does not.
		MaxConcurrentStreams: concurrencyLimit(s.cfg) * 2,
	}

	s.mu.Lock()
	s.httpSrv = srv
	s.listener = ln
	s.mu.Unlock()

	s.markServing()

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	if err := s.Shutdown(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	// Serve has returned ErrServerClosed by now; draining the channel keeps the
	// goroutine from outliving this call, which is what a leak check sees.
	<-errCh
	return nil
}

func (s *Server) markServing() {
	s.servedMu.Lock()
	defer s.servedMu.Unlock()
	select {
	case <-s.serving:
	default:
		close(s.serving)
	}
}

// Shutdown stops accepting and waits for in-flight requests, up to
// cfg.Server.DrainTimeout.
//
// The drain timeout is applied here rather than left to the caller's context
// because the deadline belongs to the deployment: it is how long the pod's
// terminationGracePeriod gives us, and a caller that passes context.Background
// should still stop rather than hold a build's last upload open forever.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}

	drain := s.cfg.Server.DrainTimeout
	if drain <= 0 {
		drain = defaultDrainTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, drain)
	defer cancel()

	err := srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		// The drain ran out with requests still open. Closing is the honest
		// outcome: the alternative is a pod that ignores its grace period and
		// is killed with SIGKILL anyway, having told nobody.
		s.log.Warn("drain timed out, closing connections", "timeout", drain)
		return srv.Close()
	}
	return err
}

// Addr reports the address the server is listening on, or the empty string
// before it listens. A test that asked for port zero learns its port here.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// readiness answers "would this replica serve a request correctly", and
// remembers the answer.
type readiness struct {
	disk   func(context.Context) error
	bucket func(context.Context) error

	ttl    time.Duration
	budget time.Duration

	mu        sync.Mutex
	checkedAt time.Time
	err       error
	// inflight collapses concurrent probes onto one run. Several replicas
	// probing at once is normal; one replica running the bucket check twice
	// because two probes arrived together is waste.
	inflight *sync.WaitGroup
}

func (rd *readiness) check(ctx context.Context) error {
	rd.mu.Lock()
	if !rd.checkedAt.IsZero() && time.Since(rd.checkedAt) < rd.ttl {
		err := rd.err
		rd.mu.Unlock()
		return err
	}
	if wg := rd.inflight; wg != nil {
		rd.mu.Unlock()
		wg.Wait()
		rd.mu.Lock()
		err := rd.err
		rd.mu.Unlock()
		return err
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	rd.inflight = wg
	rd.mu.Unlock()

	// The probe's own context is honoured, but the budget is ours: a kubelet
	// that is willing to wait ten seconds still must not have us holding a
	// bucket request open that long, because the next probe arrives before it
	// finishes and they accumulate.
	ctx, cancel := context.WithTimeout(ctx, rd.budget)
	err := rd.run(ctx)
	cancel()

	rd.mu.Lock()
	rd.err = err
	rd.checkedAt = time.Now()
	rd.inflight = nil
	rd.mu.Unlock()
	wg.Done()
	return err
}

// run is both halves, concurrently, inside one budget. Sequentially they would
// share the two seconds and a slow disk would make the bucket look broken.
func (rd *readiness) run(ctx context.Context) error {
	checks := []struct {
		name string
		f    func(context.Context) error
	}{
		{"disk", rd.disk},
		{"bucket", rd.bucket},
	}
	errs := make([]error, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		if c.f == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.f(ctx); err != nil {
				errs[i] = fmt.Errorf("%s: %w", c.name, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// concurrencyLimit is the configured limit with the package default filled in.
// New and Serve both need it, and a zero read in one of them would either
// refuse every request or uncap the HTTP/2 layer.
func concurrencyLimit(cfg config.Config) int {
	if cfg.Server.Concurrency <= 0 {
		return defaultConcurrency
	}
	return cfg.Server.Concurrency
}
