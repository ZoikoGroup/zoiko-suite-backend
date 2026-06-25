package session

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"

	"zoiko.io/identity-context-svc/internal/domain"
)

// RiskSignalCache provides READ access to the asynchronously-populated
// risk signal store.
//
// ARCHITECTURE INVARIANT (Q3 resolution — do not violate):
//   This type exposes only reads. Resolve() reads from Redis here.
//   Signals are written by a SEPARATE async consumer that subscribes
//   to Kafka topics from a rules-based risk engine. That consumer is
//   not part of the HTTP request lifecycle and may NOT be called
//   synchronously from Resolve().
//
//   If the cache returns nil (empty or expired), the resolver defaults
//   to STANDARD posture and emits session.risk.changed with
//   signal_source: UNAVAILABLE. It never blocks or calls out.
//
// Key schema:
//   risk:composite:<principal_id>           → latest aggregated signal (TTL = signal.valid_to)
//   risk:signal:<principal_id>:<signal_id>  → historical signal record (TTL = 24h)
type RiskSignalCache struct {
	rdb *redis.Client
}

func NewRiskSignalCache(rdb *redis.Client) *RiskSignalCache {
	return &RiskSignalCache{rdb: rdb}
}

// GetLatestSignal returns the most recent valid risk signal for a principal,
// or nil if the cache is empty or expired.
// This is the ONLY method called from the hot path.
func (c *RiskSignalCache) GetLatestSignal(ctx context.Context, principalID string) (*domain.RiskSignalCache, error) {
	raw, err := c.rdb.Get(ctx, fmt.Sprintf("risk:composite:%s", principalID)).Result()
	if err == redis.Nil {
		return nil, nil // cache miss — caller defaults to STANDARD posture
	}
	if err != nil {
		return nil, err
	}
	var signal domain.RiskSignalCache
	if err := json.Unmarshal([]byte(raw), &signal); err != nil {
		return nil, fmt.Errorf("unmarshal RiskSignalCache: %w", err)
	}
	return &signal, nil
}

// UpsertSignal writes a new risk signal into cache, preserving the
// supersession chain per data-model §06.1 RiskSignalCache.
//
// Called by the async risk-signal consumer ONLY — never from Resolve().
func (c *RiskSignalCache) UpsertSignal(ctx context.Context, signal domain.RiskSignalCache) error {
	// Preserve supersession chain: link old record to new before overwriting
	existing, _ := c.GetLatestSignal(ctx, signal.PrincipalID)
	if existing != nil {
		supersededID := signal.RiskSignalID
		existing.SupersededBy = &supersededID
		if data, err := json.Marshal(existing); err == nil {
			_ = c.rdb.Set(ctx,
				fmt.Sprintf("risk:signal:%s:%s", signal.PrincipalID, existing.RiskSignalID),
				data,
				24*time.Hour, // history retention in cache
			).Err()
		}
	}

	ttl := time.Until(signal.ValidTo)
	if ttl <= 0 {
		return nil // already expired — do not cache
	}
	// Clamp TTL to a sane ceiling
	if ttl > 24*time.Hour {
		ttl = 24 * time.Hour
	}

	data, err := json.Marshal(signal)
	if err != nil {
		return fmt.Errorf("marshal RiskSignalCache: %w", err)
	}
	return c.rdb.Set(ctx, fmt.Sprintf("risk:composite:%s", signal.PrincipalID), data, ttl).Err()
}

// Evict removes cached signals for a principal (e.g. after DELEGATION_REVOKED).
func (c *RiskSignalCache) Evict(ctx context.Context, principalID string) error {
	return c.rdb.Del(ctx, fmt.Sprintf("risk:composite:%s", principalID)).Err()
}

// clampScore enforces [0, 100] range on signal values.
func clampScore(v int) int {
	return int(math.Min(math.Max(float64(v), 0), 100))
}
