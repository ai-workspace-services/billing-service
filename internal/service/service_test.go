package service

import (
	"context"
	"errors"
	"sync"
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
	mu          sync.Mutex
	inTx        bool
	checkpoints map[string]model.Checkpoint
	buckets     map[string]model.MinuteBucket
	ledgers     map[string]model.LedgerEntry
	quotas      map[string]model.QuotaState
	profiles    map[string]model.BillingProfile
	sourceSync  map[string]model.SourceSyncState

	upsertBucketErr     error
	upsertLedgerErr     error
	upsertQuotaStateErr error
	upsertCheckpointErr error
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

func (m *memoryRepo) WithinAccountSampleTx(_ context.Context, fn func(repository.Repository) error) error {
	if m.inTx {
		return fn(m)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	staged := &memoryRepo{
		inTx:                true,
		checkpoints:         make(map[string]model.Checkpoint, len(m.checkpoints)),
		buckets:             make(map[string]model.MinuteBucket, len(m.buckets)),
		ledgers:             make(map[string]model.LedgerEntry, len(m.ledgers)),
		quotas:              make(map[string]model.QuotaState, len(m.quotas)),
		profiles:            make(map[string]model.BillingProfile, len(m.profiles)),
		sourceSync:          make(map[string]model.SourceSyncState, len(m.sourceSync)),
		upsertBucketErr:     m.upsertBucketErr,
		upsertLedgerErr:     m.upsertLedgerErr,
		upsertQuotaStateErr: m.upsertQuotaStateErr,
		upsertCheckpointErr: m.upsertCheckpointErr,
	}
	for k, v := range m.checkpoints {
		staged.checkpoints[k] = v
	}
	for k, v := range m.buckets {
		staged.buckets[k] = v
	}
	for k, v := range m.ledgers {
		staged.ledgers[k] = v
	}
	for k, v := range m.quotas {
		staged.quotas[k] = cloneQuotaState(v)
	}
	for k, v := range m.profiles {
		staged.profiles[k] = v
	}
	for k, v := range m.sourceSync {
		staged.sourceSync[k] = cloneSyncState(v)
	}

	if err := fn(staged); err != nil {
		return err
	}

	m.checkpoints = staged.checkpoints
	m.buckets = staged.buckets
	m.ledgers = staged.ledgers
	m.quotas = staged.quotas
	m.profiles = staged.profiles
	m.sourceSync = staged.sourceSync
	return nil
}

func cloneQuotaState(state model.QuotaState) model.QuotaState {
	cloned := state
	cloned.ArrearsSince = copyTimePtr(state.ArrearsSince)
	cloned.LastRatedBucketAt = copyTimePtr(state.LastRatedBucketAt)
	return cloned
}

func (m *memoryRepo) GetCheckpoint(_ context.Context, nodeID, accountUUID string) (*model.Checkpoint, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if checkpoint, ok := m.checkpoints[checkpointKey(nodeID, accountUUID)]; ok {
		copy := checkpoint
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) UpsertCheckpoint(_ context.Context, checkpoint model.Checkpoint) error {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if m.upsertCheckpointErr != nil {
		return m.upsertCheckpointErr
	}
	m.checkpoints[checkpointKey(checkpoint.NodeID, checkpoint.AccountUUID)] = checkpoint
	return nil
}

func (m *memoryRepo) GetMinuteBucket(_ context.Context, key model.MinuteBucket) (*model.MinuteBucket, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if bucket, ok := m.buckets[bucketKey(key)]; ok {
		copy := bucket
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) UpsertMinuteBucket(_ context.Context, bucket model.MinuteBucket) (bool, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if m.upsertBucketErr != nil {
		return false, m.upsertBucketErr
	}
	key := bucketKey(bucket)
	_, existed := m.buckets[key]
	m.buckets[key] = bucket
	return existed, nil
}

func (m *memoryRepo) GetLedger(_ context.Context, id string) (*model.LedgerEntry, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if entry, ok := m.ledgers[id]; ok {
		copy := entry
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) UpsertLedger(_ context.Context, entry model.LedgerEntry) (bool, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if m.upsertLedgerErr != nil {
		return false, m.upsertLedgerErr
	}
	_, existed := m.ledgers[entry.ID]
	m.ledgers[entry.ID] = entry
	return existed, nil
}

func (m *memoryRepo) GetQuotaState(_ context.Context, accountUUID string) (*model.QuotaState, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if quota, ok := m.quotas[accountUUID]; ok {
		copy := cloneQuotaState(quota)
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) UpsertQuotaState(_ context.Context, state model.QuotaState) error {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if m.upsertQuotaStateErr != nil {
		return m.upsertQuotaStateErr
	}
	m.quotas[state.AccountUUID] = cloneQuotaState(state)
	return nil
}

func (m *memoryRepo) ListArrearsAccounts(_ context.Context) ([]model.QuotaState, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	var states []model.QuotaState
	for _, quota := range m.quotas {
		if quota.Arrears && quota.SuspendState != "suspended" {
			states = append(states, cloneQuotaState(quota))
		}
	}
	return states, nil
}

func (m *memoryRepo) GetBillingProfile(_ context.Context, accountUUID string) (*model.BillingProfile, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if profile, ok := m.profiles[accountUUID]; ok {
		copy := profile
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) GetSourceSyncState(_ context.Context, sourceID string) (*model.SourceSyncState, error) {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if state, ok := m.sourceSync[sourceID]; ok {
		copy := cloneSyncState(state)
		return &copy, nil
	}
	return nil, nil
}

func (m *memoryRepo) UpsertSourceSyncState(_ context.Context, state model.SourceSyncState) error {
	if !m.inTx {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
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
		LineCode:    "",
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
		LastSeenAt:        time.Date(2026, 4, 8, 10, 30, 0, 0, time.UTC),
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
		LastSeenAt:        time.Date(2026, 4, 8, 10, 31, 0, 0, time.UTC),
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
		LineCode:    "",
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

// --- Multi-inbound aggregation (see 04-minimal-scope.md "必做 #1") ---
//
// A single xray instance can serve one account through several inbounds
// (e.g. xhttp + tcp), and in the future several xray instances may each
// report the same account. The exporter emits one Sample per (uuid,
// inbound_tag) pair, but billing checkpoints on (node_id, account_uuid) with
// no inbound dimension. Without aggregating by UUID first, samples for the
// same account would overwrite each other's checkpoint, alternately
// triggering a false counter-reset and overcharging.

func TestAggregateSamplesByUUIDSumsPerInboundCounters(t *testing.T) {
	result := &model.JobResult{}
	samples := []model.Sample{
		{UUID: "11111111-1111-1111-1111-111111111111", Email: "user@example.com", InboundTag: "xhttp", UplinkBytesTotal: 100, DownlinkBytesTotal: 50},
		{UUID: "11111111-1111-1111-1111-111111111111", InboundTag: "tcp", UplinkBytesTotal: 30, DownlinkBytesTotal: 20},
		{UUID: "22222222-2222-2222-2222-222222222222", InboundTag: "xhttp", UplinkBytesTotal: 5, DownlinkBytesTotal: 5},
	}

	aggregated := aggregateSamplesByUUID(samples, result)

	if len(aggregated) != 2 {
		t.Fatalf("expected 2 aggregated samples (one per account), got %d: %#v", len(aggregated), aggregated)
	}
	first := aggregated[0]
	if first.UUID != "11111111-1111-1111-1111-111111111111" || first.UplinkBytesTotal != 130 || first.DownlinkBytesTotal != 70 {
		t.Fatalf("expected first account aggregated up=130 down=70, got %#v", first)
	}
	if first.Email != "user@example.com" {
		t.Fatalf("expected email preserved from first sample, got %q", first.Email)
	}
	if first.InboundTag != "" {
		t.Fatalf("expected aggregated sample to drop the per-inbound tag, got %q", first.InboundTag)
	}
	second := aggregated[1]
	if second.UUID != "22222222-2222-2222-2222-222222222222" || second.UplinkBytesTotal != 5 {
		t.Fatalf("expected second account unaffected by first account's samples, got %#v", second)
	}
}

func TestAggregateSamplesByUUIDSkipsInvalidSamplesButKeepsValidOnes(t *testing.T) {
	result := &model.JobResult{}
	samples := []model.Sample{
		{UUID: "not-a-uuid", InboundTag: "xhttp", UplinkBytesTotal: 100},
		{UUID: "11111111-1111-1111-1111-111111111111", InboundTag: "tcp", UplinkBytesTotal: 30},
	}

	aggregated := aggregateSamplesByUUID(samples, result)

	if len(aggregated) != 1 || aggregated[0].UUID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("expected only the valid sample to survive aggregation, got %#v", aggregated)
	}
	if result.Status != "partial" {
		t.Fatalf("expected invalid sample to mark result partial, got %q", result.Status)
	}
}

func TestMultiInboundSamplesAggregateIntoSingleAccountBucket(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 30, 15, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "prod",
				Samples: []model.Sample{
					{UUID: accountUUID, Email: "user@example.com", InboundTag: "xhttp", UplinkBytesTotal: 100, DownlinkBytesTotal: 50},
					{UUID: accountUUID, Email: "user@example.com", InboundTag: "tcp", UplinkBytesTotal: 30, DownlinkBytesTotal: 20},
				},
			})},
		},
	}
	svc := New(baseConfig(), source, repo)

	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	// Two raw samples for the same account must aggregate into exactly one
	// processed sample and one bucket, not two.
	if result.ProcessedSamples != 1 || result.WrittenMinutes != 1 {
		t.Fatalf("unexpected result %#v", result)
	}
	if len(repo.buckets) != 1 {
		t.Fatalf("expected exactly 1 bucket, got %d", len(repo.buckets))
	}
	bucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 4, 8, 10, 30, 0, 0, time.UTC),
		NodeID:      composeStorageNodeID("prod", "jp-node"),
		AccountUUID: accountUUID,
		Region:      "",
		LineCode:    "",
	})]
	if bucket.UplinkBytes != 130 || bucket.DownlinkBytes != 70 || bucket.TotalBytes != 200 {
		t.Fatalf("expected aggregated totals up=130 down=70 total=200, got %#v", bucket)
	}
}

func TestIngestSnapshotUsesPushPathWithoutExporterSource(t *testing.T) {
	repo := newMemoryRepo()
	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	svc := New(cfg, &fakeWindowSource{}, repo)

	accountUUID := "11111111-1111-1111-1111-111111111111"
	result, err := svc.IngestSnapshot(context.Background(), model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 30, 15, 0, time.UTC),
		NodeID:      "uat-node",
		Env:         "uat",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			Email:              "user@example.com",
			UplinkBytesTotal:   130,
			DownlinkBytesTotal: 70,
		}},
	})
	if err != nil {
		t.Fatalf("ingest snapshot: %v", err)
	}
	if result.ProcessedSamples != 1 || result.WrittenMinutes != 1 {
		t.Fatalf("unexpected ingest result %#v", result)
	}
	if len(repo.buckets) != 1 || len(repo.ledgers) != 1 {
		t.Fatalf("expected one bucket and ledger, buckets=%d ledgers=%d", len(repo.buckets), len(repo.ledgers))
	}
}

func TestMultiInboundCumulativeAcrossRoundsDoesNotFalseReset(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	nodeKey := composeStorageNodeID("prod", "jp-node")
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {
				singleSnapshotPage(model.Snapshot{
					CollectedAt: time.Date(2026, 4, 8, 10, 30, 0, 0, time.UTC),
					NodeID:      "jp-node",
					Env:         "prod",
					Samples: []model.Sample{
						{UUID: accountUUID, InboundTag: "xhttp", UplinkBytesTotal: 100},
						{UUID: accountUUID, InboundTag: "tcp", UplinkBytesTotal: 50},
					},
				}),
				singleSnapshotPage(model.Snapshot{
					CollectedAt: time.Date(2026, 4, 8, 10, 31, 0, 0, time.UTC),
					NodeID:      "jp-node",
					Env:         "prod",
					Samples: []model.Sample{
						{UUID: accountUUID, InboundTag: "xhttp", UplinkBytesTotal: 120},
						{UUID: accountUUID, InboundTag: "tcp", UplinkBytesTotal: 60},
					},
				}),
			},
		},
	}
	svc := New(baseConfig(), source, repo)

	if _, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate"); err != nil {
		t.Fatalf("second run: %v", err)
	}

	// Round 1: xhttp(100) + tcp(50) = 150 aggregated uplink.
	firstBucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 4, 8, 10, 30, 0, 0, time.UTC),
		NodeID:      nodeKey,
		AccountUUID: accountUUID,
	})]
	if firstBucket.UplinkBytes != 150 {
		t.Fatalf("expected first-round aggregated uplink 150, got %d", firstBucket.UplinkBytes)
	}

	// Round 2: xhttp(120) + tcp(60) = 180 aggregated; delta over round 1 must
	// be 180-150=30. Under the pre-fix per-inbound checkpoint keying,
	// interleaving these two inbounds through the same (node,uuid) checkpoint
	// would alternately look like a counter reset and drop/overcharge data.
	secondBucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 4, 8, 10, 31, 0, 0, time.UTC),
		NodeID:      nodeKey,
		AccountUUID: accountUUID,
	})]
	if secondBucket.UplinkBytes != 30 {
		t.Fatalf("expected second-round delta 30, got %d", secondBucket.UplinkBytes)
	}

	if got := repo.checkpoints[checkpointKey(nodeKey, accountUUID)].ResetEpoch; got != 0 {
		t.Fatalf("expected no false reset across rounds, got reset_epoch=%d", got)
	}
}

func TestMultiInboundOneInboundStopsReportingIsConservativeReset(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	nodeKey := composeStorageNodeID("prod", "jp-node")
	// Seed a checkpoint as if a prior round had aggregated xhttp(100)+tcp(50)=150.
	repo.checkpoints[checkpointKey(nodeKey, accountUUID)] = model.Checkpoint{
		NodeID:          nodeKey,
		AccountUUID:     accountUUID,
		LastUplinkTotal: 150,
		LastSeenAt:      time.Date(2026, 4, 8, 10, 31, 0, 0, time.UTC),
	}
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(model.Snapshot{
				CollectedAt: time.Date(2026, 4, 8, 10, 32, 0, 0, time.UTC),
				NodeID:      "jp-node",
				Env:         "prod",
				// tcp stopped reporting (e.g. xray restarted and dropped the
				// inbound); only xhttp remains.
				Samples: []model.Sample{
					{UUID: accountUUID, InboundTag: "xhttp", UplinkBytesTotal: 120},
				},
			})},
		},
	}
	svc := New(baseConfig(), source, repo)

	result, err := svc.RunCollectAndRate(context.Background(), "collect-and-rate")
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	// Aggregated total (120) is lower than the checkpoint (150) purely
	// because tcp stopped reporting. This is conservatively treated as a
	// possible counter reset rather than a negative charge -- undercounting
	// is the safe direction, never reverse an existing ledger entry.
	if result.ProcessedSamples != 0 {
		t.Fatalf("expected the sample to be treated as a reset, not processed: %#v", result)
	}
	if len(repo.buckets) != 0 || len(repo.ledgers) != 0 {
		t.Fatalf("expected no bucket/ledger writes on apparent reset")
	}
	checkpoint := repo.checkpoints[checkpointKey(nodeKey, accountUUID)]
	if checkpoint.ResetEpoch != 1 {
		t.Fatalf("expected reset epoch increment, got %d", checkpoint.ResetEpoch)
	}
	if checkpoint.LastUplinkTotal != 120 {
		t.Fatalf("expected checkpoint reset to new total 120, got %d", checkpoint.LastUplinkTotal)
	}
}

func TestMultiInboundDuplicateWindowIsReplaySafe(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	snapshot := model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 30, 30, 0, time.UTC),
		NodeID:      "jp-node",
		Env:         "prod",
		Samples: []model.Sample{
			{UUID: accountUUID, InboundTag: "xhttp", UplinkBytesTotal: 100, DownlinkBytesTotal: 50},
			{UUID: accountUUID, InboundTag: "tcp", UplinkBytesTotal: 30, DownlinkBytesTotal: 20},
		},
	}
	source := &fakeWindowSource{
		pagesBySource: map[string][]model.SnapshotWindowPage{
			"default": {singleSnapshotPage(snapshot), singleSnapshotPage(snapshot)},
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
		t.Fatalf("expected replayed minutes on duplicate multi-inbound window, got %#v", result)
	}
	if len(repo.ledgers) != 1 {
		t.Fatalf("expected exactly 1 ledger entry despite replay, got %d", len(repo.ledgers))
	}
}

func TestExactDuplicateReplayPreservesBucketAndLedgerWithoutZeroing(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	nodeKey := composeStorageNodeID("uat", "uat-node")
	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	svc := New(cfg, &fakeWindowSource{}, repo)

	snapshot := model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 30, 30, 0, time.UTC),
		NodeID:      "uat-node",
		Env:         "uat",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			Email:              "user@example.com",
			UplinkBytesTotal:   100,
			DownlinkBytesTotal: 50,
		}},
	}

	first, err := svc.IngestSnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.ProcessedSamples != 1 || first.WrittenMinutes != 1 || first.ReplayedMinutes != 0 {
		t.Fatalf("unexpected first result: %#v", first)
	}

	second, err := svc.IngestSnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if second.ProcessedSamples != 1 || second.ReplayedMinutes != 1 {
		t.Fatalf("unexpected second result: %#v", second)
	}

	bucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 4, 8, 10, 30, 0, 0, time.UTC),
		NodeID:      nodeKey,
		AccountUUID: accountUUID,
	})]
	if bucket.UplinkBytes != 100 || bucket.DownlinkBytes != 50 || bucket.TotalBytes != 150 {
		t.Fatalf("duplicate replay zeroed or corrupted minute bucket: %#v", bucket)
	}

	if len(repo.ledgers) != 1 {
		t.Fatalf("expected 1 ledger entry, got %d", len(repo.ledgers))
	}
	for _, entry := range repo.ledgers {
		if entry.RatedBytes != 150 || entry.AmountDelta != -75 || entry.BalanceAfter != -75 {
			t.Fatalf("duplicate replay zeroed or corrupted ledger entry: %#v", entry)
		}
	}

	quota := repo.quotas[accountUUID]
	if quota.CurrentBalance != -75 {
		t.Fatalf("expected balance -75 without double charge, got %v", quota.CurrentBalance)
	}
}

func TestStaleOutOfOrderSnapshotIsIgnoredWithoutFalseReset(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	nodeKey := composeStorageNodeID("uat", "uat-node")
	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	svc := New(cfg, &fakeWindowSource{}, repo)

	newer := model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 31, 0, 0, time.UTC),
		NodeID:      "uat-node",
		Env:         "uat",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   200,
			DownlinkBytesTotal: 100,
		}},
	}
	if _, err := svc.IngestSnapshot(context.Background(), newer); err != nil {
		t.Fatalf("ingest newer snapshot: %v", err)
	}

	stale := model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 30, 0, 0, time.UTC),
		NodeID:      "uat-node",
		Env:         "uat",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   120,
			DownlinkBytesTotal: 60,
		}},
	}
	res, err := svc.IngestSnapshot(context.Background(), stale)
	if err != nil {
		t.Fatalf("ingest stale snapshot: %v", err)
	}
	if res.ProcessedSamples != 0 {
		t.Fatalf("expected stale snapshot to be ignored, got %#v", res)
	}

	cp := repo.checkpoints[checkpointKey(nodeKey, accountUUID)]
	if cp.ResetEpoch != 0 {
		t.Fatalf("stale snapshot triggered false counter reset: reset_epoch=%d", cp.ResetEpoch)
	}
	if cp.LastUplinkTotal != 200 || cp.LastDownlinkTotal != 100 {
		t.Fatalf("stale snapshot overwrote checkpoint totals: %#v", cp)
	}
	if !cp.LastSeenAt.Equal(newer.CollectedAt) {
		t.Fatalf("stale snapshot rewound checkpoint last_seen_at: %v", cp.LastSeenAt)
	}
	if len(repo.buckets) != 1 || len(repo.ledgers) != 1 {
		t.Fatalf("stale snapshot wrote unexpected bucket/ledger rows: buckets=%d ledgers=%d", len(repo.buckets), len(repo.ledgers))
	}
	if got := repo.quotas[accountUUID].CurrentBalance; got != -150 {
		t.Fatalf("expected balance -150 to remain unchanged, got %v", got)
	}
}

func TestSameMinuteMultipleIncreasingSnapshotsAccumulateWithoutLossOrDoubleCharge(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	nodeKey := composeStorageNodeID("uat", "uat-node")
	repo.profiles[accountUUID] = model.BillingProfile{
		AccountUUID:        accountUUID,
		PackageName:        "starter",
		IncludedQuotaBytes: 120,
		BasePricePerByte:   0.5,
		RegionMultiplier:   1.0,
		LineMultiplier:     1.0,
		PricingRuleVersion: "pricing-v1",
	}
	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	svc := New(cfg, &fakeWindowSource{}, repo)

	snap1 := model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 30, 10, 0, time.UTC),
		NodeID:      "uat-node",
		Env:         "uat",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   70,
			DownlinkBytesTotal: 30,
		}},
	}
	// +150B in the same UTC minute (total 250B across snap1 + snap2)
	snap2 := model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 30, 50, 0, time.UTC),
		NodeID:      "uat-node",
		Env:         "uat",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   170,
			DownlinkBytesTotal: 80,
		}},
	}

	if _, err := svc.IngestSnapshot(context.Background(), snap1); err != nil {
		t.Fatalf("ingest snap1: %v", err)
	}
	if _, err := svc.IngestSnapshot(context.Background(), snap2); err != nil {
		t.Fatalf("ingest snap2: %v", err)
	}

	bucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 4, 8, 10, 30, 0, 0, time.UTC),
		NodeID:      nodeKey,
		AccountUUID: accountUUID,
	})]
	if bucket.UplinkBytes != 170 || bucket.DownlinkBytes != 80 || bucket.TotalBytes != 250 {
		t.Fatalf("expected cumulative minute bucket 170/80/250, got %#v", bucket)
	}

	// Total 250B minus 120B included quota = 130B chargeable @ 0.5 = -65
	quota := repo.quotas[accountUUID]
	if quota.RemainingIncludedQuota != 0 {
		t.Fatalf("expected remaining included quota 0, got %d", quota.RemainingIncludedQuota)
	}
	if quota.CurrentBalance != -65 {
		t.Fatalf("expected balance -65 for 130 chargeable bytes, got %v", quota.CurrentBalance)
	}

	if len(repo.ledgers) != 1 {
		t.Fatalf("expected 1 ledger entry for the minute, got %d", len(repo.ledgers))
	}
	for _, entry := range repo.ledgers {
		if entry.RatedBytes != 130 || entry.AmountDelta != -65 || entry.BalanceAfter != -65 {
			t.Fatalf("expected cumulative ledger rated_bytes=130 amount_delta=-65 balance_after=-65, got %#v", entry)
		}
	}

	// Re-delivering both snap1 (now stale) and snap2 (now duplicate) must be a no-op on state.
	if _, err := svc.IngestSnapshot(context.Background(), snap1); err != nil {
		t.Fatalf("re-ingest snap1: %v", err)
	}
	if _, err := svc.IngestSnapshot(context.Background(), snap2); err != nil {
		t.Fatalf("re-ingest snap2: %v", err)
	}
	if got := repo.quotas[accountUUID].CurrentBalance; got != -65 {
		t.Fatalf("expected balance -65 after replay of snap1+snap2, got %v", got)
	}
}

func TestAtomicSampleTransactionRollsBackOnPartialFailureAndSucceedsOnRetry(t *testing.T) {
	accountUUID := "11111111-1111-1111-1111-111111111111"
	snapshot := model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 30, 15, 0, time.UTC),
		NodeID:      "uat-node",
		Env:         "uat",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   100,
			DownlinkBytesTotal: 50,
		}},
	}

	steps := []struct {
		name   string
		inject func(r *memoryRepo)
		clear  func(r *memoryRepo)
	}{
		{
			name:   "UpsertMinuteBucketError",
			inject: func(r *memoryRepo) { r.upsertBucketErr = errors.New("bucket write failed") },
			clear:  func(r *memoryRepo) { r.upsertBucketErr = nil },
		},
		{
			name:   "UpsertLedgerError",
			inject: func(r *memoryRepo) { r.upsertLedgerErr = errors.New("ledger write failed") },
			clear:  func(r *memoryRepo) { r.upsertLedgerErr = nil },
		},
		{
			name:   "UpsertQuotaStateError",
			inject: func(r *memoryRepo) { r.upsertQuotaStateErr = errors.New("quota write failed") },
			clear:  func(r *memoryRepo) { r.upsertQuotaStateErr = nil },
		},
		{
			name:   "UpsertCheckpointError",
			inject: func(r *memoryRepo) { r.upsertCheckpointErr = errors.New("checkpoint write failed") },
			clear:  func(r *memoryRepo) { r.upsertCheckpointErr = nil },
		},
	}

	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemoryRepo()
			cfg := baseConfig()
			cfg.ExporterSources = nil
			cfg.PullEnabled = false
			svc := New(cfg, &fakeWindowSource{}, repo)

			tc.inject(repo)
			if _, err := svc.IngestSnapshot(context.Background(), snapshot); err == nil {
				t.Fatalf("expected error from %s", tc.name)
			}

			// Verify full atomic rollback: no partial bucket, ledger, quota, or checkpoint left behind.
			if len(repo.buckets) != 0 || len(repo.ledgers) != 0 || len(repo.quotas) != 0 || len(repo.checkpoints) != 0 {
				t.Fatalf("partial state leaked after %s rollback: buckets=%d ledgers=%d quotas=%d checkpoints=%d",
					tc.name, len(repo.buckets), len(repo.ledgers), len(repo.quotas), len(repo.checkpoints))
			}

			// Clear error and retry: must succeed cleanly and charge exactly once.
			tc.clear(repo)
			res, err := svc.IngestSnapshot(context.Background(), snapshot)
			if err != nil {
				t.Fatalf("retry after %s: %v", tc.name, err)
			}
			if res.ProcessedSamples != 1 || res.WrittenMinutes != 1 {
				t.Fatalf("unexpected retry result: %#v", res)
			}
			if got := repo.quotas[accountUUID].CurrentBalance; got != -75 {
				t.Fatalf("expected balance -75 after retry, got %v", got)
			}
		})
	}
}

func TestConcurrentDuplicateSnapshotDeliveriesAreRaceAndDoubleChargeSafe(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false

	snapshot := model.Snapshot{
		CollectedAt: time.Date(2026, 4, 8, 10, 30, 30, 0, time.UTC),
		NodeID:      "uat-node",
		Env:         "uat",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   100,
			DownlinkBytesTotal: 50,
		}},
	}

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			svc := New(cfg, &fakeWindowSource{}, repo)
			if _, err := svc.IngestSnapshot(context.Background(), snapshot); err != nil {
				t.Errorf("concurrent ingest error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := repo.quotas[accountUUID].CurrentBalance; got != -75 {
		t.Fatalf("expected single charge of -75 across %d concurrent deliveries, got %v", workers, got)
	}
	for _, bucket := range repo.buckets {
		if bucket.TotalBytes != 150 {
			t.Fatalf("expected bucket total_bytes=150, got %d", bucket.TotalBytes)
		}
	}
	for _, entry := range repo.ledgers {
		if entry.RatedBytes != 150 || entry.AmountDelta != -75 {
			t.Fatalf("expected ledger rated_bytes=150 amount_delta=-75, got %#v", entry)
		}
	}
}
