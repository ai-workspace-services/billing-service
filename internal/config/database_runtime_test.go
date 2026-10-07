package config

import (
	"billing-service/internal/dbruntime"
	"strings"
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

func TestStandbyLoadsWithoutBusinessTokenAndPrimaryStillRequiresIt(t *testing.T) {
	// Prevent dotenv files from supplying credentials to this role fixture.
	t.Chdir(t.TempDir())
	dsn := "postgres://managed_probe@127.0.0.1:1/account?sslmode=disable"
	identity, err := dbruntime.ConnectionIdentity(dsn)
	if err != nil {
		t.Fatal("fixture identity unavailable")
	}
	t.Setenv("DATABASE_RUNTIME_ROLE", "standby")
	t.Setenv("DATABASE_BACKGROUND_WRITERS", "false")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("DATABASE_IDENTITY_SHA256", identity)
	t.Setenv("SUPABASE_CONNECT_URI", "")
	t.Setenv("SUPABASE_CONNECT_URL", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	t.Setenv("BILLING_INGEST_MODE", "push")
	cfg, err := Load()
	if err != nil || cfg.InternalServiceToken != "" || cfg.DatabaseRuntime.Role != dbruntime.Standby || cfg.DatabaseRuntime.MayRunBackgroundWriters() {
		t.Fatal("credential-free standby configuration refused")
	}
	for _, role := range []string{dbruntime.Primary, ""} {
		t.Setenv("DATABASE_RUNTIME_ROLE", role)
		if role == "" {
			t.Setenv("DATABASE_BACKGROUND_WRITERS", "")
			t.Setenv("DATABASE_IDENTITY_SHA256", "")
		}
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "INTERNAL_SERVICE_TOKEN is required") {
			t.Fatal("business role admitted missing internal token")
		}
	}
}
