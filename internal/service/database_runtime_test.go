package service

import (
	"billing-service/internal/config"
	"billing-service/internal/dbruntime"
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestDatabaseRolePausedSuppressesBackgroundWriters(t *testing.T) {
	for _, role := range []string{dbruntime.Primary, dbruntime.Standby} {
		cfg := config.Config{DatabaseRuntime: dbruntime.Config{Role: role}, PullEnabled: true, IngestMode: "pull", CollectInterval: time.Millisecond}
		// Nil repositories/sources make accidental background execution fail.
		New(cfg, nil, nil).Start(context.Background())
		NewFinOpsSyncer(nil, slog.Default(), cfg).Start(context.Background())
	}
}
