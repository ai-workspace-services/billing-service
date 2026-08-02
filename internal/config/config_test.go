package config

import "testing"

func TestLoadExporterSourcesFromJSON(t *testing.T) {
	sources, err := loadExporterSources("", `[{"source_id":"jp","base_url":"https://jp-xhttp-contabo.svc.plus","expected_node_id":"jp-xhttp-contabo.svc.plus","expected_env":"prod","enabled":true,"timeout_seconds":20}]`)
	if err != nil {
		t.Fatalf("load sources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(sources))
	}
	if sources[0].SourceID != "jp" || sources[0].BaseURL != "https://jp-xhttp-contabo.svc.plus" {
		t.Fatalf("unexpected source %#v", sources[0])
	}
	if sources[0].TimeoutSeconds != 20 {
		t.Fatalf("expected timeout 20, got %d", sources[0].TimeoutSeconds)
	}
}

func TestLoadExporterSourcesFallsBackToLegacyBaseURL(t *testing.T) {
	sources, err := loadExporterSources("http://127.0.0.1:8080", "")
	if err != nil {
		t.Fatalf("load legacy source: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(sources))
	}
	if sources[0].SourceID != "default" || sources[0].BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected source %#v", sources[0])
	}
}

func TestPushModeDoesNotRequireExporterSource(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "token")
	t.Setenv("BILLING_INGEST_MODE", "push")
	t.Setenv("EXPORTER_BASE_URL", "")
	t.Setenv("EXPORTER_SOURCES_JSON", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load push config: %v", err)
	}
	if cfg.IngestMode != "push" || cfg.PullEnabled {
		t.Fatalf("unexpected push config: %#v", cfg)
	}
	if len(cfg.ExporterSources) != 0 {
		t.Fatalf("push mode must not configure exporter pull sources: %#v", cfg.ExporterSources)
	}
}

func TestParseImageRefWithFullShaTag(t *testing.T) {
	tag, commit, version := parseImageRef("registry.example.com/billing-service:sha-0123456789abcdef0123456789abcdef01234567")
	if tag != "sha-0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected tag %q", tag)
	}
	if commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected commit %q", commit)
	}
	if version != commit {
		t.Fatalf("expected version to equal commit, got %q vs %q", version, commit)
	}
}

func TestParseImageRefRejectsIncompleteSha(t *testing.T) {
	tag, commit, version := parseImageRef("registry.example.com/billing-service:sha-1234")
	if tag != "sha-1234" || commit != "" || version != "" {
		t.Fatalf("expected partial parse failure, got tag=%q commit=%q version=%q", tag, commit, version)
	}
}
