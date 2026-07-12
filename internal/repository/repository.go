package repository

import (
	"context"

	"billing-service/internal/model"
)

type Repository interface {
	GetCheckpoint(ctx context.Context, nodeID, accountUUID string) (*model.Checkpoint, error)
	UpsertCheckpoint(ctx context.Context, checkpoint model.Checkpoint) error
	UpsertMinuteBucket(ctx context.Context, bucket model.MinuteBucket) (bool, error)
	UpsertLedger(ctx context.Context, entry model.LedgerEntry) (bool, error)
	GetQuotaState(ctx context.Context, accountUUID string) (*model.QuotaState, error)
	UpsertQuotaState(ctx context.Context, state model.QuotaState) error
	// ListArrearsAccounts returns quota states currently flagged in arrears
	// that are not yet suspended, for the dunning sweep to escalate.
	ListArrearsAccounts(ctx context.Context) ([]model.QuotaState, error)
	GetBillingProfile(ctx context.Context, accountUUID string) (*model.BillingProfile, error)
	GetSourceSyncState(ctx context.Context, sourceID string) (*model.SourceSyncState, error)
	UpsertSourceSyncState(ctx context.Context, state model.SourceSyncState) error
	UpsertCloudVendorCost(ctx context.Context, cost model.CloudVendorCost) error
}
