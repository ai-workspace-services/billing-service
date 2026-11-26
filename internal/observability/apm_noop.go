//go:build no_apm

package observability

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	ProviderNone    = "none"
	ProviderOTel    = "otel"
	ProviderDDTrace = "ddtrace"
)

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "unknown"
}

// ResolveTracingProvider always returns ProviderNone when compiled with -tags no_apm.
func ResolveTracingProvider() string {
	return ProviderNone
}

// DDTraceServiceName resolves the service name from environment or default.
func DDTraceServiceName(defaultService string) string {
	return firstNonEmpty(os.Getenv("DD_SERVICE"), os.Getenv("OTEL_SERVICE_NAME"), defaultService)
}

// DDTraceAgentAddr builds host:port for DataKit DDTrace ingestion.
func DDTraceAgentAddr() string {
	host := firstNonEmpty(os.Getenv("DD_AGENT_HOST"), "datakit-service.datakit.svc")
	port := strings.TrimSpace(os.Getenv("DD_TRACE_AGENT_PORT"))
	if port == "" {
		port = "9529"
	}
	return net.JoinHostPort(host, port)
}

// Configure is a no-op when compiled with -tags no_apm.
func Configure(context.Context, string) (func(context.Context) error, error) {
	return func(context.Context) error { return nil }, nil
}

// ConfigureTracing is a no-op when compiled with -tags no_apm.
func ConfigureTracing(context.Context, string) (func(context.Context) error, string, error) {
	return func(context.Context) error { return nil }, ProviderNone, nil
}

// WrapHTTPHandler returns next unchanged when compiled with -tags no_apm.
func WrapHTTPHandler(_ string, _ string, _ string, next http.Handler) http.Handler {
	return next
}

// WrapHTTPClient returns client unchanged when compiled with -tags no_apm.
func WrapHTTPClient(client *http.Client, _ string) *http.Client {
	if client == nil {
		return &http.Client{}
	}
	return client
}

// OpenPostgres opens a plain pgx connection pool when compiled with -tags no_apm.
func OpenPostgres(dsn string) (*sql.DB, error) {
	return sql.Open("pgx", dsn)
}

// TraceLogAttrs returns runtime log attributes when compiled with -tags no_apm.
func TraceLogAttrs(context.Context) []any {
	return RuntimeLogAttrs("web-saas-billing")
}

// RuntimeLogAttrs returns structured log identity attributes.
func RuntimeLogAttrs(defaultService string) []any {
	serviceName := firstNonEmpty(os.Getenv("DD_SERVICE"), os.Getenv("OTEL_SERVICE_NAME"), defaultService)
	environment := firstNonEmpty(os.Getenv("DD_ENV"), os.Getenv("OTEL_ENVIRONMENT"), os.Getenv("DEPLOY_ENV"), os.Getenv("RUNTIME_ENV"), os.Getenv("ENVIRONMENT"))
	instance := firstNonEmpty(os.Getenv("OTEL_SERVICE_INSTANCE_ID"), os.Getenv("INSTANCE"), os.Getenv("HOSTNAME"), serviceName)
	return []any{"service.name", serviceName, "service_name", serviceName, "environment", environment, "instance", instance}
}

// DDTraceSpanAttrs returns nil when compiled with -tags no_apm.
func DDTraceSpanAttrs(context.Context, string) []any {
	return nil
}

// RequestLogger records access logs without APM span fields when compiled with -tags no_apm.
func RequestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "latency", time.Since(start)}
		attrs = append(attrs, TraceLogAttrs(r.Context())...)
		logger.InfoContext(r.Context(), "request", attrs...)
	})
}
