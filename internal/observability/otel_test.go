package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestConfigureRegistersW3CTraceContextEvenWhenDisabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := Configure(context.Background(), "web-saas-billing")
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	fields := otel.GetTextMapPropagator().Fields()
	hasTraceParent := false
	for _, f := range fields {
		if f == "traceparent" {
			hasTraceParent = true
		}
		if f == "baggage" {
			t.Fatalf("expected W3C TraceContext without baggage, got fields %v", fields)
		}
	}
	if !hasTraceParent {
		t.Fatalf("expected traceparent propagator field, got %v", fields)
	}

	carrier := propagation.MapCarrier{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := tp.Tracer("test").Start(ctx, "child")
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 span, got %d", len(ended))
	}
	if got := ended[0].SpanContext().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("expected trace_id 4bf92f3577b34da6a3ce929d0e0e4736, got %s", got)
	}
	if got := ended[0].Parent().SpanID().String(); got != "00f067aa0ba902b7" {
		t.Fatalf("expected parent span_id 00f067aa0ba902b7, got %s", got)
	}
}

func TestConfigureFallsBackToGenericOTLPEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1.0")

	shutdown, err := Configure(context.Background(), "web-saas-billing")
	if err != nil {
		t.Fatalf("Configure with OTEL_EXPORTER_OTLP_ENDPOINT fallback: %v", err)
	}
	_ = shutdown(context.Background())
}

func TestTransportFromNodeID(t *testing.T) {
	cases := map[string]string{
		"jp-tyo-01-xhttp": "xhttp",
		"JP-TYO-01-TCP":   "tcp",
		"agent-proxy":     "unknown",
		"":                "unknown",
	}
	for nodeID, want := range cases {
		if got := TransportFromNodeID(nodeID); got != want {
			t.Fatalf("TransportFromNodeID(%q) = %q, want %q", nodeID, got, want)
		}
	}
}

func TestDeterministicSnapshotRef(t *testing.T) {
	ts := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	ref1 := DeterministicSnapshotRef("prod", "jp-tyo-01-xhttp", ts)
	ref2 := DeterministicSnapshotRef("prod", "jp-tyo-01-xhttp", ts)
	refOther := DeterministicSnapshotRef("uat", "jp-tyo-01-xhttp", ts)

	if len(ref1) != 16 {
		t.Fatalf("expected 16-char hex ref, got %q (len=%d)", ref1, len(ref1))
	}
	if ref1 != ref2 {
		t.Fatalf("expected deterministic ref, got %q != %q", ref1, ref2)
	}
	if ref1 == refOther {
		t.Fatalf("expected distinct ref for different env, got %q", ref1)
	}
}

func TestExtractTraceParent(t *testing.T) {
	sc, ok := ExtractTraceParent("", "invalid", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok || !sc.IsValid() {
		t.Fatalf("expected valid SpanContext, got ok=%v sc=%v", ok, sc)
	}
	if sc.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("unexpected trace_id %s", sc.TraceID().String())
	}
	if _, ok := ExtractTraceParent("", "not-a-traceparent"); ok {
		t.Fatalf("expected invalid traceparent to return false")
	}
}

func TestRequestLoggerCapturesStatusAndTraceFields(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := RequestLogger(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":"error"}`))
	}))

	ctx, span := tp.Tracer("test").Start(context.Background(), "server")
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest/snapshots", nil).WithContext(ctx)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	span.End()

	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 response code, got %d", res.Code)
	}

	var logEntry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("unmarshal log line: %v (%s)", err, buf.String())
	}
	if got := int(logEntry["status"].(float64)); got != http.StatusUnprocessableEntity {
		t.Fatalf("expected logged status 422, got %v", logEntry["status"])
	}
	if got := logEntry["trace_id"]; got != span.SpanContext().TraceID().String() {
		t.Fatalf("expected logged trace_id %s, got %v", span.SpanContext().TraceID().String(), got)
	}
	if got := logEntry["span_id"]; got != span.SpanContext().SpanID().String() {
		t.Fatalf("expected logged span_id %s, got %v", span.SpanContext().SpanID().String(), got)
	}
}

func TestSQLOptionsAreConfigured(t *testing.T) {
	opts := SQLOptions()
	if len(opts) < 2 {
		t.Fatalf("expected SQLOptions to configure attributes and span options, got %d option(s)", len(opts))
	}
}
