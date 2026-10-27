package spacemanager

import (
	"context"
	"github.com/lalternativefabrique/sklp/sklet/logship"
	"os"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// findSpan returns the first exported span with the given name.
func findSpan(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

func attrString(s tracetest.SpanStub, key string) (string, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

func TestOtelObserverEmitsSpaceAndServiceSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))

	meta := SpaceMeta{Name: "prod", Ephemeral: false, URL: "https://prod.example"}
	o := newOtelObserverStruct(context.Background(), meta, tp)

	o.onEvent(StateEvent{Service: "api", From: Pending, To: Starting, Detail: "api-0"})
	o.onEvent(StateEvent{Service: "api", From: Starting, To: Ready, Detail: "ready after 2s"})
	o.onEvent(StateEvent{Service: "db", From: Pending, To: Starting, Detail: "db-0"})
	o.onEvent(StateEvent{Service: "db", From: Starting, To: Ready, Detail: "ready after 1s"})

	o.finalizeSpans() // closes space span; read before provider Shutdown wipes

	spans := exp.GetSpans()

	cap, ok := findSpan(spans, "deploy.space")
	if !ok {
		t.Fatalf("no deploy.space span; got %d spans", len(spans))
	}
	if v, _ := attrString(cap, "space.name"); v != "prod" {
		t.Errorf("space.name = %q, want prod", v)
	}

	// One deploy.service span per service, each Ready, each carrying its
	// transition events, all parented to the space span.
	var svcSpans int
	for _, s := range spans {
		if s.Name != "deploy.service" {
			continue
		}
		svcSpans++
		if s.Parent.SpanID() != cap.SpanContext.SpanID() {
			t.Errorf("service span %q not parented to space", s.Name)
		}
		name, _ := attrString(s, "service.name")
		if name != "api" && name != "db" {
			t.Errorf("unexpected service.name %q", name)
		}
		// Each service saw two transitions → two deploy.transition events.
		var events int
		for _, ev := range s.Events {
			if ev.Name == "deploy.transition" {
				events++
			}
		}
		if events != 2 {
			t.Errorf("service %q: %d transition events, want 2", name, events)
		}
		if state, _ := attrString(s, "service.state"); state != "ready" {
			t.Errorf("service %q final state %q, want ready", name, state)
		}
	}
	if svcSpans != 2 {
		t.Errorf("got %d deploy.service spans, want 2", svcSpans)
	}
}

func TestOtelObserverNoEndpointIsNoop(t *testing.T) {
	// Ensure no OTLP endpoint is configured.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	os.Unsetenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")

	obs, shutdown, err := NewOtelObserver(context.Background(), SpaceMeta{Name: "prod"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obs == nil || shutdown == nil {
		t.Fatal("no-op observer must return non-nil obs + shutdown")
	}
	// The no-op observer must not panic on events or shutdown.
	obs(StateEvent{Service: "api", From: Pending, To: Starting})
	shutdown()
}

func TestOtelObserverServiceFailureSetsErrorStatus(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))

	o := newOtelObserverStruct(context.Background(), SpaceMeta{Name: "prod"}, tp)
	// Service fails and never recovers (no Ready, no Restarting after).
	o.onEvent(StateEvent{Service: "api", From: Pending, To: Starting})
	o.onEvent(StateEvent{Service: "api", From: Starting, To: Failed, Detail: "probe timeout"})
	o.finalizeSpans()

	spans := exp.GetSpans()
	cap, ok := findSpan(spans, "deploy.space")
	if !ok {
		t.Fatal("no space span")
	}
	if cap.Status.Code.String() != "Error" {
		t.Errorf("space status = %v, want Error", cap.Status.Code)
	}
	svc, ok := findSpan(spans, "deploy.service")
	if !ok {
		t.Fatal("no service span")
	}
	if svc.Status.Code.String() != "Error" {
		t.Errorf("service status = %v, want Error", svc.Status.Code)
	}
}

// A node only holds a log ingest key, which the collector refuses for
// traces (403 on every deploy event, seen on node-a 2026-09-04): with
// that key alone the observer must stay silent instead of exporting.
func TestOtelObserverIngestKeyAloneIsNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
	t.Setenv(logship.EnvIngestKey, "sklp_ingest_x")

	if traceExportAllowed() {
		t.Fatal("an ingest key alone must not export traces")
	}
	obs, shutdown, err := NewOtelObserver(context.Background(), SpaceMeta{Name: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	obs(StateEvent{Service: "api", From: Pending, To: Starting})
	shutdown()

	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=sk_project")
	if !traceExportAllowed() {
		t.Fatal("an explicit project header must allow traces")
	}
}
