package service

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"billing-service/internal/model"
	"billing-service/internal/repository"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	PoolProEC2          = "Pro-EC2-Pool"
	PoolMedia           = "Media-Pool"
	PoolCustomDedicated = "Custom-Dedicated"

	defaultPoolCacheTTL = 60 * time.Second
)

type cachedNodePool struct {
	pool      string
	found     bool
	expiresAt time.Time
}

// NodePoolResolver maps a storage node ID (optionally prefixed with <env>:)
// to its canonical routing pool using control-plane node metadata with a
// thread-safe TTL cache. Unknown nodes safely fall back to Media-Pool so they
// never drain scarce Pro high-speed quota.
type NodePoolResolver struct {
	reader repository.NodePoolMetadataReader
	ttl    time.Duration
	log    *slog.Logger
	now    func() time.Time

	mu    sync.RWMutex
	cache map[string]cachedNodePool

	unknownPoolTotal   atomic.Int64
	unknownPoolCounter metric.Int64Counter
}

func NewNodePoolResolver(reader repository.NodePoolMetadataReader, ttl time.Duration, logger *slog.Logger) *NodePoolResolver {
	if ttl <= 0 {
		ttl = defaultPoolCacheTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	counter, _ := otel.Meter("billing-service").Int64Counter(
		"billing_unknown_node_pool_total",
		metric.WithDescription("Count of traffic samples whose node pool could not be resolved and defaulted to Media-Pool"),
	)
	return &NodePoolResolver{
		reader:             reader,
		ttl:                ttl,
		log:                logger.With("component", "node_pool_resolver"),
		now:                func() time.Time { return time.Now().UTC() },
		cache:              make(map[string]cachedNodePool),
		unknownPoolCounter: counter,
	}
}

// UnknownPoolCount returns the number of times node pool resolution fell back
// to Media-Pool because neither database metadata nor snapshot.routing_pool
// identified a valid pool.
func (r *NodePoolResolver) UnknownPoolCount() int64 {
	return r.unknownPoolTotal.Load()
}

// Resolve returns the canonical routing pool for storageNodeID.
func (r *NodePoolResolver) Resolve(ctx context.Context, storageNodeID, snapshotRoutingPool string) string {
	nodeID := normalizeNodeID(storageNodeID)
	now := r.now()

	if nodeID != "" {
		r.mu.RLock()
		cached, ok := r.cache[nodeID]
		r.mu.RUnlock()
		if ok && now.Before(cached.expiresAt) {
			if cached.found {
				return cached.pool
			}
			return r.resolveFallback(ctx, nodeID, storageNodeID, snapshotRoutingPool)
		}
	}

	if r.reader != nil && nodeID != "" {
		meta, err := r.reader.GetNodePoolMetadata(ctx, nodeID)
		if err != nil {
			r.log.WarnContext(ctx, "failed to query node pool metadata",
				"node_id", nodeID,
				"storage_node_id", storageNodeID,
				"err", err,
			)
			return r.resolveFallback(ctx, nodeID, storageNodeID, snapshotRoutingPool)
		}
		pool, mapped := poolFromMetadata(meta)
		r.mu.Lock()
		r.cache[nodeID] = cachedNodePool{
			pool:      pool,
			found:     mapped,
			expiresAt: r.now().Add(r.ttl),
		}
		r.mu.Unlock()
		if mapped {
			return pool
		}
	}

	return r.resolveFallback(ctx, nodeID, storageNodeID, snapshotRoutingPool)
}

func (r *NodePoolResolver) resolveFallback(ctx context.Context, nodeID, storageNodeID, snapshotRoutingPool string) string {
	if pool, ok := normalizeRoutingPool(snapshotRoutingPool); ok {
		return pool
	}
	r.recordUnknownPool(ctx, nodeID, storageNodeID)
	return PoolMedia
}

func (r *NodePoolResolver) recordUnknownPool(ctx context.Context, nodeID, storageNodeID string) {
	r.unknownPoolTotal.Add(1)
	if r.unknownPoolCounter != nil {
		r.unknownPoolCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("node_id", nodeID)))
	}
	r.log.WarnContext(ctx, "unknown node routing pool; defaulting to Media-Pool",
		"node_id", nodeID,
		"storage_node_id", storageNodeID,
		"default_pool", PoolMedia,
	)
}

func normalizeNodeID(storageNodeID string) string {
	trimmed := strings.TrimSpace(storageNodeID)
	if _, rest, ok := strings.Cut(trimmed, ":"); ok {
		if normalized := strings.TrimSpace(rest); normalized != "" {
			return normalized
		}
	}
	return trimmed
}

func poolFromMetadata(meta *model.NodePoolMetadata) (string, bool) {
	if meta == nil {
		return "", false
	}
	if pool, ok := poolFromGroups(meta.Groups); ok {
		return pool, true
	}
	return poolFromPricingGroup(meta.PricingGroup)
}

func poolFromGroups(groups []string) (string, bool) {
	hasPro := false
	hasMedia := false
	for _, raw := range groups {
		g := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case g == "resource:custom" || strings.HasPrefix(g, "team:"):
			return PoolCustomDedicated, true
		case g == "resource:pro":
			hasPro = true
		case g == "resource:plus" || g == "resource:shared" || g == "resource:free":
			hasMedia = true
		}
	}
	if hasPro {
		return PoolProEC2, true
	}
	if hasMedia {
		return PoolMedia, true
	}
	return "", false
}

func poolFromPricingGroup(pricingGroup string) (string, bool) {
	pg := strings.ToLower(strings.TrimSpace(pricingGroup))
	switch {
	case pg == "":
		return "", false
	case pg == "custom" || pg == "team" || pg == "resource:custom" || pg == "custom-dedicated" || strings.HasPrefix(pg, "team:"):
		return PoolCustomDedicated, true
	case pg == "pro" || pg == "resource:pro" || pg == "pro-ec2-pool":
		return PoolProEC2, true
	case pg == "plus" || pg == "shared" || pg == "free" || pg == "resource:plus" || pg == "resource:shared" || pg == "resource:free" || pg == "media-pool":
		return PoolMedia, true
	default:
		return "", false
	}
}

func normalizeRoutingPool(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case strings.ToLower(PoolProEC2):
		return PoolProEC2, true
	case strings.ToLower(PoolMedia):
		return PoolMedia, true
	case strings.ToLower(PoolCustomDedicated):
		return PoolCustomDedicated, true
	default:
		return "", false
	}
}
