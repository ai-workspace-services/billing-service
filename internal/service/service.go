package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"billing-service/internal/config"
	"billing-service/internal/model"
	"billing-service/internal/repository"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	sourceWindowOverlap  = 2 * time.Minute
	sourceWindowPageSize = 120
)

type windowSource interface {
	FetchWindow(ctx context.Context, source config.ExporterSource, since, until time.Time, limit int, cursor *time.Time) (model.SnapshotWindowPage, error)
}

type Service struct {
	cfg                    config.Config
	source                 windowSource
	repo                   repository.Repository
	poolResolver           *NodePoolResolver
	log                    *slog.Logger
	shadowHighSpeedBytes   *atomic.Int64
	shadowHighSpeedCounter metric.Int64Counter

	mu         sync.Mutex
	lastResult model.JobResult
	lastOK     bool
	lastError  string
}

func New(cfg config.Config, source windowSource, repo repository.Repository) *Service {
	// Push mode does not run the exporter collection loop. Its absence is not
	// a failed collection; database readiness is checked separately by /readyz.
	pushOnly := cfg.IngestMode == "push" && !cfg.PullEnabled && len(cfg.ExporterSources) == 0
	var reader repository.NodePoolMetadataReader
	if r, ok := repo.(repository.NodePoolMetadataReader); ok {
		reader = r
	}
	logger := slog.Default()
	shadowCounter, _ := otel.Meter("billing-service").Int64Counter(
		"shadow_high_speed_bytes",
		metric.WithDescription("High-speed pool bytes observed in shadow mode when DUAL_BUCKET_QUOTA_ENFORCE=false"),
	)
	return &Service{
		cfg:                    cfg,
		source:                 source,
		repo:                   repo,
		poolResolver:           NewNodePoolResolver(reader, defaultPoolCacheTTL, logger),
		log:                    logger,
		shadowHighSpeedBytes:   &atomic.Int64{},
		shadowHighSpeedCounter: shadowCounter,
		lastOK:                 pushOnly,
	}
}

// ShadowHighSpeedBytes returns the total Pro-EC2-Pool bytes recorded in shadow mode.
func (s *Service) ShadowHighSpeedBytes() int64 {
	if s.shadowHighSpeedBytes == nil {
		return 0
	}
	return s.shadowHighSpeedBytes.Load()
}

// UnknownNodePoolCount returns how many samples fell back to Media-Pool due to unknown node pool metadata.
func (s *Service) UnknownNodePoolCount() int64 {
	if s.poolResolver == nil {
		return 0
	}
	return s.poolResolver.UnknownPoolCount()
}

func (s *Service) Start(ctx context.Context) {
	if !s.cfg.DatabaseRuntime.MayRunBackgroundWriters() || !s.pullEnabled() {
		return
	}
	go func() {
		_, _ = s.RunCollectAndRate(ctx, "collect-and-rate")
		ticker := time.NewTicker(s.cfg.CollectInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.RunCollectAndRate(ctx, "collect-and-rate")
			}
		}
	}()
}

func (s *Service) RunCollectAndRate(ctx context.Context, job string) (model.JobResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	startedAt := time.Now().UTC()
	result := model.JobResult{
		Job:       job,
		StartedAt: startedAt,
		Status:    "ok",
	}
	if !s.pullEnabled() {
		result.Status = "disabled"
		result.Error = "direct exporter pull is disabled; ingest snapshots through Vector"
		result.FinishedAt = time.Now().UTC()
		s.record(result)
		return result, errors.New(result.Error)
	}

	enabledSources := 0
	fatalSourceFailures := 0
	for _, source := range s.cfg.ExporterSources {
		if !source.Enabled {
			continue
		}
		enabledSources++

		status, err := s.collectSource(ctx, source, &result)
		result.SourceStatuses = append(result.SourceStatuses, status)
		if err != nil {
			fatalSourceFailures++
			result.Error = joinError(result.Error, err.Error())
		}
	}

	if enabledSources == 0 {
		result.Status = "error"
		result.Error = joinError(result.Error, "no enabled exporter sources configured")
	}
	if fatalSourceFailures > 0 {
		if result.ProcessedSamples == 0 && result.WrittenMinutes == 0 && result.ReplayedMinutes == 0 && result.Status != "partial" {
			result.Status = "error"
		} else if result.Status == "ok" {
			result.Status = "partial"
		}
	}

	result.FinishedAt = time.Now().UTC()
	s.record(result)
	if result.Status == "error" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

// pullEnabled keeps programmatic callers that construct Config directly
// compatible with the pre-ingest tests and integrations. Loaded production
// config uses BILLING_INGEST_MODE explicitly; push mode has no sources.
func (s *Service) pullEnabled() bool {
	return s.cfg.PullEnabled || len(s.cfg.ExporterSources) > 0
}

// IngestSnapshot rates one exporter snapshot delivered by Vector. The
// existing checkpoint and deterministic ledger keys make retries idempotent,
// so Vector can safely retry transient HTTP failures without double charging.
func (s *Service) IngestSnapshot(ctx context.Context, snapshot model.Snapshot) (model.JobResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	startedAt := time.Now().UTC()
	result := model.JobResult{Job: "ingest-snapshot", StartedAt: startedAt, Status: "ok"}
	switch {
	case snapshot.CollectedAt.IsZero():
		result.Status = "error"
		result.Error = "snapshot collected_at is required"
	case strings.TrimSpace(snapshot.NodeID) == "":
		result.Status = "error"
		result.Error = "snapshot node_id is required"
	case strings.TrimSpace(snapshot.Env) == "":
		result.Status = "error"
		result.Error = "snapshot env is required"
	default:
		if _, err := s.processSnapshot(ctx, snapshot, &result); err != nil {
			result.Status = "error"
			result.Error = err.Error()
		}
	}
	result.FinishedAt = time.Now().UTC()
	s.record(result)
	if result.Status == "error" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

func (s *Service) Status() model.JobResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastResult
}

func (s *Service) Health() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOK, s.lastError
}

func (s *Service) Ping() model.PingInfo {
	return model.PingInfo{
		Image:   s.cfg.ImageRef,
		Tag:     s.cfg.ImageTag,
		Commit:  s.cfg.ImageCommit,
		Version: s.cfg.ImageVersion,
	}
}

func (s *Service) collectSource(ctx context.Context, source config.ExporterSource, result *model.JobResult) (model.SourceStatus, error) {
	state, err := s.repo.GetSourceSyncState(ctx, source.SourceID)
	if err != nil {
		return model.SourceStatus{SourceID: source.SourceID, LastError: err.Error()}, fmt.Errorf("load source sync state %s: %w", source.SourceID, err)
	}
	if state == nil {
		state = &model.SourceSyncState{SourceID: source.SourceID}
	}

	attemptedAt := time.Now().UTC()
	state.LastAttemptedAt = &attemptedAt
	state.LastError = ""
	if err := s.repo.UpsertSourceSyncState(ctx, *state); err != nil {
		return sourceStatusFromState(*state), fmt.Errorf("record source attempt %s: %w", source.SourceID, err)
	}

	until := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	since := until.Add(-sourceWindowOverlap)
	if state.LastCompletedUntil != nil {
		since = state.LastCompletedUntil.UTC().Add(-sourceWindowOverlap)
	}
	if since.After(until) {
		completedUntil := until
		state.LastCompletedUntil = &completedUntil
		succeededAt := time.Now().UTC()
		state.LastSucceededAt = &succeededAt
		state.LastError = ""
		if err := s.repo.UpsertSourceSyncState(ctx, *state); err != nil {
			return sourceStatusFromState(*state), fmt.Errorf("record source noop completion %s: %w", source.SourceID, err)
		}
		return sourceStatusFromState(*state), nil
	}

	var cursor *time.Time
	var lastProcessed *time.Time
	for {
		page, err := s.source.FetchWindow(ctx, source, since, until, sourceWindowPageSize, cursor)
		if err != nil {
			return s.recordSourceFailure(ctx, *state, fmt.Errorf("fetch window for %s: %w", source.SourceID, err))
		}

		for _, snapshot := range page.Snapshots {
			if err := validateSnapshotSource(snapshot, source); err != nil {
				return s.recordSourceFailure(ctx, *state, err)
			}

			processed, err := s.processSnapshot(ctx, snapshot, result)
			if err != nil {
				return s.recordSourceFailure(ctx, *state, err)
			}
			if processed {
				collectedAt := snapshot.CollectedAt.UTC()
				lastProcessed = &collectedAt
			}
		}

		if !page.HasMore {
			break
		}
		if strings.TrimSpace(page.NextCursor) == "" {
			return s.recordSourceFailure(ctx, *state, fmt.Errorf("fetch window for %s: next_cursor missing while has_more=true", source.SourceID))
		}
		nextCursor, err := time.Parse(time.RFC3339, strings.TrimSpace(page.NextCursor))
		if err != nil {
			return s.recordSourceFailure(ctx, *state, fmt.Errorf("parse next cursor for %s: %w", source.SourceID, err))
		}
		cursor = &nextCursor
	}

	completedUntil := until
	if lastProcessed != nil && lastProcessed.Before(completedUntil) {
		completedUntil = lastProcessed.UTC()
	}
	succeededAt := time.Now().UTC()
	state.LastCompletedUntil = &completedUntil
	state.LastSucceededAt = &succeededAt
	state.LastError = ""
	if err := s.repo.UpsertSourceSyncState(ctx, *state); err != nil {
		return sourceStatusFromState(*state), fmt.Errorf("record source completion %s: %w", source.SourceID, err)
	}
	return sourceStatusFromState(*state), nil
}

func (s *Service) processSnapshot(ctx context.Context, snapshot model.Snapshot, result *model.JobResult) (bool, error) {
	processedAny := false
	for _, sample := range aggregateSamplesByUUID(snapshot.Samples, result) {
		processed, err := s.processSample(ctx, snapshot, sample, result)
		if err != nil {
			return processedAny, fmt.Errorf("process snapshot %s for %s: %w", snapshot.CollectedAt.UTC().Format(time.RFC3339), sample.UUID, err)
		}
		if processed {
			processedAny = true
			result.ProcessedSamples++
		}
	}
	return processedAny, nil
}

// aggregateSamplesByUUID sums per-inbound samples into one sample per
// account. A single xray instance can serve one user through several
// inbounds (and, in the future, several xray instances can each report the
// same user), so the exporter emits one Sample per (uuid, inbound_tag) pair.
// Billing meters and bills per account, not per inbound: processSample keys
// its checkpoint on (node_id, account_uuid) with no inbound dimension, so
// feeding it multiple samples for the same account would make them
// overwrite each other's checkpoint, alternating between a false
// counter-reset (undercounting) and overcharging on the next cycle. See
// docs/roadmap/feature-xray-usage-billing-portal-uat/04-minimal-scope.md
// ("必做 #1") for the incident this fixes.
//
// The aggregated sample's InboundTag is intentionally left blank: line-level
// attribution is out of scope until per-line quotas are introduced, at which
// point the checkpoint key must gain a line dimension instead of removing
// this aggregation step.
func aggregateSamplesByUUID(samples []model.Sample, result *model.JobResult) []model.Sample {
	aggregated := make(map[string]model.Sample, len(samples))
	order := make([]string, 0, len(samples))
	for _, sample := range samples {
		if err := validateSample(sample); err != nil {
			result.Status = "partial"
			result.Error = joinError(result.Error, err.Error())
			continue
		}
		uuid := strings.TrimSpace(sample.UUID)
		cur, seen := aggregated[uuid]
		if !seen {
			cur = model.Sample{UUID: uuid}
			order = append(order, uuid)
		}
		cur.UplinkBytesTotal += sample.UplinkBytesTotal
		cur.DownlinkBytesTotal += sample.DownlinkBytesTotal
		if cur.Email == "" {
			cur.Email = strings.TrimSpace(sample.Email)
		}
		aggregated[uuid] = cur
	}

	out := make([]model.Sample, 0, len(order))
	for _, uuid := range order {
		out = append(out, aggregated[uuid])
	}
	return out
}

func (s *Service) withRepo(repo repository.Repository) *Service {
	return &Service{
		cfg:                    s.cfg,
		source:                 s.source,
		repo:                   repo,
		poolResolver:           s.poolResolver,
		log:                    s.log,
		shadowHighSpeedBytes:   s.shadowHighSpeedBytes,
		shadowHighSpeedCounter: s.shadowHighSpeedCounter,
	}
}

func (s *Service) processSample(ctx context.Context, snapshot model.Snapshot, sample model.Sample, result *model.JobResult) (bool, error) {
	if transactional, ok := s.repo.(interface {
		WithAccountTransaction(context.Context, string, func(repository.Repository) error) error
	}); ok {
		var processed bool
		err := transactional.WithAccountTransaction(ctx, sample.UUID, func(repo repository.Repository) error {
			scoped := s.withRepo(repo)
			var err error
			processed, err = scoped.processSampleLocked(ctx, snapshot, sample, result)
			return err
		})
		return processed, err
	}
	return s.processSampleLocked(ctx, snapshot, sample, result)
}

func (s *Service) processSampleLocked(ctx context.Context, snapshot model.Snapshot, sample model.Sample, result *model.JobResult) (bool, error) {
	storageNodeID := composeStorageNodeID(snapshot.Env, snapshot.NodeID)
	minuteStart := snapshot.CollectedAt.UTC().Truncate(time.Minute)

	checkpoint, err := s.repo.GetCheckpoint(ctx, storageNodeID, sample.UUID)
	if err != nil {
		return false, fmt.Errorf("get checkpoint %s: %w", sample.UUID, err)
	}

	deltaUplink := sample.UplinkBytesTotal
	deltaDownlink := sample.DownlinkBytesTotal
	resetEpoch := int64(0)
	if checkpoint != nil {
		deltaUplink = sample.UplinkBytesTotal - checkpoint.LastUplinkTotal
		deltaDownlink = sample.DownlinkBytesTotal - checkpoint.LastDownlinkTotal
		resetEpoch = checkpoint.ResetEpoch
	}

	if deltaUplink < 0 || deltaDownlink < 0 {
		resetEpoch++
		if err := s.upsertSampleCheckpoint(ctx, storageNodeID, sample, snapshot.CollectedAt, resetEpoch); err != nil {
			return false, fmt.Errorf("upsert reset checkpoint %s: %w", sample.UUID, err)
		}
		return false, nil
	}

	totalBytes := deltaUplink + deltaDownlink
	profile, err := s.repo.GetBillingProfile(ctx, sample.UUID)
	if err != nil {
		return false, fmt.Errorf("get billing profile %s: %w", sample.UUID, err)
	}
	effectivePricing := resolvePricing(profile, s.cfg)
	routingPool := s.resolveRoutingPool(ctx, storageNodeID, snapshot.RoutingPool)
	ratingStatus := "rated"
	if routingPool == PoolCustomDedicated {
		ratingStatus = "exempt"
	}

	bucket := model.MinuteBucket{
		BucketStart:    minuteStart,
		NodeID:         storageNodeID,
		AccountUUID:    sample.UUID,
		Region:         s.cfg.DefaultRegion,
		LineCode:       strings.TrimSpace(sample.InboundTag),
		RoutingPool:    routingPool,
		UplinkBytes:    deltaUplink,
		DownlinkBytes:  deltaDownlink,
		TotalBytes:     totalBytes,
		Multiplier:     effectivePricing.multiplier,
		RatingStatus:   ratingStatus,
		SourceRevision: effectivePricing.pricingRuleVersion,
	}

	minuteExisted, err := s.repo.UpsertMinuteBucket(ctx, bucket)
	if err != nil {
		return false, fmt.Errorf("upsert minute bucket %s: %w", sample.UUID, err)
	}
	if minuteExisted {
		result.ReplayedMinutes++
	} else {
		result.WrittenMinutes++
	}

	if routingPool == PoolCustomDedicated {
		if err := s.upsertSampleCheckpoint(ctx, storageNodeID, sample, snapshot.CollectedAt, resetEpoch); err != nil {
			return false, fmt.Errorf("upsert checkpoint %s: %w", sample.UUID, err)
		}
		return true, nil
	}

	if err := s.rateAndPersistQuota(ctx, snapshot, sample, bucket, routingPool, totalBytes, effectivePricing, result); err != nil {
		return false, err
	}

	if err := s.upsertSampleCheckpoint(ctx, storageNodeID, sample, snapshot.CollectedAt, resetEpoch); err != nil {
		return false, fmt.Errorf("upsert checkpoint %s: %w", sample.UUID, err)
	}

	return true, nil
}

func (s *Service) resolveRoutingPool(ctx context.Context, storageNodeID, snapshotRoutingPool string) string {
	if s.poolResolver != nil {
		return s.poolResolver.Resolve(ctx, storageNodeID, snapshotRoutingPool)
	}
	var reader repository.NodePoolMetadataReader
	if r, ok := s.repo.(repository.NodePoolMetadataReader); ok {
		reader = r
	}
	return NewNodePoolResolver(reader, defaultPoolCacheTTL, s.log).Resolve(ctx, storageNodeID, snapshotRoutingPool)
}

func (s *Service) rateAndPersistQuota(
	ctx context.Context,
	snapshot model.Snapshot,
	sample model.Sample,
	bucket model.MinuteBucket,
	routingPool string,
	totalBytes int64,
	pricing effectivePricing,
	result *model.JobResult,
) error {
	minuteStart := bucket.BucketStart
	quota, err := s.repo.GetQuotaState(ctx, sample.UUID)
	if err != nil {
		return fmt.Errorf("get quota state %s: %w", sample.UUID, err)
	}
	if quota == nil {
		initialProxyState := "active"
		if pricing.isPaused {
			initialProxyState = "paused"
		}
		quota = &model.QuotaState{
			AccountUUID:             sample.UUID,
			RemainingIncludedQuota:  pricing.includedQuotaBytes,
			RemainingHighSpeedQuota: pricing.highSpeedQuotaBytes,
			CurrentBalance:          s.cfg.InitialBalance,
			ThrottleState:           "normal",
			SuspendState:            "active",
			ProxyAccessState:        initialProxyState,
			EffectiveAt:             snapshot.CollectedAt.UTC(),
		}
	} else if strings.TrimSpace(quota.ProxyAccessState) == "" {
		quota.ProxyAccessState = "active"
	}

	// Late buckets preceding a mid-month reset remain in raw history but
	// cannot consume the new allowance or create a retrospective charge.
	ratingBytes := totalBytes
	if quota.PeriodStart != nil && minuteStart.Before(quota.PeriodStart.UTC()) {
		ratingBytes = 0
	}

	useHighSpeedBucket := routingPool == PoolProEC2 && s.cfg.DualBucketQuotaEnforce && !pricing.isOneTimeTrial
	bucketAvailable := quota.RemainingIncludedQuota
	if useHighSpeedBucket {
		bucketAvailable = quota.RemainingHighSpeedQuota
	}
	if bucketAvailable < 0 {
		bucketAvailable = 0
	}

	includedApplied := minInt64(bucketAvailable, ratingBytes)
	chargeableBytes := ratingBytes - includedApplied
	// PAYG is retired: overage traffic never debits wallet balance into negative
	// arrears; exhausted accounts transition to paused traffic mode until
	// reactivated by subscription purchase.
	amountDelta := 0.0
	entry := model.LedgerEntry{
		ID:                 deterministicLedgerID(bucket),
		AccountUUID:        sample.UUID,
		BucketStart:        minuteStart,
		BucketEnd:          minuteStart.Add(time.Minute),
		EntryType:          "traffic_charge",
		RatedBytes:         chargeableBytes,
		AmountDelta:        amountDelta,
		BalanceAfter:       quota.CurrentBalance,
		PricingRuleVersion: pricing.pricingRuleVersion,
	}

	ledgerExisted, err := s.repo.UpsertLedger(ctx, entry)
	if err != nil {
		return fmt.Errorf("upsert ledger %s: %w", sample.UUID, err)
	}
	if ledgerExisted {
		result.ReplayedMinutes++
		return nil
	}

	if useHighSpeedBucket {
		quota.RemainingHighSpeedQuota = clampNonNegative(quota.RemainingHighSpeedQuota - includedApplied)
	} else {
		quota.RemainingIncludedQuota = clampNonNegative(quota.RemainingIncludedQuota - includedApplied)
	}

	if routingPool == PoolProEC2 && !s.cfg.DualBucketQuotaEnforce && ratingBytes > 0 {
		s.recordShadowHighSpeedUsage(ctx, sample.UUID, bucket.NodeID, ratingBytes, includedApplied, quota)
	}

	applyBalanceAndArrears(quota, entry.BalanceAfter, minuteStart, snapshot.CollectedAt)
	dualBucketPro := s.cfg.DualBucketQuotaEnforce && !pricing.isOneTimeTrial && pricing.includedQuotaBytes > 0 && pricing.highSpeedQuotaBytes > 0
	switch {
	case pricing.isPaused:
		quota.ProxyAccessState = "paused"
	case dualBucketPro:
		if quota.RemainingIncludedQuota <= 0 && quota.RemainingHighSpeedQuota <= 0 {
			quota.ProxyAccessState = "paused"
		}
	case pricing.includedQuotaBytes > 0:
		if quota.RemainingIncludedQuota <= 0 {
			quota.ProxyAccessState = "paused"
		}
	}
	if err := s.repo.UpsertQuotaState(ctx, *quota); err != nil {
		return fmt.Errorf("upsert quota state %s: %w", sample.UUID, err)
	}
	return nil
}

func (s *Service) recordShadowHighSpeedUsage(ctx context.Context, accountUUID, storageNodeID string, ratingBytes, legacyIncludedApplied int64, quota *model.QuotaState) {
	if s.shadowHighSpeedBytes != nil {
		s.shadowHighSpeedBytes.Add(ratingBytes)
	}
	if s.shadowHighSpeedCounter != nil {
		s.shadowHighSpeedCounter.Add(ctx, ratingBytes, metric.WithAttributes(attribute.String("routing_pool", PoolProEC2)))
	}
	shadowAvailable := clampNonNegative(quota.RemainingHighSpeedQuota)
	shadowApplied := minInt64(shadowAvailable, ratingBytes)
	shadowRemaining := shadowAvailable - shadowApplied
	shadowOverage := ratingBytes - shadowApplied
	logger := s.log
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "shadow high-speed traffic metered",
		"account_uuid", accountUUID,
		"node_id", storageNodeID,
		"routing_pool", PoolProEC2,
		"shadow_high_speed_bytes", ratingBytes,
		"shadow_high_speed_applied", shadowApplied,
		"shadow_high_speed_overage_bytes", shadowOverage,
		"shadow_remaining_high_speed_quota", shadowRemaining,
		"legacy_included_applied", legacyIncludedApplied,
		"remaining_included_quota", quota.RemainingIncludedQuota,
	)
}

func applyBalanceAndArrears(quota *model.QuotaState, balanceAfter float64, minuteStart, collectedAt time.Time) {
	quota.CurrentBalance = balanceAfter
	quota.Arrears = quota.CurrentBalance < 0
	if quota.Arrears {
		quota.ThrottleState = "throttled"
		if quota.ArrearsSince == nil {
			since := collectedAt.UTC()
			quota.ArrearsSince = &since
		}
	} else {
		quota.ThrottleState = "normal"
		quota.ArrearsSince = nil
	}
	quota.EffectiveAt = collectedAt.UTC()
	lastRated := minuteStart
	quota.LastRatedBucketAt = &lastRated
}

func (s *Service) upsertSampleCheckpoint(ctx context.Context, storageNodeID string, sample model.Sample, collectedAt time.Time, resetEpoch int64) error {
	return s.repo.UpsertCheckpoint(ctx, model.Checkpoint{
		NodeID:            storageNodeID,
		AccountUUID:       sample.UUID,
		LastUplinkTotal:   sample.UplinkBytesTotal,
		LastDownlinkTotal: sample.DownlinkBytesTotal,
		LastSeenAt:        collectedAt.UTC(),
		XrayRevision:      s.cfg.SourceRevision,
		ResetEpoch:        resetEpoch,
	})
}

func clampNonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

type effectivePricing struct {
	packageName         string
	isOneTimeTrial      bool
	isPaused            bool
	includedQuotaBytes  int64
	highSpeedQuotaBytes int64
	basePricePerByte    float64
	multiplier          float64
	pricingRuleVersion  string
}

func resolvePricing(profile *model.BillingProfile, cfg config.Config) effectivePricing {
	pricing := effectivePricing{
		includedQuotaBytes: cfg.InitialIncludedQuotaBytes,
		basePricePerByte:   cfg.PricePerByte,
		multiplier:         1.0,
		pricingRuleVersion: cfg.SourceRevision,
	}
	if profile == nil {
		return pricing
	}
	pricing.packageName = strings.ToLower(strings.TrimSpace(profile.PackageName))
	ruleUpper := strings.ToUpper(strings.TrimSpace(profile.PricingRuleVersion))
	pricing.isOneTimeTrial = pricing.packageName == "trial" || ruleUpper == "PLAN:TRIAL-5GB-ONCE"
	pricing.isPaused = pricing.packageName == "paused" || ruleUpper == "PLAN:PAUSED"
	if profile.IncludedQuotaBytes > 0 || pricing.isPaused {
		pricing.includedQuotaBytes = max(int64(0), profile.IncludedQuotaBytes)
	}
	if profile.HighSpeedQuotaBytes > 0 {
		pricing.highSpeedQuotaBytes = profile.HighSpeedQuotaBytes
	}
	if profile.BasePricePerByte > 0 {
		pricing.basePricePerByte = profile.BasePricePerByte
	}
	regionMultiplier := profile.RegionMultiplier
	if regionMultiplier <= 0 {
		regionMultiplier = 1.0
	}
	lineMultiplier := profile.LineMultiplier
	if lineMultiplier <= 0 {
		lineMultiplier = 1.0
	}
	pricing.multiplier = regionMultiplier * lineMultiplier
	if pricing.multiplier <= 0 {
		pricing.multiplier = 1.0
	}
	if strings.TrimSpace(profile.PricingRuleVersion) != "" {
		pricing.pricingRuleVersion = strings.TrimSpace(profile.PricingRuleVersion)
	}
	return pricing
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func validateSample(sample model.Sample) error {
	if strings.TrimSpace(sample.UUID) == "" {
		return fmt.Errorf("sample uuid is required")
	}
	if _, err := uuid.Parse(strings.TrimSpace(sample.UUID)); err != nil {
		return fmt.Errorf("sample uuid %q is not a valid UUID", sample.UUID)
	}
	return nil
}

func validateSnapshotSource(snapshot model.Snapshot, source config.ExporterSource) error {
	if source.ExpectedNodeID != "" && strings.TrimSpace(snapshot.NodeID) != source.ExpectedNodeID {
		return fmt.Errorf("source %s expected node_id %q, got %q", source.SourceID, source.ExpectedNodeID, strings.TrimSpace(snapshot.NodeID))
	}
	if source.ExpectedEnv != "" && strings.TrimSpace(snapshot.Env) != source.ExpectedEnv {
		return fmt.Errorf("source %s expected env %q, got %q", source.SourceID, source.ExpectedEnv, strings.TrimSpace(snapshot.Env))
	}
	return nil
}

func deterministicLedgerID(bucket model.MinuteBucket) string {
	key := fmt.Sprintf("%s|%s|%s|%s|%s", bucket.BucketStart.UTC().Format(time.RFC3339), bucket.NodeID, bucket.AccountUUID, bucket.Region, bucket.LineCode)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

func composeStorageNodeID(env, nodeID string) string {
	env = strings.TrimSpace(env)
	nodeID = strings.TrimSpace(nodeID)
	if env == "" {
		return nodeID
	}
	return env + ":" + nodeID
}

func joinError(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func sourceStatusFromState(state model.SourceSyncState) model.SourceStatus {
	return model.SourceStatus{
		SourceID:           state.SourceID,
		LastCompletedUntil: copyTimePtr(state.LastCompletedUntil),
		LastAttemptedAt:    copyTimePtr(state.LastAttemptedAt),
		LastSucceededAt:    copyTimePtr(state.LastSucceededAt),
		LastError:          state.LastError,
	}
}

func copyTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func (s *Service) recordSourceFailure(ctx context.Context, state model.SourceSyncState, err error) (model.SourceStatus, error) {
	message := err.Error()
	state.LastError = message
	if persistErr := s.repo.UpsertSourceSyncState(ctx, state); persistErr != nil {
		message = joinError(message, fmt.Sprintf("persist source error state: %v", persistErr))
		state.LastError = message
	}
	return sourceStatusFromState(state), err
}

func (s *Service) record(result model.JobResult) {
	s.lastResult = result
	s.lastError = result.Error
	s.lastOK = result.Status != "error"
}
