package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/truvity/ci-cache/config"
)

// TestNoCollectorStillCounts is the promise the package comment makes.
//
// A cache installed before the observability stack exists still has to answer
// "what did you do" through the admin API. If New returned a Telemetry whose
// counters were inert without an endpoint, that answer would be zeroes, and
// the first thing anyone did with a new installation would be to conclude it
// was not caching.
func TestNoCollectorStillCounts(t *testing.T) {
	t.Parallel()

	tel, err := New(context.Background(), config.Telemetry{}, "0.0.0-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	rec := tel.Recorder()
	if rec == nil {
		t.Fatal("Recorder is nil with no collector configured")
	}
	rec.Get("go/build", "disk", OutcomeHit)
	rec.Bytes("go/build", "disk", DirectionRead, 42)

	snap := rec.Snapshot()
	if len(snap.Frontends) != 1 || snap.Frontends[0].Tiers[0].Hits != 1 {
		t.Fatalf("counters are inert without a collector: %+v", snap)
	}
	if got := snap.Frontends[0].Tiers[0].BytesRead; got != 42 {
		t.Errorf("bytesRead = %d, want 42", got)
	}
}

// TestAConfiguredEndpointBuildsAPipeline replaces the guard that used to
// stand here while export.go was a stub.
//
// It is kept, rather than deleted, for the reason the stub existed: an
// otlpEndpoint that silently does nothing sends an operator to look at the
// collector, the NetworkPolicy and the sampling ratio before the binary. The
// claim has simply moved from "it is refused" to "it is honoured" -- and
// something must still assert one of the two.
func TestAConfiguredEndpointBuildsAPipeline(t *testing.T) {
	t.Parallel()

	// An endpoint that resolves but never answers. The SDK's exporters
	// connect lazily, so New must succeed here: a cache whose start-up
	// waited on a collector would be a cache that a collector outage can
	// stop, which is exactly backwards.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tel, err := New(context.Background(), config.Telemetry{OTLPEndpoint: srv.URL, TraceRatio: 1}, "0.0.0-test")
	if err != nil {
		t.Fatalf("New with an endpoint: %v", err)
	}

	// The seams the exporter is supposed to fill: a span that is no longer
	// inert, and a hook on the recorder.
	ctx, span := tel.StartSpan(context.Background(), "bucket.get")
	if ctx == context.Background() {
		t.Error("StartSpan returned the same context: no tracer was installed")
	}
	span.End(nil)
	tel.Recorder().Observe("go/build", "bucket", "get", time.Millisecond)

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(shutdown); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestSpansAreInertWithoutAnExporter keeps the call sites unconditional.
func TestSpansAreInertWithoutAnExporter(t *testing.T) {
	t.Parallel()

	tel, err := New(context.Background(), config.Telemetry{TraceRatio: 1}, "0.0.0-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	got, span := tel.StartSpan(ctx, "bucket.get", Attr{Key: "key", Value: "go/build/aa"})
	if got != ctx {
		t.Error("StartSpan changed the context with no exporter configured")
	}
	span.SetAttr("outcome", OutcomeHit)
	span.End(nil)
}

// TestGaugesAreReadThroughTheAccessor checks that a set registered after New
// is visible, which is the only order the process can build things in.
func TestGaugesAreReadThroughTheAccessor(t *testing.T) {
	t.Parallel()

	tel, err := New(context.Background(), config.Telemetry{}, "0.0.0-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tel.Gauges().DiskUsedBytes != nil {
		t.Error("a fresh Telemetry already has gauges")
	}
	tel.SetGauges(Gauges{DiskUsedBytes: func() int64 { return 1 << 30 }})
	if got := tel.Gauges().DiskUsedBytes(); got != 1<<30 {
		t.Errorf("DiskUsedBytes = %d, want %d", got, int64(1)<<30)
	}
}

// TestShutdownIsIdempotent: a process that failed to start still runs its
// deferred stops, sometimes twice.
func TestShutdownIsIdempotent(t *testing.T) {
	t.Parallel()

	tel, err := New(context.Background(), config.Telemetry{}, "0.0.0-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range 2 {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown %d: %v", i, err)
		}
	}
}

// TestParseLevel checks that a typo is an error rather than a silent info.
func TestParseLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		// Case and surrounding space come from a values file somebody
		// hand-edited; neither is worth a start-up failure.
		{"INFO", slog.LevelInfo},
		{"", slog.LevelInfo},
		{" warn ", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
	}
	for _, c := range cases {
		got, err := ParseLevel(c.in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("ParseLevel accepted \"verbose\": a typo would run at info and look like a cache that is not logging")
	}
}

// TestLoggerIsJSONAtTheConfiguredLevel checks both halves of the helper: the
// format a collector parses, and the level actually taking effect.
func TestLoggerIsJSONAtTheConfiguredLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := newLogger(&buf, "warn")
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	log.Info("dropped")
	log.Warn("kept", "prefix", "go/build/")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (the info line should have been dropped): %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, lines[0])
	}
	if rec["msg"] != "kept" || rec["prefix"] != "go/build/" {
		t.Errorf("log record = %v, want msg=kept prefix=go/build/", rec)
	}
}
