package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"billing-service/internal/config"
	"billing-service/internal/dbruntime"
)

func runtimeFixtureConfig(t *testing.T, role, dsn string) config.Config {
	t.Helper()
	identity, err := dbruntime.ConnectionIdentity(dsn)
	if err != nil {
		t.Fatal("fixture connection shape differs")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture listener unavailable")
	}
	addr := ln.Addr().String()
	ln.Close()
	t.Setenv("DATABASE_RUNTIME_ROLE", role)
	t.Setenv("DATABASE_BACKGROUND_WRITERS", "false")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("DATABASE_IDENTITY_SHA256", identity)
	t.Setenv("SUPABASE_CONNECT_URI", "")
	t.Setenv("SUPABASE_CONNECT_URL", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "synthetic-runtime-fixture")
	if role == dbruntime.Standby {
		t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	}
	t.Setenv("LISTEN_ADDR", addr)
	t.Setenv("IMAGE", "ghcr.io/ai-workspace-services/billing-service:sha-"+strings.Repeat("b", 40))
	t.Setenv("BILLING_INGEST_MODE", "push")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal("fixture configuration refused")
	}
	return cfg
}

func startRuntimeFixture(t *testing.T, cfg config.Config) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runBilling(ctx, cfg) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("Billing fixture returned an error")
			}
		case <-time.After(7 * time.Second):
			t.Error("Billing fixture did not stop")
		}
	}
	base := "http://" + cfg.ListenAddr
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			cancel()
			t.Fatal("Billing stopped before runtime probe")
		default:
		}
		resp, err := client.Get(base + "/api/ping")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base, stop
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	t.Fatal("Billing runtime probe never became available")
	return "", stop
}

func TestManagedBillingStandbyWithoutDatabase(t *testing.T) {
	cfg := runtimeFixtureConfig(t, "standby", "postgres://fixture:synthetic@unreachable.invalid:5432/postgres?sslmode=require")
	base, stop := startRuntimeFixture(t, cfg)
	defer stop()
	for _, path := range []string{"/readyz", "/v1/status", "/v1/jobs/collect-and-rate", "/v1/jobs/reconcile", "/v1/ingest/snapshots"} {
		for _, method := range []string{"GET", "POST", "OPTIONS"} {
			req, _ := http.NewRequest(method, base+path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal("standby request unavailable")
			}
			resp.Body.Close()
			if resp.StatusCode != 503 {
				t.Fatal("standby admitted business or readiness")
			}
		}
	}
	resp, err := http.Get(base + "/api/ping")
	if err != nil {
		t.Fatal("standby ping unavailable")
	}
	defer resp.Body.Close()
	var body map[string]any
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body["commit"] != strings.Repeat("b", 40) || body["database_role"] != "standby" || body["bootstrap_writes"] != false || body["background_writers"] != false || body["schema_version"] != float64(0) {
		t.Fatal("standby role or release metadata differs")
	}
}

func TestManagedBillingNativeStartupPreservesBusiness(t *testing.T) {
	dsn := os.Getenv("BILLING_MANAGED_RUNTIME_TEST_DSN")
	if dsn == "" {
		t.Skip("disposable native fixture not configured")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" ||
		(parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1") || parsed.Port() != "5432" || parsed.Path != "/account" || parsed.User.Username() != "postgres" {
		t.Fatal("fixture refuses a non-disposable database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("fixture database unavailable")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var count int
	if db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f')`).Scan(&count) != nil || count != 0 {
		t.Fatal("fixture requires a fresh empty public schema")
	}
	accounts, err := os.ReadFile(os.Getenv("BILLING_NATIVE_ACCOUNTS_SQL"))
	if err != nil {
		t.Fatal("fixed Accounts SQL unavailable")
	}
	accountsSHA := sha256.Sum256(accounts)
	if hex.EncodeToString(accountsSHA[:]) != "842cef3beb98ef819dc854ecdf5f85683233641a0cd85a9156b30ad59f7e0206" {
		t.Fatal("fixed Accounts SQL differs")
	}
	billing, err := os.ReadFile("../../sql/migrations/2026100701_cloud_vendor_costs.up.sql")
	if err != nil {
		t.Fatal("owned Billing SQL unavailable")
	}
	billingSHA := sha256.Sum256(billing)
	if hex.EncodeToString(billingSHA[:]) != "a7133f3ef2ea9013a055cfd1442a7488d2b837f289e0f5d9b61624d4fde9bc53" {
		t.Fatal("owned Billing SQL differs")
	}
	for _, body := range []string{string(accounts), string(billing), `CREATE TABLE public.schema_migrations(version bigint NOT NULL PRIMARY KEY,dirty boolean NOT NULL);
INSERT INTO public.schema_migrations VALUES(2026100701,false);
INSERT INTO public.users(uuid,username,password,email,proxy_uuid,active,role,groups)
VALUES('00000000-0000-0000-0000-000000000101','preserved-user','synthetic-password','fixture@svc.plus','00000000-0000-0000-0000-000000000201',true,'admin','["preserved"]');
INSERT INTO public.billing_ledger(id,account_uuid,bucket_start,bucket_end,entry_type,rated_bytes,amount_delta,balance_after)
VALUES('00000000-0000-0000-0000-000000000301','00000000-0000-0000-0000-000000000101','2026-10-01T00:00:00Z','2026-10-01T00:01:00Z','usage',9007199254740993,0.125,12.75);
INSERT INTO public.cloud_vendor_costs(provider,account_id,service_name,usage_start_time,usage_end_time,cost_amount) VALUES('fixture','synthetic','preserved','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z',12.75);
CREATE ROLE billing_runtime_readonly_ci LOGIN PASSWORD 'synthetic-runtime' NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT CONNECT ON DATABASE account TO billing_runtime_readonly_ci;
GRANT USAGE ON SCHEMA public TO billing_runtime_readonly_ci;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO billing_runtime_readonly_ci`} {
		if _, err = db.ExecContext(ctx, body); err != nil {
			t.Fatal("native synthetic fixture initialization failed")
		}
	}
	fingerprint := func() string {
		t.Helper()
		h := sha256.New()
		rows, err := db.QueryContext(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r' ORDER BY c.relname`)
		if err != nil {
			t.Fatal("fixture table names unavailable")
		}
		var names []string
		for rows.Next() {
			var name string
			if rows.Scan(&name) != nil {
				t.Fatal("fixture table name invalid")
			}
			names = append(names, name)
		}
		if rows.Err() != nil {
			t.Fatal("fixture table scope incomplete")
		}
		rows.Close()
		for _, name := range names {
			var hash string
			if db.QueryRowContext(ctx, `SELECT encode(digest(coalesce(string_agg(value,E'\n' ORDER BY value),''),'sha256'),'hex') FROM (SELECT to_jsonb(t)::text value FROM public."`+strings.ReplaceAll(name, `"`, `""`)+`" t) rows`).Scan(&hash) != nil {
				t.Fatal("fixture business hash unavailable")
			}
			h.Write([]byte(name + hash))
		}
		var catalog string
		if db.QueryRowContext(ctx, `SELECT coalesce(string_agg(v,E'\n' ORDER BY v),'') FROM (
SELECT to_jsonb(c)::text v FROM information_schema.columns c WHERE table_schema='public'
UNION ALL SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace='public'::regnamespace
UNION ALL SELECT indexdef FROM pg_indexes WHERE schemaname='public'
UNION ALL SELECT pg_get_functiondef(p.oid) FROM pg_proc p WHERE pronamespace='public'::regnamespace AND prokind='f') catalog`).Scan(&catalog) != nil {
			t.Fatal("fixture catalog hash unavailable")
		}
		h.Write([]byte(catalog))
		return hex.EncodeToString(h.Sum(nil))
	}
	before := fingerprint()
	parsed.User = url.UserPassword("billing_runtime_readonly_ci", "synthetic-runtime")
	cfg := runtimeFixtureConfig(t, "primary", parsed.String())
	base, stop := startRuntimeFixture(t, cfg)
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatal("native Billing readiness unavailable")
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("native Billing not ready")
	}
	stop()
	if before != fingerprint() {
		t.Fatal("managed Billing startup changed business or schema")
	}
	if verifyNativeRuntime(ctx, db) != nil {
		t.Fatal("exact native Billing refused")
	}
	if _, err = db.ExecContext(ctx, "ALTER TABLE public.cloud_vendor_costs DROP CONSTRAINT uq_cloud_vendor_cost_period"); err != nil {
		t.Fatal("fixture key tamper failed")
	}
	if verifyNativeRuntime(ctx, db) == nil {
		t.Fatal("Billing accepted an incompatible upsert key")
	}
	if _, err = db.ExecContext(ctx, "UPDATE public.schema_migrations SET dirty=true"); err != nil {
		t.Fatal("fixture checkpoint tamper failed")
	}
	if verifyNativeRuntime(ctx, db) == nil {
		t.Fatal("Billing accepted a dirty checkpoint")
	}
}
