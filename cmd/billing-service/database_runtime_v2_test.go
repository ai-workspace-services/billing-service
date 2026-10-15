package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
)

// Exercise the real reviewed schema on an explicitly disposable local server.
// No CI credentials or production database inputs are accepted by this test.
func TestNativeV2CheckpointPostgres(t *testing.T) {
	dsn := os.Getenv("BILLING_NATIVE_V2_TEST_DSN")
	if dsn == "" {
		t.Skip("disposable local V2 database not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "55432" || u.Path != "/account" {
		t.Fatal("only the isolated local PostgreSQL fixture is permitted")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&n); err != nil || n != 0 {
		t.Fatal("fixture must start empty")
	}
	for _, key := range []string{"BILLING_NATIVE_V2_ACCOUNTS_SQL", "BILLING_NATIVE_V2_COST_SQL"} {
		body, err := os.ReadFile(os.Getenv(key))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("CREATE TABLE schema_migrations(version bigint PRIMARY KEY, dirty boolean NOT NULL); INSERT INTO schema_migrations VALUES(2026101502,false)"); err != nil {
		t.Fatal(err)
	}
	if err = verifyNativeRuntime(ctx, db); err != nil {
		t.Fatalf("reviewed V2 schema refused: %v", err)
	}
	for _, mutation := range []struct{ change, undo string }{
		{"UPDATE schema_migrations SET dirty=true", "UPDATE schema_migrations SET dirty=false"},
		{"UPDATE schema_migrations SET version=2026101503", "UPDATE schema_migrations SET version=2026101502"},
		{"CREATE TABLE unreviewed_extra(id int)", "DROP TABLE unreviewed_extra"},
		{"ALTER TABLE cloud_vendor_costs ADD COLUMN unreviewed_extra int", "ALTER TABLE cloud_vendor_costs DROP COLUMN unreviewed_extra"},
	} {
		if _, err = db.Exec(mutation.change); err != nil {
			t.Fatal(err)
		}
		if err = verifyNativeRuntime(ctx, db); err == nil {
			t.Fatalf("unreviewed catalog mutation accepted: %s", mutation.change)
		}
		if _, err = db.Exec(mutation.undo); err != nil {
			t.Fatal(err)
		}
	}
	if err = verifyNativeRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
}
