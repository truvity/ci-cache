// Package admin serves the admin.v1.Admin Connect service.
//
// It is on its OWN listener, never the data port. On the data port every CI
// job in the estate is a client, and a job that can wipe can empty or poison
// the cache for everyone; separating the two makes that a question of which
// port a NetworkPolicy admits rather than of getting an authorisation rule
// right in every handler.
//
// # There is no authentication in this milestone, and that is a decision
//
// The port's reachability IS the boundary. The service listens on the admin
// port, the chart puts that port in no consumer NetworkPolicy, and reaching
// it means being inside the namespace or holding a port-forward -- which
// already means holding credentials that can delete the pod and its volume
// outright. A token checked here would add nothing against that attacker and
// would be one more secret to mount, rotate and lose.
//
// A later change adds a gateway-issued token, when the UI is served to
// humans from outside the cluster and reachability stops being the boundary.
// That change adds an interceptor here; nothing else in this package moves.
//
// This paragraph exists so that nobody reads the absence as an oversight and
// bolts on a half-scheme in a hurry.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	adminv1 "github.com/truvity/ci-cache/gen/admin/v1"
	"github.com/truvity/ci-cache/gen/admin/v1/adminv1connect"
)

// shutdownGrace is how long ListenAndServe gives in-flight calls when its
// context is cancelled. Admin calls are unary and short, except a wipe, which
// is the one nobody wants interrupted halfway through a bucket.
const shutdownGrace = 30 * time.Second

// Service is the administrative API.
type Service struct {
	deps Deps
	log  *slog.Logger

	// mu guards srv, which exists only between ListenAndServe and Shutdown.
	// Shutdown may be called from a signal handler while ListenAndServe is
	// still setting up, and a nil map read there would be a crash during an
	// orderly stop -- the worst possible time for one.
	mu  sync.Mutex
	srv *http.Server
}

// Option configures a Service.
type Option func(*Service)

// WithLogger sets the logger. Every mutating call is logged through it at
// INFO; a Service built without one logs to slog's default.
func WithLogger(log *slog.Logger) Option {
	return func(s *Service) {
		if log != nil {
			s.log = log
		}
	}
}

// New returns the administrative service over deps.
func New(deps Deps, opts ...Option) *Service {
	s := &Service{deps: deps, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Handler returns the service's route and handler, for mounting on a mux the
// caller owns.
//
// Returning the path rather than taking a mux is what lets the CLI put the
// admin service and a future UI on one listener without this package knowing
// the UI exists.
func (s *Service) Handler() (string, http.Handler) {
	return adminv1connect.NewAdminServiceHandler(s)
}

// ListenAndServe serves the administrative API on addr until the context is
// cancelled or Shutdown is called.
//
// It returns nil on an orderly stop. Callers treat a non-nil return as fatal,
// so http.ErrServerClosed -- which is what an orderly stop looks like from
// inside -- must not escape.
//
// Plain HTTP/1.1, with no h2c. Every RPC here is unary, which Connect speaks
// over HTTP/1.1 perfectly well; h2c would pull in a dependency to serve an
// admin port that carries one request at a time.
func (s *Service) ListenAndServe(ctx context.Context, addr string) error {
	path, h := s.Handler()
	mux := http.NewServeMux()
	mux.Handle(path, h)

	// The listener is opened through the context so that a start-up
	// cancelled while the port is still busy -- a rolling restart with a
	// lingering socket -- fails fast instead of blocking the shutdown.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("admin: listen on %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler: mux,
		// A wipe of a large prefix takes as long as the bucket takes, and a
		// read deadline that cut it off would leave half a prefix deleted
		// with nothing recording which half.
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
			defer cancel()
			_ = s.Shutdown(sctx)
		case <-done:
		}
	}()

	s.log.Info("admin: listening", "addr", ln.Addr().String(), "version", s.deps.Version)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("admin: serve: %w", err)
	}
	return nil
}

// Shutdown stops the listener, letting in-flight calls finish.
//
// Calling it before ListenAndServe, or twice, is not an error: a process that
// fails to start still runs its deferred stops.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// caller is the address a mutating call came from, for the log line.
//
// It is best effort: behind a proxy it is the proxy. That is still worth
// recording -- "which pod issued the wipe" is answerable from it, and the
// alternative is a log line that says a cache was emptied and not by whom.
func caller(peer connect.Peer) string {
	if peer.Addr == "" {
		return "unknown"
	}
	return peer.Addr
}

// removed builds the wire form of what a mutating call did.
func removed(entries, bytes int64) *adminv1.Removed {
	return &adminv1.Removed{Entries: entries, Bytes: bytes}
}
