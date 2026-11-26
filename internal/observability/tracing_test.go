//go:build !no_apm

package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveTracingProvider(t *testing.T) {
	t.Run("prefers OTel when both are set", func(t *testing.T) {
		t.Setenv("TRACING_PROVIDER", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://otel:4317")
		t.Setenv("OTEL_SDK_DISABLED", "")
		t.Setenv("DD_AGENT_HOST", "datakit-service.observability-sit.svc")
		if got := ResolveTracingProvider(); got != ProviderOTel {
			t.Fatalf("got %q, want %q", got, ProviderOTel)
		}
	})

	t.Run("falls back to ddtrace when OTel is absent", func(t *testing.T) {
		t.Setenv("TRACING_PROVIDER", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("DD_AGENT_HOST", "datakit-service.observability-sit.svc")
		t.Setenv("DD_TRACE_ENABLED", "")
		if got := ResolveTracingProvider(); got != ProviderDDTrace {
			t.Fatalf("got %q, want %q", got, ProviderDDTrace)
		}
	})

	t.Run("defaults to none", func(t *testing.T) {
		t.Setenv("TRACING_PROVIDER", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("DD_AGENT_HOST", "")
		if got := ResolveTracingProvider(); got != ProviderNone {
			t.Fatalf("got %q, want %q", got, ProviderNone)
		}
	})
}

func TestWrapHTTPHandler(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, provider := range []string{ProviderNone, ProviderOTel, ProviderDDTrace} {
		h := WrapHTTPHandler(provider, "web-saas-billing", "billing.request", base)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("provider %s: got status %d", provider, rec.Code)
		}
	}
}

func TestWrapHTTPClient(t *testing.T) {
	t.Run("ddtrace wraps transport", func(t *testing.T) {
		t.Setenv("TRACING_PROVIDER", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("DD_AGENT_HOST", "datakit-service.observability-prod.svc")
		t.Setenv("DD_TRACE_ENABLED", "true")
		base := &http.Client{}
		got := WrapHTTPClient(base, "web-saas-billing")
		if got == nil || got.Transport == nil {
			t.Fatalf("expected wrapped client transport for ddtrace")
		}
	})
}
