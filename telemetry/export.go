package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/truvity/ci-cache/config"
)

// exporters is everything New has to shut down again, plus the two seams an
// OTLP pipeline needs into the rest of this package.
//
// It is a struct rather than a set of return values so that adding a third
// pipeline -- logs, when there is a reason for them -- is a field here and
// nothing at the call site.
type exporters struct {
	// hook is handed to the Recorder so that the instruments with no
	// observable form see their events; see Hook.
	hook Hook
	// tracer backs Telemetry.StartSpan.
	tracer tracer
	// shutdown flushes each pipeline, in the order given.
	shutdown []func(context.Context) error
}

// newExporters builds the OTLP pipelines, or returns (nil, nil) when nothing
// is to be exported.
//
// EVERY COUNTER IS OBSERVED, NOT PUSHED. The instruments read
// Recorder.Snapshot at collection time rather than being incremented beside
// it, so the collector and the admin API report one set of numbers instead of
// two that drift -- and they drift silently, which is the worst kind: a
// dashboard and a CLI that disagree about a hit rate leave nobody able to say
// which is lying. The two exceptions are histograms, which have no observable
// form, and they arrive through Hook.
func newExporters(ctx context.Context, cfg config.Telemetry, version string, t *Telemetry) (*exporters, error) {
	if cfg.OTLPEndpoint == "" {
		// Nothing goes out, and the Recorder still counts everything: see
		// the package comment for why that is a supported configuration and
		// not a degraded one.
		return nil, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("ci-cache"),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, err
	}

	metricExp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(cfg.OTLPEndpoint))
	if err != nil {
		return nil, err
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
	)
	meter := provider.Meter("github.com/truvity/ci-cache")

	h, err := registerInstruments(meter, t)
	if err != nil {
		return nil, err
	}

	traceExp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint))
	if err != nil {
		// The metric pipeline is already running; shutting it down here
		// keeps a half-built telemetry layer from being returned as if it
		// were whole.
		return nil, errors.Join(err, provider.Shutdown(ctx))
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
		// ParentBased so that a request sampled at the front-end stays
		// sampled across the hop into the bucket. Re-rolling per span would
		// produce traces with holes in them, which are worse than no traces:
		// they look like a component that did not run.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TraceRatio))),
	)

	return &exporters{
		hook:   h,
		tracer: otelTracer{t: tp.Tracer("github.com/truvity/ci-cache")},
		shutdown: []func(context.Context) error{
			tp.Shutdown,
			provider.Shutdown,
		},
	}, nil
}

// registerInstruments declares every name in MetricNames and wires its
// callback.
//
// One callback registration covers all of the observable instruments, so the
// whole set is read from a SINGLE Snapshot: registered separately, each would
// take its own snapshot microseconds apart and a scrape could report more
// hits than gets. Nothing in a graph is harder to argue with than a ratio
// above one.
func registerInstruments(meter metric.Meter, t *Telemetry) (Hook, error) {
	var (
		errs []error
		must = func(err error) { errs = append(errs, err) }
	)

	getC, err := meter.Int64ObservableCounter(MetricGet)
	must(err)
	putC, err := meter.Int64ObservableCounter(MetricPut)
	must(err)
	bytesC, err := meter.Int64ObservableCounter(MetricBytes)
	must(err)
	diskUsed, err := meter.Int64ObservableGauge(MetricDiskUsedBytes)
	must(err)
	diskBudget, err := meter.Int64ObservableGauge(MetricDiskBudgetBytes)
	must(err)
	diskEntries, err := meter.Int64ObservableGauge(MetricDiskEntries)
	must(err)
	gcEvicted, err := meter.Int64ObservableCounter(MetricGCEvictedBytes)
	must(err)
	gcRuns, err := meter.Int64ObservableCounter(MetricGCRuns)
	must(err)
	negEntries, err := meter.Int64ObservableGauge(MetricNegativeEntries)
	must(err)
	queueDepth, err := meter.Int64ObservableGauge(MetricUploadQueueDepth)
	must(err)
	degraded, err := meter.Int64ObservableGauge(MetricAgentDegraded)
	must(err)

	// The two synchronous instruments. A histogram cannot be observed at
	// collection time -- its whole point is the distribution of individual
	// events -- so these are the only ones fed as they happen.
	latency, err := meter.Float64Histogram(MetricLatency, metric.WithUnit("s"))
	must(err)
	gcDuration, err := meter.Float64Histogram(MetricGCDuration, metric.WithUnit("s"))
	must(err)

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	observed := []metric.Observable{
		getC, putC, bytesC,
		diskUsed, diskBudget, diskEntries,
		gcEvicted, gcRuns, negEntries, queueDepth, degraded,
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		snap := t.Recorder().Snapshot()
		for _, f := range snap.Frontends {
			for _, ts := range f.Tiers {
				base := []attribute.KeyValue{
					attribute.String(AttrFrontend, f.Frontend),
					attribute.String(AttrTier, ts.Tier),
				}
				for outcome, n := range ts.GetByOutcome {
					o.ObserveInt64(getC, n, metric.WithAttributes(
						append(append([]attribute.KeyValue{}, base...),
							attribute.String(AttrOutcome, outcome))...))
				}
				for outcome, n := range ts.PutByOutcome {
					o.ObserveInt64(putC, n, metric.WithAttributes(
						append(append([]attribute.KeyValue{}, base...),
							attribute.String(AttrOutcome, outcome))...))
				}
				o.ObserveInt64(bytesC, ts.BytesRead, metric.WithAttributes(
					append(append([]attribute.KeyValue{}, base...),
						attribute.String(AttrDirection, DirectionRead))...))
				o.ObserveInt64(bytesC, ts.BytesWritten, metric.WithAttributes(
					append(append([]attribute.KeyValue{}, base...),
						attribute.String(AttrDirection, DirectionWritten))...))
			}
		}
		o.ObserveInt64(gcEvicted, snap.GC.EvictedBytes)
		o.ObserveInt64(gcRuns, snap.GC.Runs)

		// The gauges are pulled from whatever registered them. A nil field
		// is observed as nothing at all rather than as zero: a gap on a
		// graph says "this component is not reporting", and a zero says "the
		// volume is empty", which are opposite conclusions.
		g := t.Gauges()
		if g.DiskUsedBytes != nil {
			o.ObserveInt64(diskUsed, g.DiskUsedBytes())
		}
		if g.DiskBudgetBytes != nil {
			o.ObserveInt64(diskBudget, g.DiskBudgetBytes())
		}
		if g.DiskEntries != nil {
			o.ObserveInt64(diskEntries, g.DiskEntries())
		}
		if g.DiskBytesByFrontend != nil {
			for fe, n := range g.DiskBytesByFrontend() {
				o.ObserveInt64(diskUsed, n, metric.WithAttributes(attribute.String(AttrFrontend, fe)))
			}
		}
		if g.DiskEntriesByFrontend != nil {
			for fe, n := range g.DiskEntriesByFrontend() {
				o.ObserveInt64(diskEntries, n, metric.WithAttributes(attribute.String(AttrFrontend, fe)))
			}
		}
		if g.NegativeEntriesByFrontend != nil {
			for fe, n := range g.NegativeEntriesByFrontend() {
				o.ObserveInt64(negEntries, n, metric.WithAttributes(attribute.String(AttrFrontend, fe)))
			}
		}
		if g.UploadQueueDepth != nil {
			o.ObserveInt64(queueDepth, g.UploadQueueDepth())
		}
		if g.AgentDegraded != nil {
			o.ObserveInt64(degraded, g.AgentDegraded())
		}
		return ctx.Err()
	}, observed...)
	if err != nil {
		return nil, err
	}

	return otelHook{latency: latency, gc: gcDuration}, nil
}

// otelHook forwards the two synchronous instruments.
type otelHook struct {
	latency metric.Float64Histogram
	gc      metric.Float64Histogram
}

func (h otelHook) Observe(frontend, tierName, op string, d time.Duration) {
	h.latency.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String(AttrFrontend, frontend),
		attribute.String(AttrTier, tierName),
		attribute.String(AttrOp, op),
	))
}

func (h otelHook) GCDuration(d time.Duration) {
	h.gc.Record(context.Background(), d.Seconds())
}

// otelTracer adapts the SDK to this package's span seam, so that nothing
// outside this file imports OpenTelemetry.
type otelTracer struct{ t oteltrace.Tracer }

func (o otelTracer) start(ctx context.Context, name string, attrs []Attr) (context.Context, Span) {
	kv := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		kv = append(kv, attribute.String(a.Key, a.Value))
	}
	ctx, s := o.t.Start(ctx, name, oteltrace.WithAttributes(kv...))
	return ctx, otelSpan{s: s}
}

type otelSpan struct{ s oteltrace.Span }

func (o otelSpan) SetAttr(k, v string) {
	o.s.SetAttributes(attribute.String(k, v))
}

func (o otelSpan) End(err error) {
	if err != nil {
		o.s.RecordError(err)
	}
	o.s.End()
}
