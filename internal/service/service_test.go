package service

import (
	"context"
	"testing"
	"time"

	"billing-service/internal/config"
	"billing-service/internal/model"
	"billing-service/internal/repository"
)

type fakeWindowSource struct {
	pagesBySource map[string][]model.SnapshotWindowPage
	errBySource   map[string]error
	requests      []windowRequest
}

type windowRequest struct {
	sourceID string
	since    time.Time
	until    time.Time
	limit    int
	cursor   *time.Time
}

func (f *fakeWindowSource) FetchWindow(_ context.Context, source config.ExporterSource, since, until time.Time, limit int, cursor *time.Time) (model.SnapshotWindowPage, error) {
	f.requests = append(f.requests, windowRequest{
		sourceID: source.SourceID,
		since:    since,
		until:    until,
		limit:    limit,
		cursor:   cursor,
	})
	if err := f.errBySource[source.SourceID]; err != nil {
		return model.SnapshotWindowPage{}, err
	}
	pages := f.pagesBySource[source.SourceID]
	if len(pages) == 0 {
		return model.SnapshotWindowPage{}, nil
	}
	page := pages[0]
	f.pagesBySource[source.SourceID] = pages[1:]
	return page, nil
}

type memoryRepo struct {
	checkpoints map[string]model.Checkpoint
	buckets     map[string]model.MinuteBucket
	ledgers     map[string]model.LedgerEntry
	quotas      map[string]model.QuotaState
	profiles    map[string]model.BillingProfile
	sourceSync  map[string]model.SourceSyncState
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{
		checkpoints: map[string]model.Checkpoint{},
		buckets:     map[string]model.MinuteBucket{},
		ledgers:     map[string]model.LedgerEntry{},
		quotas:      map[string]model.QuotaState{},
		profiles:    map[string]model.BillingProfile{},
		sourceSync:  map[string]model.SourceSyncState{},
	}
}

func checkpointKey(nodeID, accountUUID string) string {
	return nodeID + "\x00" + accountUUID
}

func bucketKey(bucket model.MinuteBucket) string {
	return bucket.BucketStart.UTC().Format(time.RFC3339) + "\x00" + bucket.NodeID + "\x00" + bucket.AccountUUID + "\x00" + bucket.Region + "\x00" + bucket.LineCode
}

func (m *memoryRepo) GetCheckpoint(_ context.Context, nodeID, accountUUID string) (*model.Checkpoint, error) {
	if checkpoint, ok := m.checkpoints[checkpointKey(nodeID, accountUUID)]; ok {
		copy := checkpoint
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) UpsertCheckpoint(_ context.Context, checkpoint model.Checkpoint) error {
	m.checkpoints[checkpointKey(checkpoint.NodeID, checkpoint.AccountUUID)] = checkpoint
	return nil
}

func (m *memoryRepo) UpsertMinuteBucket(_ context.Context, bucket model.MinuteBucket) (bool, error) {
	key := bucketKey(bucket)
	_, existed := m.buckets[key]
	m.buckets[key] = bucket
	return existed, nil
}

func (m *memoryRepo) UpsertLedger(_ context.Context, entry model.LedgerEntry) (bool, error) {
	_, existed := m.ledgers[entry.ID]
	m.ledgers[entry.ID] = entry
	return existed, nil
}

func (m *memoryRepo) GetQuotaState(_ context.Context, accountUUID string) (*model.QuotaState, error) {
	if quota, ok := m.quotas[accountUUID]; ok {
		copy := quota
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) UpsertQuotaState(_ context.Context, state model.QuotaState) error {
	m.quotas[state.AccountUUID] = state
	return nil
}

func (m *memoryRepo) GetBillingProfile(_ context.Context, accountUUID string) (*model.BillingProfile, error) {
	if profile, ok := m.profiles[accountUUID]; ok {
		copy := profile
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) GetSourceSyncState(_ context.Context, sourceID string) (*model.SourceSyncState, error) {
	if state, ok := m.sourceSync[sourceID]; ok {
		copy := cloneSyncState(state)
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) UpsertSourceSyncState(_ context.Context, state model.SourceSyncState) error {
	m.sourceSync[state.SourceID] = cloneSyncState(state)
	return nil
}

func cloneSyncState(state model.SourceSyncState) model.SourceSyncState {
	copy := state
	copy.LastCompletedUntil = copyTimePtr(state.LastCompletedUntil)
	copy.LastAttemptedAt = copyTimePtr(state.LastAttemptedAt)
	copy.LastSucceededAt = copyTimePtr(state.LastSucceededAt)
	return copy
}

func (m *memoryRepo) UpsertCloudVendorCost(ctx context.Context, cost model.CloudVendorCost) error {
	// Dummy implementation for tests
	return nil
}

var _ repository.Repository = (*memoryRepo)(nil)

func baseConfig() config.Config {
	return config.Config{
		ImageRef:     "registry.example.com/billing-service:sha-0123456789abcdef0123456789abcdef01234567",
		ImageTag:     "sha-0123456789abcdef0123456789abcdef01234567",
		ImageCommit:  "0123456789abcdef0123456789abcdef01234567",
		ImageVersion: "0123456789abcdef0123456789abcdef01234567",
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
		SourceRevision:            "billing-service-v1",
		PricePerByte:              0.5,
		InitialIncludedQuotaBytes: 0,
		InitialBalance:            0,
	}
}

func TestPingReflectsImageRef(t *testing.T) {
	svc := New(baseConfig(), &fakeWindowSource{}, newMemoryRepo())
	ping := svc.Ping()
	if ping.Image != baseConfig().ImageRef || ping.Tag != baseConfig().ImageTag || ping.Commit != baseConfig().ImageCommit || ping.Version != baseConfig().ImageVersion {
		t.Fatalf("unexpected ping %#v", ping)
	}
}

func singleSnapshotPage(snapshot model.Snapshot) model.SnapshotWindowPage {
	return model.SnapshotWindowPage{
		NodeID:    snapshot.NodeID,
		Env:       snapshot.Env,
		Snapshots: []model.Snapshot{snapshot},
	}
}

func TestDeltaCalculationAndQuotaUpdate(t *testing.T) {
	repo := newMemoryRepo()
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 30, 15, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "prod",
				Samples: []model.Sample{{
					UUID:               "11111111-1111-1111-1111-111111111111",
					Email:              "user@example.com",
					InboundTag:         "premium",
					UplinkBytesTotal:   100,
					DownlinkBytesTotal: 50,
				}},
			})},
		},
	}
	svc := New(baseConfig(), source, repo)

	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if result.ProcessedSamples != 1 || result.WrittenMinutes != 1 {
		t.Fatalf("unexpected result %#v", result)
	}
	quota := repo.quotas["11111111-1111-1111-1111-111111111111"]
	if quota.CurrentBalance != -75 {
		t.Fatalf("expected current balance -75, got %v", quota.CurrentBalance)
	}
}

func TestIncludedQuotaAndMultipliersFromBillingProfile(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	repo.profiles[accountUUID] = model.BillingProfile{
		AccountUUID:        accountUUID,
		PackageName:        "starter",
		IncludedQuotaBytes: 100,
		BasePricePerByte:   0.5,
		RegionMultiplier:   1.2,
		LineMultiplier:     2.0,
		PricingRuleVersion: "pricing-v2",
	}
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 30, 15, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "prod",
				Samples: []model.Sample{{
					UUID:               accountUUID,
					InboundTag:         "premium",
					UplinkBytesTotal:   100,
					DownlinkBytesTotal: 50,
				}},
			})},
		},
	}
	svc := New(baseConfig(), source, repo)

	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if result.ProcessedSamples != 1 || result.WrittenMinutes != 1 {
		t.Fatalf("unexpected result %#v", result)
	}

	quota := repo.quotas[accountUUID]
	if quota.RemainingIncludedQuota != 0 {
		t.Fatalf("expected remaining quota 0, got %d", quota.RemainingIncludedQuota)
	}
	if quota.CurrentBalance != -60 {
		t.Fatalf("expected current balance -60, got %v", quota.CurrentBalance)
	}

	bucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 4, 8, 10, 30, 0, 0, time.UTC),
		NodeID:      composeStorageNodeID("prod", "jp-node"),
		AccountUUID: accountUUID,
		Region:      "",
		LineCode:    "premium",
	})]
	if bucket.Multiplier != 2.4 {
		t.Fatalf("expected multiplier 2.4, got %v", bucket.Multiplier)
	}
	for _, entry := range repo.ledgers {
		if entry.RatedBytes != 50 {
			t.Fatalf("expected rated bytes 50, got %d", entry.RatedBytes)
		}
		if entry.AmountDelta != -60 {
			t.Fatalf("expected amount delta -60, got %v", entry.AmountDelta)
		}
		if entry.PricingRuleVersion != "pricing-v2" {
			t.Fatalf("expected pricing version pricing-v2, got %q", entry.PricingRuleVersion)
		}
	}
}

func TestDuplicateMinuteIsReplaySafe(t *testing.T) {
	repo := newMemoryRepo()
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {
				singleSnapshotPage(model.Snapshot{
					CollectedAt: time.Date(2026, 4, 8, 10, 30, 30, 0, time.UTC),
					NodeID:      "jp-node",
					Env:         "prod",
					Samples: []model.Sample{{
						UUID:               "11111111-1111-1111-1111-111111111111",
						Email:              "user@example.com",
						InboundTag:         "premium",
						UplinkBytesTotal:   100,
						DownlinkBytesTotal: 50,
					}},
				}),
				singleSnapshotPage(model.Snapshot{
					CollectedAt: time.Date(2026, 4, 8, 10, 30, 30, 0, time.UTC),
					NodeID:      "jp-node",
					Env:         "prod",
					Samples: []model.Sample{{
						UUID:               "11111111-1111-1111-1111-111111111111",
						Email:              "user@example.com",
						InboundTag:         "premium",
						UplinkBytesTotal:   100,
						DownlinkBytesTotal: 50,
					}},
				}),
			},
		},
	}
	svc := New(baseConfig(), source, repo)

	if _, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if result.ReplayedMinutes == 0 {
		t.Fatalf("expected replayed minutes, got %#v", result)
	}
	if len(repo.ledgers) != 1 {
		t.Fatalf("expected 1 ledger entry, got %d", len(repo.ledgers))
	}
}

func TestNegativeDeltaProtection(t *testing.T) {
	repo := newMemoryRepo()
	cfg := baseConfig()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	nodeKey := composeStorageNodeID("prod", "jp-node")
	repo.checkpoints[checkpointKey(nodeKey, accountUUID)] = model.Checkpoint{
		NodeID:            nodeKey,
		AccountUUID:       accountUUID,
		LastUplinkTotal:   200,
		LastDownlinkTotal: 200,
		LastSeenAt:        time.Now().UTC(),
		XrayRevision:      "prev",
		ResetEpoch:        0,
	}
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 31, 0, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "prod",
				Samples: []model.Sample{{
					UUID:               accountUUID,
					InboundTag:         "premium",
					UplinkBytesTotal:   10,
					DownlinkBytesTotal: 20,
				}},
			})},
		},
	}
	svc := New(cfg, source, repo)

	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if result.ProcessedSamples != 0 {
		t.Fatalf("expected negative delta sample to be skipped, got %#v", result)
	}
	if len(repo.buckets) != 0 || len(repo.ledgers) != 0 {
		t.Fatalf("expected no writes on negative delta")
	}
	if repo.checkpoints[checkpointKey(nodeKey, accountUUID)].ResetEpoch != 1 {
		t.Fatalf("expected reset epoch increment")
	}
}

func TestRestartRecoveryFromCheckpoint(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	nodeKey := composeStorageNodeID("prod", "jp-node")
	repo.checkpoints[checkpointKey(nodeKey, accountUUID)] = model.Checkpoint{
		NodeID:            nodeKey,
		AccountUUID:       accountUUID,
		LastUplinkTotal:   100,
		LastDownlinkTotal: 100,
		LastSeenAt:        time.Now().UTC(),
	}
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 32, 0, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "prod",
				Samples: []model.Sample{{
					UUID:               accountUUID,
					InboundTag:         "premium",
					UplinkBytesTotal:   130,
					DownlinkBytesTotal: 140,
				}},
			})},
		},
	}
	svc := New(baseConfig(), source, repo)

	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if result.ProcessedSamples != 1 || result.WrittenMinutes != 1 {
		t.Fatalf("unexpected result %#v", result)
	}
	bucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 4, 8, 10, 32, 0, 0, time.UTC),
		NodeID:      nodeKey,
		AccountUUID: accountUUID,
		Region:      "",
		LineCode:    "premium",
	})]
	if bucket.TotalBytes != 70 {
		t.Fatalf("expected recovered delta 70, got %d", bucket.TotalBytes)
	}
}

func TestMultiEnvIsolation(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	cfg := baseConfig()
	cfg.ExporterSources = []config.ExporterSource{
		{
			SourceID:       "prod-source",
			BaseURL:        "https://prod.svc.plus",
			ExpectedNodeID: "jp-node",
			ExpectedEnv:    "prod",
			Enabled:        true,
			TimeoutSeconds: 15,
		},
		{
			SourceID:       "preview-source",
			BaseURL:        "https://preview.svc.plus",
			ExpectedNodeID: "jp-node",
			ExpectedEnv:    "preview",
			Enabled:        true,
			TimeoutSeconds: 15,
		},
	}
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"prod-source": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 33, 0, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "prod",
				Samples:     []model.Sample{{UUID: accountUUID, InboundTag: "premium", UplinkBytesTotal: 10, DownlinkBytesTotal: 10}},
			})},
			"preview-source": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 33, 0, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "preview",
				Samples:     []model.Sample{{UUID: accountUUID, InboundTag: "premium", UplinkBytesTotal: 10, DownlinkBytesTotal: 10}},
			})},
		},
	}
	svc := New(cfg, source, repo)

	if _, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(repo.buckets) != 2 {
		t.Fatalf("expected isolated buckets per env, got %d", len(repo.buckets))
	}
}

func TestExpectedNodeIDMismatchIsFatalForSource(t *testing.T) {
	repo := newMemoryRepo()
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 34, 0, 0, time.UTC),
				NodeID:      "unexpected-node",
				Env:         "prod",
				Samples:     []model.Sample{{UUID: "11111111-1111-1111-1111-111111111111", InboundTag: "premium", UplinkBytesTotal: 10, DownlinkBytesTotal: 10}},
			})},
		},
	}
	svc := New(baseConfig(), source, repo)

	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err == nil {
		t.Fatalf("expected source mismatch error")
	}
	if result.Status != "error" {
		t.Fatalf("expected error status, got %#v", result)
	}
}

func TestSourceStatusIncludesSyncState(t *testing.T) {
	repo := newMemoryRepo()
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 35, 0, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "prod",
				Samples: []model.Sample{{
					UUID:               "11111111-1111-1111-1111-111111111111",
					InboundTag:         "premium",
					UplinkBytesTotal:   10,
					DownlinkBytesTotal: 10,
				}},
			})},
		},
	}
	svc := New(baseConfig(), source, repo)

	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if len(result.SourceStatuses) != 1 {
		t.Fatalf("expected one source status, got %#v", result.SourceStatuses)
	}
	if result.SourceStatuses[0].SourceID != "default" {
		t.Fatalf("unexpected source status %#v", result.SourceStatuses[0])
	}
	if result.SourceStatuses[0].LastCompletedUntil == nil {
		t.Fatalf("expected last completed until in source status")
	}
}
