package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"billing-service/internal/config"
	"billing-service/internal/model"
	"billing-service/internal/observability"
	"billing-service/internal/repository"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func serviceTracer() trace.Tracer {
	return otel.Tracer("billing-service")
}

const (
	sourceWindowOverlap  = 2 * time.Minute
	sourceWindowPageSize = 120
)

type windowSource interface {
	FetchWindow(ctx context.Context, source config.ExporterSource, since, until time.Time, limit int, cursor *time.Time) (model.SnapshotWindowPage, error)
}

type Service struct {
	cfg    config.Config
	source windowSource
	repo   repository.Repository

	mu         sync.Mutex
	lastResult model.JobResult
	lastOK     bool
	lastError  string
}

func New(cfg config.Config, source windowSource, repo repository.Repository) *Service {
	return &Service{
		cfg:    cfg,
		source: source,
		repo:   repo,
	}
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

	transport := observability.TransportFromNodeID(snapshot.NodeID)
	snapshotRef := strings.TrimSpace(snapshot.SnapshotRef)
	if snapshotRef == "" {
		snapshotRef = observability.DeterministicSnapshotRef(snapshot.Env, snapshot.NodeID, snapshot.CollectedAt)
	}
	attrs := []attribute.KeyValue{
		attribute.String("stage", "validate"),
		attribute.String("transport", transport),
		attribute.String("env", strings.TrimSpace(snapshot.Env)),
		attribute.String("snapshot_ref", snapshotRef),
		attribute.Int("sample_count", len(snapshot.Samples)),
	}
	if batchRef := strings.TrimSpace(snapshot.BatchRef); batchRef != "" {
		attrs = append(attrs, attribute.String("batch_ref", batchRef))
	}
	opts := []trace.SpanStartOption{trace.WithAttributes(attrs...)}
	if producerSpanCtx, ok := observability.ExtractTraceParent(snapshot.ProducerTraceParent, snapshot.TraceParent); ok {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: producerSpanCtx}))
	}
	validateCtx, validateSpan := serviceTracer().Start(ctx, "billing.ingest.validate", opts...)
	defer validateSpan.End()

	switch {
	case snapshot.CollectedAt.IsZero():
		result.Status = "error"
		result.Error = "snapshot collected_at is required"
		validateSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "missing_collected_at"),
		)
		validateSpan.SetStatus(codes.Error, result.Error)
	case strings.TrimSpace(snapshot.NodeID) == "":
		result.Status = "error"
		result.Error = "snapshot node_id is required"
		validateSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "missing_node_id"),
		)
		validateSpan.SetStatus(codes.Error, result.Error)
	case strings.TrimSpace(snapshot.Env) == "":
		result.Status = "error"
		result.Error = "snapshot env is required"
		validateSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "missing_env"),
		)
		validateSpan.SetStatus(codes.Error, result.Error)
	default:
		if len(snapshot.Samples) == 0 {
			validateSpan.SetAttributes(attribute.String("outcome", "empty_samples"))
		} else {
			validateSpan.SetAttributes(attribute.String("outcome", "ok"))
		}
		if _, err := s.processSnapshot(validateCtx, snapshot, &result); err != nil {
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
	transport := observability.TransportFromNodeID(snapshot.NodeID)
	_, dedupSpan := serviceTracer().Start(ctx, "billing.ingest.deduplicate",
		trace.WithAttributes(
			attribute.String("stage", "deduplicate"),
			attribute.String("transport", transport),
			attribute.Int("raw_samples", len(snapshot.Samples)),
		),
	)
	aggregated, hadInvalid := aggregateSamplesByUUIDWithStats(snapshot.Samples, result)
	dedupOutcome := "ok"
	if hadInvalid {
		dedupOutcome = "partial_invalid"
	}
	dedupSpan.SetAttributes(
		attribute.Int("aggregated_accounts", len(aggregated)),
		attribute.String("outcome", dedupOutcome),
	)
	dedupSpan.End()

	processedAny := false
	for _, sample := range aggregated {
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
	out, _ := aggregateSamplesByUUIDWithStats(samples, result)
	return out
}

func aggregateSamplesByUUIDWithStats(samples []model.Sample, result *model.JobResult) ([]model.Sample, bool) {
	aggregated := make(map[string]model.Sample, len(samples))
	order := make([]string, 0, len(samples))
	hadInvalid := false
	for _, sample := range samples {
		if err := validateSample(sample); err != nil {
			hadInvalid = true
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
	return out, hadInvalid
}

func (s *Service) processSample(ctx context.Context, snapshot model.Snapshot, sample model.Sample, result *model.JobResult) (bool, error) {
	storageNodeID := composeStorageNodeID(snapshot.Env, snapshot.NodeID)
	minuteStart := snapshot.CollectedAt.UTC().Truncate(time.Minute)
	transport := observability.TransportFromNodeID(snapshot.NodeID)

	deltaCtx, deltaSpan := serviceTracer().Start(ctx, "billing.ingest.delta",
		trace.WithAttributes(
			attribute.String("stage", "delta"),
			attribute.String("transport", transport),
		),
	)
	checkpoint, err := s.repo.GetCheckpoint(deltaCtx, storageNodeID, sample.UUID)
	if err != nil {
		deltaSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "checkpoint_read_error"),
		)
		deltaSpan.SetStatus(codes.Error, "get_checkpoint_failed")
		deltaSpan.End()
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
		deltaSpan.SetAttributes(
			attribute.String("outcome", "counter_reset"),
			attribute.Int64("delta_bytes", 0),
		)
		deltaSpan.End()

		resetEpoch++
		cpCtx, cpSpan := serviceTracer().Start(ctx, "billing.ingest.checkpoint.write",
			trace.WithAttributes(
				attribute.String("stage", "checkpoint_write"),
				attribute.String("transport", transport),
			),
		)
		err := s.repo.UpsertCheckpoint(cpCtx, model.Checkpoint{
			NodeID:            storageNodeID,
			AccountUUID:       sample.UUID,
			LastUplinkTotal:   sample.UplinkBytesTotal,
			LastDownlinkTotal: sample.DownlinkBytesTotal,
			LastSeenAt:        snapshot.CollectedAt.UTC(),
			XrayRevision:      s.cfg.SourceRevision,
			ResetEpoch:        resetEpoch,
		})
		if err != nil {
			cpSpan.SetAttributes(
				attribute.String("outcome", "error"),
				attribute.String("error_class", "checkpoint_write_error"),
			)
			cpSpan.SetStatus(codes.Error, "upsert_reset_checkpoint_failed")
			cpSpan.End()
			return false, fmt.Errorf("upsert reset checkpoint %s: %w", sample.UUID, err)
		}
		cpSpan.SetAttributes(attribute.String("outcome", "reset_written"))
		cpSpan.End()
		return false, nil
	}

	totalBytes := deltaUplink + deltaDownlink
	deltaOutcome := "positive_delta"
	switch {
	case checkpoint == nil:
		deltaOutcome = "baseline"
	case totalBytes == 0:
		deltaOutcome = "zero_delta"
	}
	deltaSpan.SetAttributes(
		attribute.String("outcome", deltaOutcome),
		attribute.Int64("delta_bytes", totalBytes),
	)
	deltaSpan.End()

	profile, err := s.repo.GetBillingProfile(ctx, sample.UUID)
	if err != nil {
		return false, fmt.Errorf("get billing profile %s: %w", sample.UUID, err)
	}
	effectivePricing := resolvePricing(profile, s.cfg)

	bucket := model.MinuteBucket{
		BucketStart:    minuteStart,
		NodeID:         storageNodeID,
		AccountUUID:    sample.UUID,
		Region:         s.cfg.DefaultRegion,
		LineCode:       strings.TrimSpace(sample.InboundTag),
		UplinkBytes:    deltaUplink,
		DownlinkBytes:  deltaDownlink,
		TotalBytes:     totalBytes,
		Multiplier:     effectivePricing.multiplier,
		RatingStatus:   "rated",
		SourceRevision: effectivePricing.pricingRuleVersion,
	}

	bucketCtx, bucketSpan := serviceTracer().Start(ctx, "billing.ingest.bucket.write",
		trace.WithAttributes(
			attribute.String("stage", "bucket_write"),
			attribute.String("transport", transport),
		),
	)
	minuteExisted, err := s.repo.UpsertMinuteBucket(bucketCtx, bucket)
	if err != nil {
		bucketSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "bucket_write_error"),
		)
		bucketSpan.SetStatus(codes.Error, "upsert_minute_bucket_failed")
		bucketSpan.End()
		return false, fmt.Errorf("upsert minute bucket %s: %w", sample.UUID, err)
	}
	if minuteExisted {
		result.ReplayedMinutes++
		bucketSpan.SetAttributes(attribute.String("outcome", "replayed"))
	} else {
		result.WrittenMinutes++
		bucketSpan.SetAttributes(attribute.String("outcome", "written"))
	}
	bucketSpan.End()

	entry := model.LedgerEntry{
		ID:                 deterministicLedgerID(bucket),
		AccountUUID:        sample.UUID,
		BucketStart:        minuteStart,
		BucketEnd:          minuteStart.Add(time.Minute),
		EntryType:          "traffic_charge",
		PricingRuleVersion: s.cfg.SourceRevision,
	}

	quota, err := s.repo.GetQuotaState(ctx, sample.UUID)
	if err != nil {
		return false, fmt.Errorf("get quota state %s: %w", sample.UUID, err)
	}
	if quota == nil {
		quota = &model.QuotaState{
			AccountUUID:            sample.UUID,
			RemainingIncludedQuota: effectivePricing.includedQuotaBytes,
			CurrentBalance:         s.cfg.InitialBalance,
			ThrottleState:          "normal",
			SuspendState:           "active",
			EffectiveAt:            snapshot.CollectedAt.UTC(),
		}
	}

	includedApplied := minInt64(quota.RemainingIncludedQuota, totalBytes)
	chargeableBytes := totalBytes - includedApplied
	amountDelta := -float64(chargeableBytes) * effectivePricing.basePricePerByte * effectivePricing.multiplier
	entry.RatedBytes = chargeableBytes
	entry.AmountDelta = amountDelta
	entry.PricingRuleVersion = effectivePricing.pricingRuleVersion
	entry.BalanceAfter = quota.CurrentBalance + amountDelta

	ledgerCtx, ledgerSpan := serviceTracer().Start(ctx, "billing.ingest.ledger.write",
		trace.WithAttributes(
			attribute.String("stage", "ledger_write"),
			attribute.String("transport", transport),
		),
	)
	ledgerExisted, err := s.repo.UpsertLedger(ledgerCtx, entry)
	if err != nil {
		ledgerSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "ledger_write_error"),
		)
		ledgerSpan.SetStatus(codes.Error, "upsert_ledger_failed")
		ledgerSpan.End()
		return false, fmt.Errorf("upsert ledger %s: %w", sample.UUID, err)
	}
	if ledgerExisted {
		ledgerSpan.SetAttributes(attribute.String("outcome", "replayed"))
	} else {
		ledgerSpan.SetAttributes(attribute.String("outcome", "written"))
	}
	ledgerSpan.End()

	quotaCtx, quotaSpan := serviceTracer().Start(ctx, "billing.ingest.quota.write",
		trace.WithAttributes(
			attribute.String("stage", "quota_write"),
			attribute.String("transport", transport),
		),
	)
	if !ledgerExisted {
		remainingQuota := quota.RemainingIncludedQuota - includedApplied
		if remainingQuota < 0 {
			remainingQuota = 0
		}
		quota.RemainingIncludedQuota = remainingQuota
		quota.CurrentBalance = entry.BalanceAfter
		quota.Arrears = quota.CurrentBalance < 0
		if quota.Arrears {
			quota.ThrottleState = "throttled"
			if quota.ArrearsSince == nil {
				since := snapshot.CollectedAt.UTC()
				quota.ArrearsSince = &since
			}
		} else {
			quota.ThrottleState = "normal"
			quota.ArrearsSince = nil
		}
		quota.EffectiveAt = snapshot.CollectedAt.UTC()
		lastRated := minuteStart
		quota.LastRatedBucketAt = &lastRated
		if err := s.repo.UpsertQuotaState(quotaCtx, *quota); err != nil {
			quotaSpan.SetAttributes(
				attribute.String("outcome", "error"),
				attribute.String("error_class", "quota_write_error"),
			)
			quotaSpan.SetStatus(codes.Error, "upsert_quota_state_failed")
			quotaSpan.End()
			return false, fmt.Errorf("upsert quota state %s: %w", sample.UUID, err)
		}
		quotaSpan.SetAttributes(attribute.String("outcome", "written"))
	} else {
		result.ReplayedMinutes++
		quotaSpan.SetAttributes(attribute.String("outcome", "skipped_replay"))
	}
	quotaSpan.End()

	cpCtx, cpSpan := serviceTracer().Start(ctx, "billing.ingest.checkpoint.write",
		trace.WithAttributes(
			attribute.String("stage", "checkpoint_write"),
			attribute.String("transport", transport),
		),
	)
	if err := s.repo.UpsertCheckpoint(cpCtx, model.Checkpoint{
		NodeID:            storageNodeID,
		AccountUUID:       sample.UUID,
		LastUplinkTotal:   sample.UplinkBytesTotal,
		LastDownlinkTotal: sample.DownlinkBytesTotal,
		LastSeenAt:        snapshot.CollectedAt.UTC(),
		XrayRevision:      s.cfg.SourceRevision,
		ResetEpoch:        resetEpoch,
	}); err != nil {
		cpSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "checkpoint_write_error"),
		)
		cpSpan.SetStatus(codes.Error, "upsert_checkpoint_failed")
		cpSpan.End()
		return false, fmt.Errorf("upsert checkpoint %s: %w", sample.UUID, err)
	}
	cpSpan.SetAttributes(attribute.String("outcome", "written"))
	cpSpan.End()

	return true, nil
}

type effectivePricing struct {
	includedQuotaBytes int64
	basePricePerByte   float64
	multiplier         float64
	pricingRuleVersion string
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
	if profile.IncludedQuotaBytes > 0 {
		pricing.includedQuotaBytes = profile.IncludedQuotaBytes
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
