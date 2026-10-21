package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Configure turns tracing into a no-op until deployment supplies an OTLP URL.
// The deployment selects OTLP/gRPC for the private collector; HTTP remains
// supported for the public ingestion endpoint during migration.
func Configure(ctx context.Context, defaultService string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))

	endpoint := firstEndpoint(
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if endpoint == "" || strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return func(context.Context) error { return nil }, nil
	}

	ratio := 0.05
	if raw := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed < 0 || parsed > 1 {
			return nil, fmt.Errorf("invalid OTEL_TRACES_SAMPLER_ARG %q", raw)
		}
		ratio = parsed
	}
	serviceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = defaultService
	}
	environment := firstNonEmpty(
		os.Getenv("OTEL_ENVIRONMENT"),
		os.Getenv("DEPLOY_ENV"),
		os.Getenv("RUNTIME_ENV"),
		os.Getenv("ENVIRONMENT"),
		"unknown",
	)
	instance := firstNonEmpty(
		os.Getenv("OTEL_SERVICE_INSTANCE_ID"),
		os.Getenv("INSTANCE"),
		os.Getenv("HOSTNAME"),
		serviceName,
	)
	var exporter sdktrace.SpanExporter
	var err error
	if strings.EqualFold(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"), "grpc") {
		exporter, err = otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
	} else {
		exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	}
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(
			exporter,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(2*time.Second),
			sdktrace.WithExportTimeout(3*time.Second),
		),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(resource.NewWithAttributes("",
			attribute.String("service.name", serviceName),
			attribute.String("service.instance.id", instance),
			attribute.String("deployment.environment.name", environment),
			attribute.String("deployment.environment", environment),
			attribute.String("environment", environment),
			attribute.String("instance", instance),
		)),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// SQLOptions configures otelsql so PostgreSQL driver spans never record raw SQL
// statements or query parameters.
func SQLOptions() []otelsql.Option {
	return []otelsql.Option{
		otelsql.WithAttributes(attribute.String("db.system", "postgresql")),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			DisableQuery:         true,
			OmitConnResetSession: true,
			OmitConnPrepare:      true,
		}),
	}
}

// TransportFromNodeID extracts the proxy transport label ("xhttp", "tcp", or
// "unknown") from a node identifier.
func TransportFromNodeID(nodeID string) string {
	trimmed := strings.ToLower(strings.TrimSpace(nodeID))
	switch {
	case strings.HasSuffix(trimmed, "-xhttp") || trimmed == "xhttp":
		return "xhttp"
	case strings.HasSuffix(trimmed, "-tcp") || trimmed == "tcp":
		return "tcp"
	default:
		return "unknown"
	}
}

// DeterministicSnapshotRef computes a 16-hex-char SHA-256 prefix over the
// snapshot identity tuple so spans can correlate retries without exposing PII.
func DeterministicSnapshotRef(env, nodeID string, collectedAt time.Time) string {
	raw := strings.TrimSpace(env) + "|" + strings.TrimSpace(nodeID) + "|" + collectedAt.UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

// ExtractTraceParent parses the first valid W3C traceparent candidate into a
// SpanContext suitable for OTel Span Links across async hops.
func ExtractTraceParent(candidates ...string) (trace.SpanContext, bool) {
	tc := propagation.TraceContext{}
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		ctx := tc.Extract(context.Background(), propagation.MapCarrier{"traceparent": trimmed})
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			return sc, true
		}
	}
	return trace.SpanContext{}, false
}

func firstEndpoint(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "unknown"
}

// TraceLogAttrs returns stable correlation fields for structured request logs.
func TraceLogAttrs(ctx context.Context) []any {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return nil
	}
	attrs := RuntimeLogAttrs("web-saas-billing")
	return append(attrs,
		"trace_id", spanContext.TraceID().String(),
		"span_id", spanContext.SpanID().String(),
	)
}

func RuntimeLogAttrs(defaultService string) []any {
	serviceName := firstNonEmpty(os.Getenv("OTEL_SERVICE_NAME"), defaultService)
	environment := firstNonEmpty(os.Getenv("OTEL_ENVIRONMENT"), os.Getenv("DEPLOY_ENV"), os.Getenv("RUNTIME_ENV"), os.Getenv("ENVIRONMENT"))
	instance := firstNonEmpty(os.Getenv("OTEL_SERVICE_INSTANCE_ID"), os.Getenv("INSTANCE"), os.Getenv("HOSTNAME"), serviceName)
	return []any{"service.name", serviceName, "service_name", serviceName, "environment", environment, "instance", instance}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// RequestLogger records access logs after otelhttp has created the server
// span, so trace_id and span_id are available to VictoriaLogs.
func RequestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status, "latency", time.Since(start)}
		attrs = append(attrs, TraceLogAttrs(r.Context())...)
		logger.InfoContext(r.Context(), "request", attrs...)
	})
}
