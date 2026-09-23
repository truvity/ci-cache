// Package telemetry is the cache's metrics, traces and logging.
//
// It is one package rather than three because the three have one rule between
// them: the numbers a collector scrapes and the numbers the admin API reports
// are the same numbers. A Recorder holds them, the admin API reads it, and
// the OTLP exporter observes it. Nothing here keeps a second set.
//
// An empty OTLPEndpoint exports NOTHING and still counts everything. That is
// deliberate: a cache installed before a collector exists, or running on a
// laptop, must still answer "what did you do" through `ci-cache stats`. A
// telemetry layer that only works when a collector is reachable makes the
// first hour of every installation blind.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/truvity/ci-cache/config"
)

// Telemetry is the process's metrics and traces.
type Telemetry struct {
	rec    *Recorder
	tracer tracer

	// mu guards gauges and shutdown, both of which are set after New: the
	// gauge sources live in components that need the Telemetry to exist
	// first, and an exporter's shutdown is registered as it is built.
	mu       sync.Mutex
	gauges   Gauges
	shutdown []func(context.Context) error
}

// Gauges is what the observable instruments read at collection time.
//
// They are functions rather than values because a gauge that is pushed goes
// stale the moment the component that pushes it stops: a disk tier that has
// wedged would keep reporting the last size it managed to write, and the
// graph would show a healthy volume. Pulled, it reports whatever the
// component says now, or -- for a nil field -- nothing at all, which is a
// gap on the graph and therefore visible.
type Gauges struct {
	// DiskUsedBytes, DiskBudgetBytes and DiskEntries describe the volume as
	// a whole; they carry no frontend attribute.
	DiskUsedBytes   func() int64
	DiskBudgetBytes func() int64
	DiskEntries     func() int64

	// DiskBytesByFrontend and DiskEntriesByFrontend split the same volume by
	// front-end, which is what a per-front-end budget is judged against.
	DiskBytesByFrontend   func() map[string]int64
	DiskEntriesByFrontend func() map[string]int64

	// NegativeEntriesByFrontend is how many "known absent" answers are held.
	NegativeEntriesByFrontend func() map[string]int64

	// UploadQueueDepth is how many objects are waiting for the bucket.
	UploadQueueDepth func() int64

	// AgentDegraded is 1 while a runner's agent cannot reach the server, and
	// is left nil by the server: see MetricAgentDegraded.
	AgentDegraded func() int64
}

// New builds the telemetry layer.
//
// With an empty cfg.OTLPEndpoint it exports nothing and still counts
// everything; with an endpoint it also starts the OTLP exporters. Either way
// the returned Telemetry is usable and Shutdown is safe to call.
func New(ctx context.Context, cfg config.Telemetry, version string) (*Telemetry, error) {
	t := &Telemetry{rec: newRecorder(nil), tracer: noopTracer{}}

	exp, err := newExporters(ctx, cfg, version, t)
	if err != nil {
		return nil, err
	}
	if exp != nil {
		// The recorder is rebuilt rather than mutated, because its hook is
		// read without a lock on every tier call: setting it after the first
		// goroutine exists would be a data race in the hottest path there is.
		t.rec = newRecorder(exp.hook)
		t.tracer = exp.tracer
		t.shutdown = exp.shutdown
	}
	return t, nil
}

// Recorder returns the counter set. It is never nil.
func (t *Telemetry) Recorder() *Recorder { return t.rec }

// SetGauges registers the sources the observable instruments read.
//
// It is separate from New because the things being measured -- the disk tier,
// the upload queue -- are built with a Recorder in hand, so they cannot exist
// before New returns. Calling it twice replaces the whole set.
func (t *Telemetry) SetGauges(g Gauges) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gauges = g
}

// Gauges returns the registered gauge sources.
//
// The exporter's observable callbacks read it on every collection, rather
// than capturing the set once: SetGauges may be called after the pipeline is
// already running, and a callback that captured an empty set would export a
// disk of zero bytes forever.
func (t *Telemetry) Gauges() Gauges {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gauges
}

// Shutdown flushes and stops the exporters.
//
// Errors are joined rather than returned one at a time: a shutdown that stops
// at the first failure leaves the rest of the pipeline running, and the last
// batch of spans is exactly the batch somebody wanted.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	t.mu.Lock()
	fns := t.shutdown
	t.shutdown = nil
	t.mu.Unlock()

	var errs []error
	for _, fn := range fns {
		if err := fn(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// StartSpan begins a span, and returns the context its children belong to.
//
// There is one span per upstream fetch and one per bucket call, and nothing
// finer. A span per disk read would be the majority of every trace and would
// say only what MetricLatency already says; the two calls that leave the pod
// are the ones a trace is for.
//
// With no exporter the returned Span does nothing and the context is
// unchanged, so call sites need no condition around them.
func (t *Telemetry) StartSpan(ctx context.Context, name string, attrs ...Attr) (context.Context, Span) {
	return t.tracer.start(ctx, name, attrs)
}

// Attr is one key/value on a span.
type Attr struct {
	Key   string
	Value string
}

// Span is one unit of work in a trace.
type Span interface {
	// SetAttr adds a key/value to the span.
	SetAttr(key, value string)
	// End closes the span. A non-nil err marks it failed and records the
	// message; passing the error rather than a bool is what makes a failed
	// span say WHY without a second call nobody remembers to make.
	End(err error)
}

type tracer interface {
	start(ctx context.Context, name string, attrs []Attr) (context.Context, Span)
}

type noopTracer struct{}

func (noopTracer) start(ctx context.Context, _ string, _ []Attr) (context.Context, Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) SetAttr(_, _ string) {}
func (noopSpan) End(_ error)         {}

// NewLogger returns a JSON logger on stdout at the named level.
//
// JSON on stdout and nothing else: the pod's logs are collected by whatever
// the cluster runs, and a cache that writes its own files gives an operator
// one more thing to rotate. Stdout rather than stderr because these are
// records of what happened, not diagnostics of a failing program.
func NewLogger(level string) (*slog.Logger, error) {
	return newLogger(os.Stdout, level)
}

func newLogger(w io.Writer, level string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}

// ParseLevel turns a configured level name into a slog.Level.
//
// An unknown name is an error rather than a silent fall back to info. A
// deployment that asked for debug and got info would look like a cache that
// is not logging, and the search for that starts everywhere except the typo.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("telemetry: unknown log level %q (debug, info, warn, error)", s)
	}
}
