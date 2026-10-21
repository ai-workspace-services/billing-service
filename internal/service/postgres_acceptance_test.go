package service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"billing-service/internal/config"
	"billing-service/internal/model"
	"billing-service/internal/repository"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresAcceptanceWritesAccountingTables(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}

	bootstrapPath := filepath.Join("..", "..", "testdata", "postgres", "init.sql")
	bootstrapSQL, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap sql: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(bootstrapSQL)); err != nil {
		t.Fatalf("apply bootstrap sql: %v", err)
	}

	accountUUID := "11111111-1111-1111-1111-111111111111"
	if _, err := db.ExecContext(ctx, `
		DELETE FROM billing_source_sync_state;
		DELETE FROM billing_ledger;
		DELETE FROM traffic_minute_buckets;
		DELETE FROM traffic_stat_checkpoints;
		DELETE FROM account_quota_states;
		DELETE FROM users;
	`); err != nil {
		t.Fatalf("reset tables: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (uuid, username, password, email, proxy_uuid)
		VALUES ($1, 'billing-test', 'irrelevant', 'billing@example.com', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa')
	`, accountUUID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	svc := New(config.Config{
		ExporterSources: []config.ExporterSource{{
			SourceID:       "default",
			BaseURL:        "https://jp-xhttp-contabo.svc.plus",
			ExpectedNodeID: "jp-node",
			ExpectedEnv:    "prod",
			Enabled:        true,
			TimeoutSeconds: 15,
		}},
		InternalServiceToken:      "secret",
		DefaultRegion:             "",
		SourceRevision:            "billing-service-acceptance",
		PricePerByte:              0.5,
		InitialIncludedQuotaBytes: 1000,
		InitialBalance:            0,
	}, &fakeWindowSource{pagesBySource: map[string][]model.SnapshotWindowPage{
		"default": {singleSnapshotPage(model.Snapshot{
			CollectedAt: time.Date(2026, 4, 8, 11, 0, 45, 0, time.UTC),
			NodeID:      "jp-node",
			Env:         "prod",
			Samples: []model.Sample{{
				UUID:               accountUUID,
				Email:              "billing@example.com",
				InboundTag:         "premium",
				UplinkBytesTotal:   100,
				DownlinkBytesTotal: 50,
			}},
		})},
	}}, repository.NewPostgres(db))

	result, err := svc.RunCollectAndRate(ctx, "collect-and-rate")
	if err != nil {
		t.Fatalf("run collect-and-rate: %v", err)
	}
	if result.ProcessedSamples != 1 || result.WrittenMinutes != 1 {
		t.Fatalf("unexpected result %#v", result)
	}

	assertRowCount(t, db, "traffic_stat_checkpoints", 1)
	assertRowCount(t, db, "traffic_minute_buckets", 1)
	assertRowCount(t, db, "billing_ledger", 1)
	assertRowCount(t, db, "account_quota_states", 1)
	assertRowCount(t, db, "billing_source_sync_state", 1)

	var totalBytes int64
	if err := db.QueryRowContext(ctx, `SELECT total_bytes FROM traffic_minute_buckets LIMIT 1`).Scan(&totalBytes); err != nil {
		t.Fatalf("query total_bytes: %v", err)
	}
	if totalBytes != 150 {
		t.Fatalf("expected total_bytes 150, got %d", totalBytes)
	}

	// Verify exact duplicate snapshot replay against real PostgreSQL does not zero out bucket/ledger or double charge.
	replayResult, err := svc.IngestSnapshot(ctx, model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 11, 0, 45, 0, time.UTC),
		NodeID:      "jp-node",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			Email:              "billing@example.com",
			InboundTag:         "premium",
			UplinkBytesTotal:   100,
			DownlinkBytesTotal: 50,
		}},
	})
	if err != nil {
		t.Fatalf("replay ingest snapshot: %v", err)
	}
	if replayResult.ReplayedMinutes != 1 {
		t.Fatalf("expected 1 replayed minute on duplicate delivery, got %#v", replayResult)
	}
	if err := db.QueryRowContext(ctx, `SELECT total_bytes FROM traffic_minute_buckets LIMIT 1`).Scan(&totalBytes); err != nil {
		t.Fatalf("query total_bytes after replay: %v", err)
	}
	if totalBytes != 150 {
		t.Fatalf("expected total_bytes 150 preserved after duplicate replay, got %d", totalBytes)
	}

	// Verify same-minute increasing snapshot accumulates into the existing minute bucket and deducts only the incremental delta.
	sameMinuteResult, err := svc.IngestSnapshot(ctx, model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 11, 0, 58, 0, time.UTC),
		NodeID:      "jp-node",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			Email:              "billing@example.com",
			InboundTag:         "premium",
			UplinkBytesTotal:   220,
			DownlinkBytesTotal: 130,
		}},
	})
	if err != nil {
		t.Fatalf("same-minute ingest snapshot: %v", err)
	}
	if sameMinuteResult.WrittenMinutes != 1 {
		t.Fatalf("expected 1 written minute on same-minute increment, got %#v", sameMinuteResult)
	}
	if err := db.QueryRowContext(ctx, `SELECT total_bytes FROM traffic_minute_buckets LIMIT 1`).Scan(&totalBytes); err != nil {
		t.Fatalf("query total_bytes after same-minute increment: %v", err)
	}
	if totalBytes != 350 {
		t.Fatalf("expected accumulated total_bytes 350, got %d", totalBytes)
	}
	var remainingQuota int64
	if err := db.QueryRowContext(ctx, `SELECT remaining_included_quota FROM account_quota_states WHERE account_uuid = $1`, accountUUID).Scan(&remainingQuota); err != nil {
		t.Fatalf("query remaining_included_quota: %v", err)
	}
	if remainingQuota != 650 {
		t.Fatalf("expected remaining_included_quota 650, got %d", remainingQuota)
	}
}

func assertRowCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()

	var got int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatalf("count rows in %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("expected %d rows in %s, got %d", want, table, got)
	}
}
