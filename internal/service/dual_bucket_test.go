package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"billing-service/internal/model"
	"billing-service/internal/repository"
)

type threadSafeNodeReader struct {
	mu    sync.RWMutex
	meta  map[string]model.NodePoolMetadata
	calls atomic.Int64
	err   error
}

func (r *threadSafeNodeReader) GetNodePoolMetadata(_ context.Context, nodeID string) (*model.NodePoolMetadata, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if m, ok := r.meta[nodeID]; ok {
		copied := m
		copied.Groups = append([]string(nil), m.Groups...)
		return &copied, nil
	}
	return nil, nil
}

func TestNodePoolResolverResolutionAndFallback(t *testing.T) {
	reader := &threadSafeNodeReader{
		meta: map[string]model.NodePoolMetadata{
			"jp-xconnect-pro-01.svc.plus": {
				NodeID: "jp-xconnect-pro-01.svc.plus",
				Groups: []string{"agent:configure-public", "resource:pro"},
			},
			"jp-xconnect-01.svc.plus": {
				NodeID: "jp-xconnect-01.svc.plus",
				Groups: []string{"resource:plus"},
			},
			"jp-xconnect-free-01.svc.plus": {
				NodeID: "jp-xconnect-free-01.svc.plus",
				Groups: []string{"resource:free"},
			},
			"hk-shared-01.svc.plus": {
				NodeID: "hk-shared-01.svc.plus",
				Groups: []string{"resource:shared"},
			},
			"us-xconnect.svc.plus": {
				NodeID: "us-xconnect.svc.plus",
				Groups: []string{"resource:custom"},
			},
			"team-edge-01.svc.plus": {
				NodeID: "team-edge-01.svc.plus",
				Groups: []string{"team:acme"},
			},
			"pricing-only-pro.svc.plus": {
				NodeID:       "pricing-only-pro.svc.plus",
				PricingGroup: "pro",
			},
			"pricing-only-plus.svc.plus": {
				NodeID:       "pricing-only-plus.svc.plus",
				PricingGroup: "plus",
			},
			"pricing-only-custom.svc.plus": {
				NodeID:       "pricing-only-custom.svc.plus",
				PricingGroup: "custom",
			},
		},
	}

	resolver := NewNodePoolResolver(reader, 60*time.Second, slog.Default())
	ctx := context.Background()

	cases := []struct {
		name          string
		storageNodeID string
		snapshotPool  string
		wantPool      string
		wantUnknown   int64
	}{
		{name: "pro from groups", storageNodeID: "prod:jp-xconnect-pro-01.svc.plus", wantPool: PoolProEC2},
		{name: "plus from groups", storageNodeID: "prod:jp-xconnect-01.svc.plus", wantPool: PoolMedia},
		{name: "free from groups", storageNodeID: "prod:jp-xconnect-free-01.svc.plus", wantPool: PoolMedia},
		{name: "shared from groups", storageNodeID: "uat:hk-shared-01.svc.plus", wantPool: PoolMedia},
		{name: "custom from groups", storageNodeID: "prod:us-xconnect.svc.plus", wantPool: PoolCustomDedicated},
		{name: "team from groups", storageNodeID: "prod:team-edge-01.svc.plus", wantPool: PoolCustomDedicated},
		{name: "pro from pricing_group", storageNodeID: "prod:pricing-only-pro.svc.plus", wantPool: PoolProEC2},
		{name: "plus from pricing_group", storageNodeID: "prod:pricing-only-plus.svc.plus", wantPool: PoolMedia},
		{name: "custom from pricing_group", storageNodeID: "prod:pricing-only-custom.svc.plus", wantPool: PoolCustomDedicated},
		{name: "snapshot fallback when unknown in DB", storageNodeID: "prod:new-edge.svc.plus", snapshotPool: PoolProEC2, wantPool: PoolProEC2},
		{name: "unknown defaults to Media-Pool and increments counter", storageNodeID: "prod:unknown-edge.svc.plus", snapshotPool: "", wantPool: PoolMedia, wantUnknown: 1},
		{name: "invalid snapshot fallback defaults to Media-Pool", storageNodeID: "prod:bad-pool-edge.svc.plus", snapshotPool: "unknown-pool", wantPool: PoolMedia, wantUnknown: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolver.Resolve(ctx, tc.storageNodeID, tc.snapshotPool)
			if got != tc.wantPool {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", tc.storageNodeID, tc.snapshotPool, got, tc.wantPool)
			}
			if tc.wantUnknown > 0 && resolver.UnknownPoolCount() != tc.wantUnknown {
				t.Fatalf("UnknownPoolCount = %d, want %d", resolver.UnknownPoolCount(), tc.wantUnknown)
			}
		})
	}
}

func TestNodePoolResolverTTLCacheAndConcurrency(t *testing.T) {
	reader := &threadSafeNodeReader{
		meta: map[string]model.NodePoolMetadata{
			"jp-xconnect-pro-01.svc.plus": {
				NodeID: "jp-xconnect-pro-01.svc.plus",
				Groups: []string{"resource:pro"},
			},
		},
	}

	currentTime := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	resolver := NewNodePoolResolver(reader, 60*time.Second, slog.Default())
	resolver.now = func() time.Time { return currentTime }

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if got := resolver.Resolve(ctx, "prod:jp-xconnect-pro-01.svc.plus", ""); got != PoolProEC2 {
			t.Fatalf("got %q, want %q", got, PoolProEC2)
		}
	}
	if calls := reader.calls.Load(); calls != 1 {
		t.Fatalf("expected 1 DB lookup within 60s TTL, got %d", calls)
	}

	// Update metadata and advance clock past 60s TTL.
	reader.mu.Lock()
	reader.meta["jp-xconnect-pro-01.svc.plus"] = model.NodePoolMetadata{
		NodeID: "jp-xconnect-pro-01.svc.plus",
		Groups: []string{"resource:plus"},
	}
	reader.mu.Unlock()
	currentTime = currentTime.Add(61 * time.Second)

	if got := resolver.Resolve(ctx, "prod:jp-xconnect-pro-01.svc.plus", ""); got != PoolMedia {
		t.Fatalf("expected refreshed pool %q after TTL expiry, got %q", PoolMedia, got)
	}
	if calls := reader.calls.Load(); calls != 2 {
		t.Fatalf("expected 2 DB lookups after TTL expiry, got %d", calls)
	}

	// Concurrent reads under -race must stay safe.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			node := "prod:jp-xconnect-pro-01.svc.plus"
			if idx%2 == 1 {
				node = "prod:missing.svc.plus"
			}
			_ = resolver.Resolve(ctx, node, "")
		}(i)
	}
	wg.Wait()
}

func TestNodePoolResolverHandlesReaderErrorGracefully(t *testing.T) {
	reader := &threadSafeNodeReader{err: errors.New("db unavailable")}
	resolver := NewNodePoolResolver(reader, 60*time.Second, slog.Default())

	if got := resolver.Resolve(context.Background(), "prod:jp-xconnect-pro-01.svc.plus", ""); got != PoolMedia {
		t.Fatalf("expected Media-Pool fallback on DB error, got %q", got)
	}
	if resolver.UnknownPoolCount() != 1 {
		t.Fatalf("expected UnknownPoolCount=1, got %d", resolver.UnknownPoolCount())
	}
}

func TestDualBucketShadowModePreservesVPSDeductionAndTracksShadow(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	repo.nodeMetadata["jp-xconnect-pro-01.svc.plus"] = model.NodePoolMetadata{
		NodeID: "jp-xconnect-pro-01.svc.plus",
		Groups: []string{"resource:pro"},
	}
	repo.profiles[accountUUID] = model.BillingProfile{
		AccountUUID:         accountUUID,
		PackageName:         "pro",
		IncludedQuotaBytes:  5000,
		HighSpeedQuotaBytes: 3000,
		BasePricePerByte:    0.5,
		PricingRuleVersion:  "pro-v2",
	}
	repo.quotas[accountUUID] = model.QuotaState{
		AccountUUID:             accountUUID,
		RemainingIncludedQuota:  5000,
		RemainingHighSpeedQuota: 3000,
		CurrentBalance:          100,
		ThrottleState:           "normal",
		SuspendState:            "active",
	}

	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	cfg.DualBucketQuotaEnforce = false // Shadow mode

	svc := New(cfg, nil, repo)
	snap := model.Snapshot{
		CollectedAt: time.Date(2026, 11, 21, 10, 0, 15, 0, time.UTC),
		NodeID:      "jp-xconnect-pro-01.svc.plus",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   600,
			DownlinkBytesTotal: 400,
		}},
	}

	res, err := svc.IngestSnapshot(context.Background(), snap)
	if err != nil {
		t.Fatalf("ingest snapshot: %v", err)
	}
	if res.WrittenMinutes != 1 {
		t.Fatalf("expected 1 written minute, got %#v", res)
	}

	bucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
		NodeID:      "prod:jp-xconnect-pro-01.svc.plus",
		AccountUUID: accountUUID,
	})]
	if bucket.RoutingPool != PoolProEC2 || bucket.RatingStatus != "rated" {
		t.Fatalf("unexpected bucket metadata: %+v", bucket)
	}

	quota := repo.quotas[accountUUID]
	if quota.RemainingIncludedQuota != 4000 {
		t.Fatalf("shadow mode must preserve legacy RemainingIncludedQuota deduction: got %d, want 4000", quota.RemainingIncludedQuota)
	}
	if quota.RemainingHighSpeedQuota != 3000 {
		t.Fatalf("shadow mode must not mutate RemainingHighSpeedQuota: got %d, want 3000", quota.RemainingHighSpeedQuota)
	}
	if svc.ShadowHighSpeedBytes() != 1000 {
		t.Fatalf("expected ShadowHighSpeedBytes=1000, got %d", svc.ShadowHighSpeedBytes())
	}

	// Replay same minute must be idempotent for both quota and shadow counter.
	res, err = svc.IngestSnapshot(context.Background(), snap)
	if err != nil {
		t.Fatalf("replay snapshot: %v", err)
	}
	if res.ReplayedMinutes == 0 {
		t.Fatalf("expected ReplayedMinutes > 0, got %#v", res)
	}
	if repo.quotas[accountUUID].RemainingIncludedQuota != 4000 || repo.quotas[accountUUID].RemainingHighSpeedQuota != 3000 {
		t.Fatalf("replay modified quota: %+v", repo.quotas[accountUUID])
	}
	if svc.ShadowHighSpeedBytes() != 1000 {
		t.Fatalf("replay must not double count ShadowHighSpeedBytes: got %d", svc.ShadowHighSpeedBytes())
	}
}

func TestDualBucketEnforceModeIsolatesPoolsAndHandlesOverageAndReplay(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	repo.nodeMetadata["jp-xconnect-pro-01.svc.plus"] = model.NodePoolMetadata{
		NodeID: "jp-xconnect-pro-01.svc.plus",
		Groups: []string{"resource:pro"},
	}
	repo.nodeMetadata["jp-xconnect-01.svc.plus"] = model.NodePoolMetadata{
		NodeID: "jp-xconnect-01.svc.plus",
		Groups: []string{"resource:plus"},
	}
	repo.profiles[accountUUID] = model.BillingProfile{
		AccountUUID:         accountUUID,
		PackageName:         "pro",
		IncludedQuotaBytes:  5000,
		HighSpeedQuotaBytes: 3000,
		BasePricePerByte:    0.5,
		PricingRuleVersion:  "pro-v2",
	}

	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	cfg.InitialBalance = 100
	cfg.DualBucketQuotaEnforce = true

	svc := New(cfg, nil, repo)
	ctx := context.Background()

	// 1. Pro-EC2-Pool consumes 1000 bytes -> initializes both buckets from BillingProfile, deducts ONLY RemainingHighSpeedQuota.
	proSnap1 := model.Snapshot{
		CollectedAt: time.Date(2026, 11, 21, 10, 0, 15, 0, time.UTC),
		NodeID:      "jp-xconnect-pro-01.svc.plus",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   600,
			DownlinkBytesTotal: 400,
		}},
	}
	if _, err := svc.IngestSnapshot(ctx, proSnap1); err != nil {
		t.Fatalf("pro ingest 1: %v", err)
	}
	q := repo.quotas[accountUUID]
	if q.RemainingHighSpeedQuota != 2000 || q.RemainingIncludedQuota != 5000 || q.CurrentBalance != 100 {
		t.Fatalf("unexpected quota after Pro ingest 1: %+v", q)
	}

	// Replay Pro snapshot 1 -> must not double deduct RemainingHighSpeedQuota.
	res, err := svc.IngestSnapshot(ctx, proSnap1)
	if err != nil {
		t.Fatalf("pro replay 1: %v", err)
	}
	if res.ReplayedMinutes == 0 || repo.quotas[accountUUID].RemainingHighSpeedQuota != 2000 {
		t.Fatalf("expected idempotent replay on Pro bucket, res=%+v quota=%+v", res, repo.quotas[accountUUID])
	}

	// 2. Media-Pool consumes 1500 bytes -> deducts ONLY RemainingIncludedQuota, leaves RemainingHighSpeedQuota untouched.
	mediaSnap1 := model.Snapshot{
		CollectedAt: time.Date(2026, 11, 21, 10, 1, 15, 0, time.UTC),
		NodeID:      "jp-xconnect-01.svc.plus",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   1000,
			DownlinkBytesTotal: 500,
		}},
	}
	if _, err := svc.IngestSnapshot(ctx, mediaSnap1); err != nil {
		t.Fatalf("media ingest 1: %v", err)
	}
	q = repo.quotas[accountUUID]
	if q.RemainingIncludedQuota != 3500 || q.RemainingHighSpeedQuota != 2000 || q.CurrentBalance != 100 {
		t.Fatalf("unexpected quota after Media ingest 1: %+v", q)
	}

	mediaBucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: time.Date(2026, 11, 21, 10, 1, 0, 0, time.UTC),
		NodeID:      "prod:jp-xconnect-01.svc.plus",
		AccountUUID: accountUUID,
	})]
	if mediaBucket.RoutingPool != PoolMedia || mediaBucket.RatingStatus != "rated" {
		t.Fatalf("unexpected media bucket: %+v", mediaBucket)
	}

	// 3. Pro-EC2-Pool overage: cumulative 3500 (delta 2500 > remaining 2000) -> RemainingHighSpeedQuota hits 0, VPS quota stays 3500, NO negative balance debit, proxy stays active while VPS quota remains!
	proSnap2 := model.Snapshot{
		CollectedAt: time.Date(2026, 11, 21, 10, 2, 15, 0, time.UTC),
		NodeID:      "jp-xconnect-pro-01.svc.plus",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   2100,
			DownlinkBytesTotal: 1400,
		}},
	}
	if _, err := svc.IngestSnapshot(ctx, proSnap2); err != nil {
		t.Fatalf("pro ingest 2 overage: %v", err)
	}
	q = repo.quotas[accountUUID]
	if q.RemainingHighSpeedQuota != 0 {
		t.Fatalf("expected RemainingHighSpeedQuota=0 after overage, got %d", q.RemainingHighSpeedQuota)
	}
	if q.RemainingIncludedQuota != 3500 {
		t.Fatalf("Pro overage must leave RemainingIncludedQuota untouched at 3500, got %d", q.RemainingIncludedQuota)
	}
	if q.CurrentBalance != 100 || q.Arrears || q.ProxyAccessState != "active" || q.SuspendState != "active" {
		t.Fatalf("expected balance untouched and proxy active while VPS quota remains: %+v", q)
	}

	// 4. Exhaust Media-Pool remaining 3500 -> both buckets 0 triggers proxy_access_state='paused' with suspend_state='active'.
	mediaSnap2 := model.Snapshot{
		CollectedAt: time.Date(2026, 11, 21, 10, 3, 15, 0, time.UTC),
		NodeID:      "jp-xconnect-01.svc.plus",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   3000,
			DownlinkBytesTotal: 2500,
		}},
	}
	if _, err := svc.IngestSnapshot(ctx, mediaSnap2); err != nil {
		t.Fatalf("media ingest 2 exhaustion: %v", err)
	}
	q = repo.quotas[accountUUID]
	if q.RemainingIncludedQuota != 0 || q.RemainingHighSpeedQuota != 0 || q.ProxyAccessState != "paused" || q.SuspendState != "active" {
		t.Fatalf("exhausting both Pro buckets must pause proxy access while keeping suspend_state active: %+v", q)
	}
}

func TestOneTimeTrialSharesSingleBucketAcrossPublicPoolsAndPausesOnExhaustion(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "22222222-2222-2222-2222-222222222222"
	repo.nodeMetadata["jp-xconnect-pro-01.svc.plus"] = model.NodePoolMetadata{
		NodeID: "jp-xconnect-pro-01.svc.plus",
		Groups: []string{"resource:pro"},
	}
	repo.nodeMetadata["jp-xconnect-01.svc.plus"] = model.NodePoolMetadata{
		NodeID: "jp-xconnect-01.svc.plus",
		Groups: []string{"resource:plus"},
	}
	repo.profiles[accountUUID] = model.BillingProfile{
		AccountUUID:        accountUUID,
		PackageName:        "trial",
		IncludedQuotaBytes: 5000,
		PricingRuleVersion: "plan:TRIAL-5GB-ONCE",
	}

	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	cfg.DualBucketQuotaEnforce = true
	svc := New(cfg, nil, repo)
	ctx := context.Background()

	// Trial consumption on Pro-EC2-Pool deducts from the single RemainingIncludedQuota bucket.
	if _, err := svc.IngestSnapshot(ctx, model.Snapshot{
		CollectedAt: time.Date(2026, 11, 23, 10, 0, 15, 0, time.UTC),
		NodeID:      "jp-xconnect-pro-01.svc.plus",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   1200,
			DownlinkBytesTotal: 800,
		}},
	}); err != nil {
		t.Fatalf("trial Pro pool ingest: %v", err)
	}
	q := repo.quotas[accountUUID]
	if q.RemainingIncludedQuota != 3000 || q.ProxyAccessState != "active" || q.CurrentBalance != 0 {
		t.Fatalf("unexpected trial state after Pro-EC2 usage: %+v", q)
	}

	// Remaining 3000 bytes exhausted on Media-Pool -> transitions to paused without arrears or account suspension.
	if _, err := svc.IngestSnapshot(ctx, model.Snapshot{
		CollectedAt: time.Date(2026, 11, 23, 10, 1, 15, 0, time.UTC),
		NodeID:      "jp-xconnect-01.svc.plus",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   2000,
			DownlinkBytesTotal: 1500,
		}},
	}); err != nil {
		t.Fatalf("trial Media pool exhaustion: %v", err)
	}
	q = repo.quotas[accountUUID]
	if q.RemainingIncludedQuota != 0 || q.ProxyAccessState != "paused" || q.SuspendState != "active" || q.Arrears {
		t.Fatalf("exhausted one-time trial must pause traffic without arrears or account suspension: %+v", q)
	}
}

func TestCustomDedicatedExemptFromQuotaAndCharges(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	repo.nodeMetadata["us-xconnect.svc.plus"] = model.NodePoolMetadata{
		NodeID: "us-xconnect.svc.plus",
		Groups: []string{"resource:custom"},
	}
	repo.quotas[accountUUID] = model.QuotaState{
		AccountUUID:             accountUUID,
		RemainingIncludedQuota:  5000,
		RemainingHighSpeedQuota: 3000,
		CurrentBalance:          42.5,
		ThrottleState:           "normal",
		SuspendState:            "active",
	}

	for _, enforce := range []bool{false, true} {
		cfg := baseConfig()
		cfg.ExporterSources = nil
		cfg.PullEnabled = false
		cfg.DualBucketQuotaEnforce = enforce
		svc := New(cfg, nil, repo)

		collected := time.Date(2026, 11, 21, 11, 0, 10, 0, time.UTC)
		if enforce {
			collected = collected.Add(time.Minute)
		}
		res, err := svc.IngestSnapshot(context.Background(), model.Snapshot{
			CollectedAt: collected,
			NodeID:      "us-xconnect.svc.plus",
			Env:         "prod",
			Samples: []model.Sample{{
				UUID:               accountUUID,
				UplinkBytesTotal:   1000,
				DownlinkBytesTotal: 1000,
			}},
		})
		if err != nil {
			t.Fatalf("custom ingest (enforce=%v): %v", enforce, err)
		}
		if res.WrittenMinutes != 1 {
			t.Fatalf("expected 1 written minute, got %#v", res)
		}

		bucket := repo.buckets[bucketKey(model.MinuteBucket{
			BucketStart: collected.UTC().Truncate(time.Minute),
			NodeID:      "prod:us-xconnect.svc.plus",
			AccountUUID: accountUUID,
		})]
		if bucket.RoutingPool != PoolCustomDedicated || bucket.RatingStatus != "exempt" {
			t.Fatalf("expected RoutingPool=Custom-Dedicated and RatingStatus=exempt, got %+v", bucket)
		}
		if len(repo.ledgers) != 0 {
			t.Fatalf("expected zero ledger charges for Custom-Dedicated, got %d", len(repo.ledgers))
		}
		q := repo.quotas[accountUUID]
		if q.RemainingIncludedQuota != 5000 || q.RemainingHighSpeedQuota != 3000 || q.CurrentBalance != 42.5 {
			t.Fatalf("Custom-Dedicated must not deduct quota or charge wallet: %+v", q)
		}
	}
}

func TestDualBucketLateBucketProtectionPreservesBothBuckets(t *testing.T) {
	repo := newMemoryRepo()
	accountUUID := "11111111-1111-1111-1111-111111111111"
	repo.nodeMetadata["jp-xconnect-pro-01.svc.plus"] = model.NodePoolMetadata{
		NodeID: "jp-xconnect-pro-01.svc.plus",
		Groups: []string{"resource:pro"},
	}
	cutoff := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	repo.quotas[accountUUID] = model.QuotaState{
		AccountUUID:             accountUUID,
		RemainingIncludedQuota:  5000,
		RemainingHighSpeedQuota: 3000,
		CurrentBalance:          50,
		PeriodStart:             &cutoff,
		PeriodEnd:               &end,
	}

	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	cfg.DualBucketQuotaEnforce = true
	svc := New(cfg, nil, repo)

	// Late bucket preceding PeriodStart must not consume RemainingHighSpeedQuota or RemainingIncludedQuota.
	if _, err := svc.IngestSnapshot(context.Background(), model.Snapshot{
		CollectedAt: cutoff.Add(-30 * time.Second),
		NodeID:      "jp-xconnect-pro-01.svc.plus",
		Env:         "prod",
		Samples: []model.Sample{{
			UUID:               accountUUID,
			UplinkBytesTotal:   500,
			DownlinkBytesTotal: 500,
		}},
	}); err != nil {
		t.Fatalf("late pro ingest: %v", err)
	}
	q := repo.quotas[accountUUID]
	if q.RemainingHighSpeedQuota != 3000 || q.RemainingIncludedQuota != 5000 || q.CurrentBalance != 50 {
		t.Fatalf("late bucket must not deduct either bucket or balance: %+v", q)
	}
	lateBucket := repo.buckets[bucketKey(model.MinuteBucket{
		BucketStart: cutoff.Add(-time.Minute),
		NodeID:      "prod:jp-xconnect-pro-01.svc.plus",
		AccountUUID: accountUUID,
	})]
	if lateBucket.RoutingPool != PoolProEC2 || lateBucket.TotalBytes != 1000 {
		t.Fatalf("expected raw late bucket preserved with RoutingPool=Pro-EC2-Pool, got %+v", lateBucket)
	}
}

type legacyRepoNoMetadata struct {
	inner *memoryRepo
}

func (l *legacyRepoNoMetadata) GetCheckpoint(ctx context.Context, nodeID, accountUUID string) (*model.Checkpoint, error) {
	return l.inner.GetCheckpoint(ctx, nodeID, accountUUID)
}
func (l *legacyRepoNoMetadata) UpsertCheckpoint(ctx context.Context, cp model.Checkpoint) error {
	return l.inner.UpsertCheckpoint(ctx, cp)
}
func (l *legacyRepoNoMetadata) UpsertMinuteBucket(ctx context.Context, b model.MinuteBucket) (bool, error) {
	return l.inner.UpsertMinuteBucket(ctx, b)
}
func (l *legacyRepoNoMetadata) UpsertLedger(ctx context.Context, e model.LedgerEntry) (bool, error) {
	return l.inner.UpsertLedger(ctx, e)
}
func (l *legacyRepoNoMetadata) GetQuotaState(ctx context.Context, id string) (*model.QuotaState, error) {
	return l.inner.GetQuotaState(ctx, id)
}
func (l *legacyRepoNoMetadata) UpsertQuotaState(ctx context.Context, s model.QuotaState) error {
	return l.inner.UpsertQuotaState(ctx, s)
}
func (l *legacyRepoNoMetadata) ListArrearsAccounts(ctx context.Context) ([]model.QuotaState, error) {
	return l.inner.ListArrearsAccounts(ctx)
}
func (l *legacyRepoNoMetadata) GetBillingProfile(ctx context.Context, id string) (*model.BillingProfile, error) {
	return l.inner.GetBillingProfile(ctx, id)
}
func (l *legacyRepoNoMetadata) GetSourceSyncState(ctx context.Context, id string) (*model.SourceSyncState, error) {
	return l.inner.GetSourceSyncState(ctx, id)
}
func (l *legacyRepoNoMetadata) UpsertSourceSyncState(ctx context.Context, s model.SourceSyncState) error {
	return l.inner.UpsertSourceSyncState(ctx, s)
}
func (l *legacyRepoNoMetadata) UpsertCloudVendorCost(ctx context.Context, c model.CloudVendorCost) error {
	return l.inner.UpsertCloudVendorCost(ctx, c)
}

var _ repository.Repository = (*legacyRepoNoMetadata)(nil)

func TestServiceWorksWithLegacyRepositoryMock(t *testing.T) {
	inner := newMemoryRepo()
	repo := &legacyRepoNoMetadata{inner: inner}
	accountUUID := "11111111-1111-1111-1111-111111111111"
	inner.quotas[accountUUID] = model.QuotaState{
		AccountUUID:             accountUUID,
		RemainingIncludedQuota:  1000,
		RemainingHighSpeedQuota: 500,
	}

	cfg := baseConfig()
	cfg.ExporterSources = nil
	cfg.PullEnabled = false
	cfg.DualBucketQuotaEnforce = true
	svc := New(cfg, nil, repo)

	if _, err := svc.IngestSnapshot(context.Background(), model.Snapshot{
		CollectedAt: time.Date(2026, 11, 21, 12, 5, 10, 0, time.UTC),
		NodeID:      "legacy-node",
		Env:         "prod",
		Samples:     []model.Sample{{UUID: accountUUID, UplinkBytesTotal: 100}},
	}); err != nil {
		t.Fatalf("legacy repo ingest: %v", err)
	}

	if inner.quotas[accountUUID].RemainingIncludedQuota != 900 || inner.quotas[accountUUID].RemainingHighSpeedQuota != 500 {
		t.Fatalf("expected legacy repo to fall back to Media-Pool, got %+v", inner.quotas[accountUUID])
	}
	if svc.UnknownNodePoolCount() != 1 {
		t.Fatalf("expected UnknownNodePoolCount=1, got %d", svc.UnknownNodePoolCount())
	}
}
