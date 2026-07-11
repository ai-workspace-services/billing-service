package service

import (
	"context"
	"log/slog"
	"time"

	"billing-service/internal/model"
	"billing-service/internal/repository"
)

// FinOpsSyncer is responsible for fetching cost data from cloud providers
// (AWS, GCP, Azure) and writing it to the database for AI auditing.
type FinOpsSyncer struct {
	repo repository.Repository
	log  *slog.Logger
}

// NewFinOpsSyncer creates a new FinOpsSyncer.
func NewFinOpsSyncer(repo repository.Repository, logger *slog.Logger) *FinOpsSyncer {
	return &FinOpsSyncer{
		repo: repo,
		log:  logger.With("component", "finops_syncer"),
	}
}

// Start begins the background synchronization process.
func (s *FinOpsSyncer) Start(ctx context.Context) {
	s.log.Info("Starting FinOps multi-cloud billing syncer")

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	// Initial sync on startup
	s.syncAllProviders(ctx)

	for {
		select {
		case <-ctx.Done():
			s.log.Info("FinOps syncer shutting down")
			return
		case <-ticker.C:
			s.syncAllProviders(ctx)
		}
	}
}

func (s *FinOpsSyncer) syncAllProviders(ctx context.Context) {
	s.log.Info("Starting daily sync for all cloud providers")

	// 1. Sync AWS Costs
	if err := s.syncAWS(ctx); err != nil {
		s.log.Error("Failed to sync AWS costs", "err", err)
	}

	// 2. Sync GCP Costs
	if err := s.syncGCP(ctx); err != nil {
		s.log.Error("Failed to sync GCP costs", "err", err)
	}

	// 3. Sync Azure Costs
	if err := s.syncAzure(ctx); err != nil {
		s.log.Error("Failed to sync Azure costs", "err", err)
	}
}

// syncAWS is a stub for AWS Cost Explorer API integration.
func (s *FinOpsSyncer) syncAWS(ctx context.Context) error {
	s.log.Info("Syncing AWS costs (stub)")

	// TODO: Initialize AWS SDK session, call Cost Explorer GetCostAndUsage
	// and map the results to model.CloudVendorCost.
	// Example dummy data:
	dummyCost := model.CloudVendorCost{
		Provider:       "aws",
		AccountID:      "123456789012",
		ServiceName:    "AmazonEC2",
		Region:         "us-east-1",
		UsageStartTime: time.Now().Add(-24 * time.Hour).Truncate(24 * time.Hour),
		UsageEndTime:   time.Now().Truncate(24 * time.Hour),
		CostAmount:     120.50,
		Currency:       "USD",
	}

	return s.repo.UpsertCloudVendorCost(ctx, dummyCost)
}

// syncGCP is a stub for GCP Cloud Billing API integration.
func (s *FinOpsSyncer) syncGCP(ctx context.Context) error {
	s.log.Info("Syncing GCP costs (stub)")
	// TODO: Integrate GCP Billing / BigQuery
	return nil
}

// syncAzure is a stub for Azure Cost Management API integration.
func (s *FinOpsSyncer) syncAzure(ctx context.Context) error {
	s.log.Info("Syncing Azure costs (stub)")
	// TODO: Integrate Azure Cost Management
	return nil
}
