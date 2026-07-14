package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type ExporterSource struct {
	SourceID       string
	BaseURL        string
	ExpectedNodeID string
	ExpectedEnv    string
	Enabled        bool
	TimeoutSeconds int
}

type Config struct {
	ImageRef                  string
	ImageTag                  string
	ImageCommit               string
	ImageVersion              string
	ExporterBaseURL           string
	ExporterSources           []ExporterSource
	InternalServiceToken      string
	DatabaseURL               string
	ListenAddr                string
	CollectInterval           time.Duration
	DefaultRegion             string
	SourceRevision            string
	PricePerByte              float64
	InitialIncludedQuotaBytes int64
	InitialBalance            float64

	// AWS FinOps Config
	AWSAccessKeyID     string
	AWSSecretAccessKey string

	// GCP FinOps Config
	GCPCredentialsJSON string
	GCPBillingProject  string
	GCPBillingDataset  string
	GCPBillingTable    string

	// Azure FinOps Config
	AzureTenantID       string
	AzureClientID       string
	AzureClientSecret   string
	AzureSubscriptionID string

	// OpenCost is an allocation source, never an authoritative cloud bill.
	OpenCostEndpoint  string
	OpenCostAuthToken string
}

type rawExporterSource struct {
	SourceID       string `json:"source_id"`
	BaseURL        string `json:"base_url"`
	ExpectedNodeID string `json:"expected_node_id"`
	ExpectedEnv    string `json:"expected_env"`
	Enabled        *bool  `json:"enabled"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func Load() (Config, error) {
	appEnv := strings.TrimSpace(os.Getenv("APP_ENV"))
	if appEnv == "" {
		appEnv = "dev"
	}

	// Progressively load env files, allowing overrides from more specific ones.
	_ = godotenv.Load(".env." + appEnv + ".local")
	_ = godotenv.Load(".env." + appEnv)
	_ = godotenv.Load() // fallback to .env

	imageRef := strings.TrimSpace(os.Getenv("IMAGE"))
	imageTag, imageCommit, imageVersion := parseImageRef(imageRef)
	cfg := Config{
		ImageRef:             imageRef,
		ImageTag:             imageTag,
		ImageCommit:          imageCommit,
		ImageVersion:         imageVersion,
		ExporterBaseURL:      strings.TrimRight(strings.TrimSpace(os.Getenv("EXPORTER_BASE_URL")), "/"),
		InternalServiceToken: strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_TOKEN")),
		DatabaseURL:          strings.TrimSpace(os.Getenv("DATABASE_URL")),
		ListenAddr:           strings.TrimSpace(os.Getenv("LISTEN_ADDR")),
		DefaultRegion:        strings.TrimSpace(os.Getenv("DEFAULT_REGION")),
		SourceRevision:       strings.TrimSpace(os.Getenv("SOURCE_REVISION")),

		// AWS FinOps Config
		AWSAccessKeyID:     strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")),
		AWSSecretAccessKey: strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY")),

		// GCP FinOps Config
		GCPCredentialsJSON: strings.TrimSpace(os.Getenv("GCP_CREDENTIALS_JSON")),
		GCPBillingProject:  strings.TrimSpace(os.Getenv("GCP_BILLING_PROJECT")),
		GCPBillingDataset:  strings.TrimSpace(os.Getenv("GCP_BILLING_DATASET")),
		GCPBillingTable:    strings.TrimSpace(os.Getenv("GCP_BILLING_TABLE")),

		// Azure FinOps Config
		AzureTenantID:       strings.TrimSpace(os.Getenv("AZURE_TENANT_ID")),
		AzureClientID:       strings.TrimSpace(os.Getenv("AZURE_CLIENT_ID")),
		AzureClientSecret:   strings.TrimSpace(os.Getenv("AZURE_CLIENT_SECRET")),
		AzureSubscriptionID: strings.TrimSpace(os.Getenv("AZURE_SUBSCRIPTION_ID")),

		OpenCostEndpoint:  strings.TrimRight(strings.TrimSpace(os.Getenv("OPENCOST_ENDPOINT")), "/"),
		OpenCostAuthToken: strings.TrimSpace(os.Getenv("OPENCOST_AUTH_TOKEN")),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8081"
	}
	if cfg.SourceRevision == "" {
		cfg.SourceRevision = "billing-service-v1"
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.InternalServiceToken == "" {
		return Config{}, fmt.Errorf("INTERNAL_SERVICE_TOKEN is required")
	}

	sources, err := loadExporterSources(cfg.ExporterBaseURL, strings.TrimSpace(os.Getenv("EXPORTER_SOURCES_JSON")))
	if err != nil {
		return Config{}, err
	}
	cfg.ExporterSources = sources

	interval := strings.TrimSpace(os.Getenv("COLLECT_INTERVAL"))
	if interval == "" {
		cfg.CollectInterval = time.Minute
	} else {
		parsed, err := time.ParseDuration(interval)
		if err != nil {
			return Config{}, fmt.Errorf("parse COLLECT_INTERVAL: %w", err)
		}
		cfg.CollectInterval = parsed
	}

	cfg.PricePerByte = parseFloatEnv("PRICE_PER_BYTE", 0)
	cfg.InitialBalance = parseFloatEnv("INITIAL_BALANCE", 0)
	cfg.InitialIncludedQuotaBytes = parseIntEnv("INITIAL_INCLUDED_QUOTA_BYTES", 0)
	return cfg, nil
}

var fullSHARegexp = regexp.MustCompile(`^[a-f0-9]{40}$`)

func parseImageRef(imageRef string) (tag, commit, version string) {
	trimmed := strings.TrimSpace(imageRef)
	if trimmed == "" {
		return "", "", ""
	}
	colon := strings.LastIndex(trimmed, ":")
	if colon < 0 || colon == len(trimmed)-1 {
		return "", "", ""
	}
	tag = trimmed[colon+1:]
	switch {
	case strings.HasPrefix(tag, "sha-") && fullSHARegexp.MatchString(strings.TrimPrefix(tag, "sha-")):
		commit = strings.TrimPrefix(tag, "sha-")
	case fullSHARegexp.MatchString(tag):
		commit = tag
	}
	if commit != "" {
		version = commit
	}
	return tag, commit, version
}

func loadExporterSources(legacyBaseURL, rawJSON string) ([]ExporterSource, error) {
	if rawJSON == "" {
		if legacyBaseURL == "" {
			return nil, fmt.Errorf("EXPORTER_SOURCES_JSON or EXPORTER_BASE_URL is required")
		}
		return []ExporterSource{{
			SourceID:       "default",
			BaseURL:        strings.TrimRight(strings.TrimSpace(legacyBaseURL), "/"),
			Enabled:        true,
			TimeoutSeconds: 15,
		}}, nil
	}

	var rawSources []rawExporterSource
	if err := json.Unmarshal([]byte(rawJSON), &rawSources); err != nil {
		return nil, fmt.Errorf("parse EXPORTER_SOURCES_JSON: %w", err)
	}
	if len(rawSources) == 0 {
		return nil, fmt.Errorf("EXPORTER_SOURCES_JSON must define at least one source")
	}

	sources := make([]ExporterSource, 0, len(rawSources))
	for _, raw := range rawSources {
		source := ExporterSource{
			SourceID:       strings.TrimSpace(raw.SourceID),
			BaseURL:        strings.TrimRight(strings.TrimSpace(raw.BaseURL), "/"),
			ExpectedNodeID: strings.TrimSpace(raw.ExpectedNodeID),
			ExpectedEnv:    strings.TrimSpace(raw.ExpectedEnv),
			Enabled:        true,
			TimeoutSeconds: raw.TimeoutSeconds,
		}
		if raw.Enabled != nil {
			source.Enabled = *raw.Enabled
		}
		if source.SourceID == "" {
			return nil, fmt.Errorf("EXPORTER_SOURCES_JSON source_id is required")
		}
		if source.BaseURL == "" {
			return nil, fmt.Errorf("EXPORTER_SOURCES_JSON base_url is required for source %s", source.SourceID)
		}
		if source.TimeoutSeconds <= 0 {
			source.TimeoutSeconds = 15
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func parseFloatEnv(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseIntEnv(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
