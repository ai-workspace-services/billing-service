package service

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"billing-service/internal/model"
)

func TestSuspendSweepEscalatesProlongedArrears(t *testing.T) {
	repo := newMemoryRepo()
	now := time.Now().UTC()
	overdue := now.Add(-15 * 24 * time.Hour)
	repo.quotas["overdue"] = model.QuotaState{
		AccountUUID:   "overdue",
		Arrears:       true,
		ArrearsSince:  &overdue,
		ThrottleState: "throttled",
		SuspendState:  "active",
	}

	syncer := NewSuspendSyncer(repo, 14*24*time.Hour, time.Hour, slog.Default())
	syncer.sweep(context.Background())

	if got := repo.quotas["overdue"].SuspendState; got != "suspended" {
		t.Fatalf("expected suspend_state=suspended after threshold, got %q", got)
	}
}

func TestSuspendSweepLeavesRecentArrearsAlone(t *testing.T) {
	repo := newMemoryRepo()
	now := time.Now().UTC()
	recent := now.Add(-3 * 24 * time.Hour)
	repo.quotas["recent"] = model.QuotaState{
		AccountUUID:   "recent",
		Arrears:       true,
		ArrearsSince:  &recent,
		ThrottleState: "throttled",
		SuspendState:  "active",
	}

	syncer := NewSuspendSyncer(repo, 14*24*time.Hour, time.Hour, slog.Default())
	syncer.sweep(context.Background())

	if got := repo.quotas["recent"].SuspendState; got != "active" {
		t.Fatalf("expected suspend_state=active within grace period, got %q", got)
	}
}

func TestSuspendSweepIgnoresArrearsWithoutEpisodeStart(t *testing.T) {
	// Legacy rows flagged arrears before the arrears_since column existed have
	// no episode start; the sweep must not guess and must leave them active.
	repo := newMemoryRepo()
	repo.quotas["legacy"] = model.QuotaState{
		AccountUUID:   "legacy",
		Arrears:       true,
		ThrottleState: "throttled",
		SuspendState:  "active",
	}

	syncer := NewSuspendSyncer(repo, 14*24*time.Hour, time.Hour, slog.Default())
	syncer.sweep(context.Background())

	if got := repo.quotas["legacy"].SuspendState; got != "active" {
		t.Fatalf("expected legacy arrears row to stay active, got %q", got)
	}
}

func TestSuspendSweepSkipsClearedAndAlreadySuspended(t *testing.T) {
	repo := newMemoryRepo()
	now := time.Now().UTC()
	overdue := now.Add(-30 * 24 * time.Hour)
	repo.quotas["paid-up"] = model.QuotaState{
		AccountUUID:  "paid-up",
		Arrears:      false,
		SuspendState: "active",
	}
	repo.quotas["already"] = model.QuotaState{
		AccountUUID:  "already",
		Arrears:      true,
		ArrearsSince: &overdue,
		SuspendState: "suspended",
	}

	syncer := NewSuspendSyncer(repo, 14*24*time.Hour, time.Hour, slog.Default())
	syncer.sweep(context.Background())

	if got := repo.quotas["paid-up"].SuspendState; got != "active" {
		t.Fatalf("expected cleared account to stay active, got %q", got)
	}
	if got := repo.quotas["already"].SuspendState; got != "suspended" {
		t.Fatalf("expected suspended account to stay suspended, got %q", got)
	}
}
