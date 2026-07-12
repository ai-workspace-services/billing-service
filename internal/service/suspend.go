package service

import (
	"context"
	"log/slog"
	"time"

	"billing-service/internal/repository"
)

// SuspendSyncer is the P1.5 "arrears -> execution" closing step: it
// periodically escalates accounts that have been in arrears longer than the
// configured grace threshold to suspend_state=suspended. accounts.svc.plus
// reads suspend_state when building agent/xray sync payloads and drops
// suspended accounts, which is what actually cuts off service; billing-service
// only owns the time-based decision to flip the flag.
type SuspendSyncer struct {
	repo      repository.Repository
	threshold time.Duration
	interval  time.Duration
	log       *slog.Logger
}

func NewSuspendSyncer(repo repository.Repository, threshold, interval time.Duration, logger *slog.Logger) *SuspendSyncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &SuspendSyncer{
		repo:      repo,
		threshold: threshold,
		interval:  interval,
		log:       logger.With("component", "suspend_syncer"),
	}
}

func (s *SuspendSyncer) Start(ctx context.Context) {
	s.log.Info("Starting arrears suspend syncer", "threshold", s.threshold, "interval", s.interval)

	s.sweep(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log.Info("Suspend syncer shutting down")
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *SuspendSyncer) sweep(ctx context.Context) {
	accounts, err := s.repo.ListArrearsAccounts(ctx)
	if err != nil {
		s.log.Error("Failed to list arrears accounts", "err", err)
		return
	}

	now := time.Now().UTC()
	for _, quota := range accounts {
		if quota.ArrearsSince == nil || now.Sub(*quota.ArrearsSince) < s.threshold {
			continue
		}
		quota.SuspendState = "suspended"
		if err := s.repo.UpsertQuotaState(ctx, quota); err != nil {
			s.log.Error("Failed to suspend account for prolonged arrears", "accountUUID", quota.AccountUUID, "err", err)
			continue
		}
		s.log.Warn("Suspended account for prolonged arrears", "accountUUID", quota.AccountUUID, "arrearsSince", quota.ArrearsSince)
	}
}
