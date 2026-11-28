package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
)

// This qualification writes only to an explicit disposable local PostgreSQL fixture.
func TestSITNativeSchemaQualification(t *testing.T) {
	dsn := os.Getenv("BILLING_SIT_SCHEMA_FIXTURE_DSN")
	if dsn == "" {
		t.Skip("local disposable SIT schema fixture not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() == "5432" || u.Port() == "" || u.Path != "/account" || u.User == nil || u.User.Username() != "postgres" {
		t.Fatal("fixture must use a local disposable PostgreSQL instance on a nondefault port")
	}
	native, err := os.ReadFile(os.Getenv("BILLING_SIT_ACCOUNTS_NATIVE_SQL"))
	if err != nil {
		t.Fatal("pinned Accounts native SQL unavailable")
	}
	sum := sha256.Sum256(native)
	if hex.EncodeToString(sum[:]) != "6d76daecc76cc71b958a95d654df4e6822660c3186a09e952b950d2ad2857798" {
		t.Fatal("Accounts native SQL does not match pinned SIT artifact")
	}
	billing, err := os.ReadFile("../../sql/migrations/2026100701_cloud_vendor_costs.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f')`).Scan(&count); err != nil || count != 0 {
		t.Fatal("fixture database must be empty")
	}
	for _, query := range []string{string(native), string(billing), `CREATE TABLE public.schema_migrations(version bigint PRIMARY KEY,dirty boolean NOT NULL); INSERT INTO public.schema_migrations VALUES (2026112401,false);`} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal("could not initialize disposable fixture:", err)
		}
	}
	version, err := verifyNativeRuntimeCheckpoint(ctx, db)
	if err != nil || version != 2026112401 {
		t.Fatalf("native fixture rejected: version=%d err=%v", version, err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE public.cloud_vendor_costs DROP CONSTRAINT uq_cloud_vendor_cost_period`); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyNativeRuntimeCheckpoint(ctx, db); err == nil {
		t.Fatal("schema with missing upsert constraint was accepted")
	}
}
