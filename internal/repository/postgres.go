package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"billing-service/internal/model"
)

type Postgres struct {
	db *sql.DB
}

func NewPostgres(db *sql.DB) *Postgres {
	return &Postgres{db: db}
}

func (p *Postgres) GetCheckpoint(ctx context.Context, nodeID, accountUUID string) (*model.Checkpoint, error) {
	const query = `
		SELECT node_id, account_uuid, last_uplink_total, last_downlink_total, last_seen_at, xray_revision, reset_epoch
		FROM traffic_stat_checkpoints
		WHERE node_id = $1 AND account_uuid = $2`
	var checkpoint model.Checkpoint
	err := p.db.QueryRowContext(ctx, query, nodeID, accountUUID).Scan(
		&checkpoint.NodeID,
		&checkpoint.AccountUUID,
		&checkpoint.LastUplinkTotal,
		&checkpoint.LastDownlinkTotal,
		&checkpoint.LastSeenAt,
		&checkpoint.XrayRevision,
		&checkpoint.ResetEpoch,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &checkpoint, nil
}

func (p *Postgres) UpsertCheckpoint(ctx context.Context, checkpoint model.Checkpoint) error {
	const query = `
		INSERT INTO traffic_stat_checkpoints (
			node_id, account_uuid, last_uplink_total, last_downlink_total, last_seen_at, xray_revision, reset_epoch
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (node_id, account_uuid) DO UPDATE SET
			last_uplink_total = EXCLUDED.last_uplink_total,
			last_downlink_total = EXCLUDED.last_downlink_total,
			last_seen_at = EXCLUDED.last_seen_at,
			xray_revision = EXCLUDED.xray_revision,
			reset_epoch = EXCLUDED.reset_epoch,
			updated_at = now()`
	_, err := p.db.ExecContext(ctx, query,
		checkpoint.NodeID,
		checkpoint.AccountUUID,
		checkpoint.LastUplinkTotal,
		checkpoint.LastDownlinkTotal,
		checkpoint.LastSeenAt.UTC(),
		checkpoint.XrayRevision,
		checkpoint.ResetEpoch,
	)
	return err
}

func (p *Postgres) UpsertMinuteBucket(ctx context.Context, bucket model.MinuteBucket) (bool, error) {
	existed, err := p.minuteBucketExists(ctx, bucket)
	if err != nil {
		return false, err
	}

	const query = `
		INSERT INTO traffic_minute_buckets (
			bucket_start, node_id, account_uuid, region, line_code, uplink_bytes, downlink_bytes, total_bytes, multiplier, rating_status, source_revision
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (bucket_start, node_id, account_uuid, region, line_code) DO UPDATE SET
			uplink_bytes = EXCLUDED.uplink_bytes,
			downlink_bytes = EXCLUDED.downlink_bytes,
			total_bytes = EXCLUDED.total_bytes,
			multiplier = EXCLUDED.multiplier,
			rating_status = EXCLUDED.rating_status,
			source_revision = EXCLUDED.source_revision,
			updated_at = now()`
	_, err = p.db.ExecContext(ctx, query,
		bucket.BucketStart.UTC(),
		bucket.NodeID,
		bucket.AccountUUID,
		bucket.Region,
		bucket.LineCode,
		bucket.UplinkBytes,
		bucket.DownlinkBytes,
		bucket.TotalBytes,
		bucket.Multiplier,
		bucket.RatingStatus,
		bucket.SourceRevision,
	)
	return existed, err
}

func (p *Postgres) minuteBucketExists(ctx context.Context, bucket model.MinuteBucket) (bool, error) {
	const query = `
		SELECT 1
		FROM traffic_minute_buckets
		WHERE bucket_start = $1 AND node_id = $2 AND account_uuid = $3 AND region = $4 AND line_code = $5`
	var marker int
	err := p.db.QueryRowContext(ctx, query,
		bucket.BucketStart.UTC(),
		bucket.NodeID,
		bucket.AccountUUID,
		bucket.Region,
		bucket.LineCode,
	).Scan(&marker)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (p *Postgres) UpsertLedger(ctx context.Context, entry model.LedgerEntry) (bool, error) {
	existed, err := p.ledgerExists(ctx, entry.ID)
	if err != nil {
		return false, err
	}

	const query = `
		INSERT INTO billing_ledger (
			id, account_uuid, bucket_start, bucket_end, entry_type, rated_bytes, amount_delta, balance_after, pricing_rule_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			rated_bytes = EXCLUDED.rated_bytes,
			amount_delta = EXCLUDED.amount_delta,
			balance_after = EXCLUDED.balance_after,
			pricing_rule_version = EXCLUDED.pricing_rule_version`
	_, err = p.db.ExecContext(ctx, query,
		entry.ID,
		entry.AccountUUID,
		entry.BucketStart.UTC(),
		entry.BucketEnd.UTC(),
		entry.EntryType,
		entry.RatedBytes,
		entry.AmountDelta,
		entry.BalanceAfter,
		entry.PricingRuleVersion,
	)
	return existed, err
}

func (p *Postgres) ledgerExists(ctx context.Context, id string) (bool, error) {
	var marker int
	err := p.db.QueryRowContext(ctx, `SELECT 1 FROM billing_ledger WHERE id = $1`, id).Scan(&marker)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (p *Postgres) GetQuotaState(ctx context.Context, accountUUID string) (*model.QuotaState, error) {
	const query = `
		SELECT account_uuid, remaining_included_quota, current_balance, arrears, arrears_since, throttle_state, suspend_state, last_rated_bucket_at, effective_at
		FROM account_quota_states
		WHERE account_uuid = $1`
	var state model.QuotaState
	var lastRated sql.NullTime
	var arrearsSince sql.NullTime
	err := p.db.QueryRowContext(ctx, query, accountUUID).Scan(
		&state.AccountUUID,
		&state.RemainingIncludedQuota,
		&state.CurrentBalance,
		&state.Arrears,
		&arrearsSince,
		&state.ThrottleState,
		&state.SuspendState,
		&lastRated,
		&state.EffectiveAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if lastRated.Valid {
		value := lastRated.Time
		state.LastRatedBucketAt = &value
	}
	if arrearsSince.Valid {
		value := arrearsSince.Time
		state.ArrearsSince = &value
	}
	return &state, nil
}

func (p *Postgres) UpsertQuotaState(ctx context.Context, state model.QuotaState) error {
	const query = `
		INSERT INTO account_quota_states (
			account_uuid, remaining_included_quota, current_balance, arrears, arrears_since, throttle_state, suspend_state, last_rated_bucket_at, effective_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (account_uuid) DO UPDATE SET
			remaining_included_quota = EXCLUDED.remaining_included_quota,
			current_balance = EXCLUDED.current_balance,
			arrears = EXCLUDED.arrears,
			arrears_since = EXCLUDED.arrears_since,
			throttle_state = EXCLUDED.throttle_state,
			suspend_state = EXCLUDED.suspend_state,
			last_rated_bucket_at = EXCLUDED.last_rated_bucket_at,
			effective_at = EXCLUDED.effective_at,
			updated_at = now()`

	var lastRated interface{}
	if state.LastRatedBucketAt != nil {
		lastRated = state.LastRatedBucketAt.UTC()
	}
	var arrearsSince interface{}
	if state.ArrearsSince != nil {
		arrearsSince = state.ArrearsSince.UTC()
	}
	_, err := p.db.ExecContext(ctx, query,
		state.AccountUUID,
		state.RemainingIncludedQuota,
		state.CurrentBalance,
		state.Arrears,
		arrearsSince,
		state.ThrottleState,
		state.SuspendState,
		lastRated,
		state.EffectiveAt.UTC(),
	)
	return err
}

// ListArrearsAccounts returns every account currently flagged in arrears
// that has not yet been suspended, for the dunning sweep (SuspendSyncer) to
// evaluate against the configured grace threshold.
func (p *Postgres) ListArrearsAccounts(ctx context.Context) ([]model.QuotaState, error) {
	const query = `
		SELECT account_uuid, remaining_included_quota, current_balance, arrears, arrears_since, throttle_state, suspend_state, last_rated_bucket_at, effective_at
		FROM account_quota_states
		WHERE arrears = true AND suspend_state <> 'suspended'`
	rows, err := p.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []model.QuotaState
	for rows.Next() {
		var state model.QuotaState
		var lastRated sql.NullTime
		var arrearsSince sql.NullTime
		if err := rows.Scan(
			&state.AccountUUID,
			&state.RemainingIncludedQuota,
			&state.CurrentBalance,
			&state.Arrears,
			&arrearsSince,
			&state.ThrottleState,
			&state.SuspendState,
			&lastRated,
			&state.EffectiveAt,
		); err != nil {
			return nil, err
		}
		if lastRated.Valid {
			value := lastRated.Time
			state.LastRatedBucketAt = &value
		}
		if arrearsSince.Valid {
			value := arrearsSince.Time
			state.ArrearsSince = &value
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

func (p *Postgres) GetBillingProfile(ctx context.Context, accountUUID string) (*model.BillingProfile, error) {
	const query = `
		SELECT account_uuid, package_name, included_quota_bytes, base_price_per_byte, region_multiplier, line_multiplier, peak_multiplier, offpeak_multiplier, pricing_rule_version
		FROM account_billing_profiles
		WHERE account_uuid = $1`
	var profile model.BillingProfile
	err := p.db.QueryRowContext(ctx, query, accountUUID).Scan(
		&profile.AccountUUID,
		&profile.PackageName,
		&profile.IncludedQuotaBytes,
		&profile.BasePricePerByte,
		&profile.RegionMultiplier,
		&profile.LineMultiplier,
		&profile.PeakMultiplier,
		&profile.OffPeakMultiplier,
		&profile.PricingRuleVersion,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &profile, nil
}

func (p *Postgres) GetSourceSyncState(ctx context.Context, sourceID string) (*model.SourceSyncState, error) {
	const query = `
		SELECT source_id, last_completed_until, last_attempted_at, last_succeeded_at, last_error
		FROM billing_source_sync_state
		WHERE source_id = $1`

	var state model.SourceSyncState
	var lastCompleted sql.NullTime
	var lastAttempted sql.NullTime
	var lastSucceeded sql.NullTime

	err := p.db.QueryRowContext(ctx, query, sourceID).Scan(
		&state.SourceID,
		&lastCompleted,
		&lastAttempted,
		&lastSucceeded,
		&state.LastError,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if lastCompleted.Valid {
		value := lastCompleted.Time
		state.LastCompletedUntil = &value
	}
	if lastAttempted.Valid {
		value := lastAttempted.Time
		state.LastAttemptedAt = &value
	}
	if lastSucceeded.Valid {
		value := lastSucceeded.Time
		state.LastSucceededAt = &value
	}
	return &state, nil
}

func (p *Postgres) UpsertSourceSyncState(ctx context.Context, state model.SourceSyncState) error {
	const query = `
		INSERT INTO billing_source_sync_state (
			source_id, last_completed_until, last_attempted_at, last_succeeded_at, last_error
		) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (source_id) DO UPDATE SET
			last_completed_until = EXCLUDED.last_completed_until,
			last_attempted_at = EXCLUDED.last_attempted_at,
			last_succeeded_at = EXCLUDED.last_succeeded_at,
			last_error = EXCLUDED.last_error,
			updated_at = now()
	`

	var lastCompleted any
	if state.LastCompletedUntil != nil {
		lastCompleted = state.LastCompletedUntil.UTC()
	}
	var lastAttempted any
	if state.LastAttemptedAt != nil {
		lastAttempted = state.LastAttemptedAt.UTC()
	}
	var lastSucceeded any
	if state.LastSucceededAt != nil {
		lastSucceeded = state.LastSucceededAt.UTC()
	}

	_, err := p.db.ExecContext(ctx, query,
		state.SourceID,
		lastCompleted,
		lastAttempted,
		lastSucceeded,
		state.LastError,
	)
	return err
}

func (p *Postgres) UpsertCloudVendorCost(ctx context.Context, cost model.CloudVendorCost) error {
	const query = `
		INSERT INTO cloud_vendor_costs (
			id, provider, account_id, service_name, region, usage_start_time, usage_end_time, cost_amount, currency, usage_quantity, usage_unit, created_at
		) VALUES (COALESCE($1, gen_random_uuid()), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
		ON CONFLICT (provider, account_id, service_name, region, usage_start_time) DO UPDATE SET
			usage_end_time = EXCLUDED.usage_end_time,
			cost_amount = EXCLUDED.cost_amount,
			currency = EXCLUDED.currency,
			usage_quantity = EXCLUDED.usage_quantity,
			usage_unit = EXCLUDED.usage_unit
	`

	// Handle nil pointer for UUID
	var id interface{}
	if cost.ID.String() != "00000000-0000-0000-0000-000000000000" {
		id = cost.ID
	}

	_, err := p.db.ExecContext(ctx, query,
		id,
		cost.Provider,
		cost.AccountID,
		cost.ServiceName,
		cost.Region,
		cost.UsageStartTime.UTC(),
		cost.UsageEndTime.UTC(),
		cost.CostAmount,
		cost.Currency,
		cost.UsageQuantity,
		cost.UsageUnit,
	)
	return err
}

var _ Repository = (*Postgres)(nil)

func ensureUTC(ts time.Time) time.Time {
	return ts.UTC()
}

func unexpectedStatus(name string) error {
	return fmt.Errorf("unexpected status for %s", name)
}
