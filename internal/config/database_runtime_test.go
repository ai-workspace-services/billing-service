package config

import (
	"billing-service/internal/dbruntime"
	"testing"
)

func TestDatabaseRoleUsesCanonicalURLAndExplicitBackground(t *testing.T) {
	dsn := "postgres://fixture:synthetic@localhost:5432/account?sslmode=disable"
	identity, _ := dbruntime.ConnectionIdentity(dsn)
	t.Setenv("DATABASE_RUNTIME_ROLE", "primary")
	t.Setenv("DATABASE_BACKGROUND_WRITERS", "false")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("DATABASE_IDENTITY_SHA256", identity)
	t.Setenv("SUPABASE_CONNECT_URI", "")
	t.Setenv("SUPABASE_CONNECT_URL", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "fixture-token")
	cfg, err := Load()
	if err != nil || cfg.DatabaseURL != dsn || cfg.DatabaseRuntime.MayRunBackgroundWriters() {
		t.Fatal("explicit native configuration refused")
	}
	t.Setenv("SUPABASE_CONNECT_URI", dsn)
	if _, err := Load(); err == nil {
		t.Fatal("managed runtime accepted lingering Supabase alias")
	}
}
