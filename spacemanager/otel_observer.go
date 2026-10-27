package spacemanager

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/sklet/logship"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// otelObserver mirrors the agent's StateEvent stream into OpenTelemetry traces
// the Skalpai UI already ingests (the collector + core trace pipeline exist —
// we only emit, we add NO core/API/DuckDB surface for deploy).
//
// Span model (chosen for a natural timeline in any trace UI):
//
//   - deploy.space  — one root span per `sklp deploy` of a space. Lives
//     for the whole reconciliation; carries space.name / space.ephemeral.
//   - deploy.service  — one CHILD span per space service, opened on the
//     service's first transition and closed when it reaches Ready/Failed.
//     Each transition is recorded as a span EVENT (deploy.transition) with
//     from/to/detail attributes, so the service span shows the full
//     Pending→Starting→Ready (or crash-loop) lifeline as a timeline.
//
// Service spans nested under the space span give the UI an out-of-the-box
// waterfall: space width = total deploy time, each service bar = its
// startup time, transition events = the dots along the bar.
//
// Best-effort by construction: if OTEL is not configured (no endpoint env
// var) newOtelObserver returns a no-op. A collector that is down/absent never
// fails or slows a deploy — the batch processor drops on its own; shutdown is
// bounded by a short timeout.
type otelObserver struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer

	capCtx  context.Context
	capSpan trace.Span

	mu       sync.Mutex
	services map[string]trace.Span // service -> open child span
	failed   map[string]bool       // service -> ended in Failed (for space status)
}

// noopObserver is the StateObserver returned when OTEL is not configured.
func noopObserver(StateEvent) {}

// otelConfigured reports whether any standard OTLP endpoint env var is set.
// We gate span emission on it so the observer is a silent no-op out of the box
// and only talks to a collector the operator explicitly pointed us at.
// traceExportAllowed reports whether the configured credential may push
// traces. A node's log ingest key is scoped to logs and metrics: sending
// deploy spans with it only earns a 403 from the collector on every event.
// An explicit OTEL_EXPORTER_OTLP_HEADERS (a project key) opts back in.
func traceExportAllowed() bool {
	if os.Getenv("OTEL_EXPORTER_OTLP_HEADERS") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS") != "" {
		return true
	}
	return os.Getenv(logship.EnvIngestKey) == ""
}

func otelConfigured() bool {
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// NewOtelObserver builds the space trace exporter and opens the root span.
// It returns:
//   - a StateObserver to compose into the agent's Options.Observer,
//   - a shutdown func to flush + close the provider (call when the deploy ends),
//   - an error only if OTEL is configured AND init failed.
//
// When OTEL is not configured it returns (noopObserver, no-op shutdown, nil):
// the caller wires it unconditionally and a missing collector is invisible.
func NewOtelObserver(ctx context.Context, meta SpaceMeta) (StateObserver, func(), error) {
	if !otelConfigured() || !traceExportAllowed() {
		return noopObserver, func() {}, nil
	}

	// otlptracehttp.New reads OTEL_EXPORTER_OTLP_* (endpoint, TLS, protocol)
	// from the environment, defaulting to localhost:4318 — point it at the
	// Skalpai collector via OTEL_EXPORTER_OTLP_ENDPOINT. The SDK does not read
	// x-api-key from any env var on its own (unlike logship's exporter), so it
	// is passed explicitly here, same key as log shipping.
	opts := []otlptracehttp.Option{}
	if key := os.Getenv(logship.EnvIngestKey); key != "" {
		opts = append(opts, otlptracehttp.WithHeaders(map[string]string{"x-api-key": key}))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}

	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("sklp-deploy"),
	))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	o := newOtelObserverStruct(ctx, meta, tp)
	return o.onEvent, func() { o.shutdown() }, nil
}

// newOtelObserverStruct builds the observer on an already-constructed
// TracerProvider and opens the root space span. Split out (and returning the
// concrete type) so tests can inject a tracetest-backed provider, finalise
// spans (finalizeSpans), and read the in-memory exporter BEFORE provider
// Shutdown — tracetest's InMemoryExporter wipes its store on Shutdown.
func newOtelObserverStruct(ctx context.Context, meta SpaceMeta, tp *sdktrace.TracerProvider) *otelObserver {
	tracer := tp.Tracer("github.com/lalternativefabrique/sklp/spacemanager")
	o := &otelObserver{
		tp:       tp,
		tracer:   tracer,
		services: make(map[string]trace.Span),
		failed:   make(map[string]bool),
	}
	o.capCtx, o.capSpan = tracer.Start(ctx, "deploy.space",
		trace.WithAttributes(
			attribute.String("space.name", meta.Name),
			attribute.Bool("space.ephemeral", meta.Ephemeral),
			attribute.String("space.url", meta.URL),
		),
	)
	return o
}

// onEvent records one transition: open the service child span on first sight,
// add a deploy.transition event, and close the span on a terminal state.
func (o *otelObserver) onEvent(e StateEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()

	span := o.services[e.Service]
	if span == nil {
		_, span = o.tracer.Start(o.capCtx, "deploy.service",
			trace.WithAttributes(attribute.String("service.name", e.Service)),
		)
		o.services[e.Service] = span
	}

	span.AddEvent("deploy.transition", trace.WithAttributes(
		attribute.String("state.from", e.From.String()),
		attribute.String("state.to", e.To.String()),
		attribute.String("detail", e.Detail),
	))
	span.SetAttributes(attribute.String("service.state", e.To.String()))

	switch e.To {
	case Ready, Done:
		// Done = a oneshot job completed (exit 0): close its span Ok, like a
		// service reaching Ready.
		span.SetStatus(codes.Ok, "")
		span.End()
		delete(o.services, e.Service)
		delete(o.failed, e.Service)
	case Failed, Degraded:
		// Mark failed but keep the span OPEN: a crash loop emits
		// Failed→Restarting and we want the eventual Ready (or the deploy's
		// end) to close it, not the first transient failure. We only flip the
		// space to error at shutdown if the service is still failed.
		o.failed[e.Service] = true
	default:
		// Starting/Restarting after a failure clears the sticky failed mark.
		delete(o.failed, e.Service)
	}
}

// finalizeSpans closes any still-open service spans and the space span with
// an aggregate status. Separated from provider teardown so tests can read the
// in-memory exporter before Shutdown wipes it.
func (o *otelObserver) finalizeSpans() {
	o.mu.Lock()
	defer o.mu.Unlock()
	anyFailed := false
	for svc, span := range o.services {
		if o.failed[svc] {
			span.SetStatus(codes.Error, "never became ready")
			anyFailed = true
		} else {
			span.SetStatus(codes.Ok, "stopped")
		}
		span.End()
		delete(o.services, svc)
	}
	if anyFailed {
		o.capSpan.SetStatus(codes.Error, "one or more services never became ready")
	} else {
		o.capSpan.SetStatus(codes.Ok, "")
	}
	o.capSpan.End()
}

// shutdown finalises every span then flushes + closes the provider under a
// short bounded timeout, so a slow/absent collector can never hang CLI exit.
func (o *otelObserver) shutdown() {
	o.finalizeSpans()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = o.tp.Shutdown(ctx)
}
