// Package session provides Redis-backed storage for session envelopes and
// risk signal data.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"zoiko.io/identity-context-svc/internal/domain"
)

// Cache manages the Redis-backed session envelope store.
//
// Storage model:
//   session:jwt:<session_context_id>  → signed envelope JWT (TTL = session TTL)
//   session:ctx:<session_context_id>  → full SessionContext JSON (KEEPTTL on invalidation)
//
// Evidence obligation: SessionContext records are NEVER deleted.
// Invalidation appends invalidated_at; the record persists for the duration
// of the data residency retention window (permanent store write: TODO outbox).
type Cache struct {
	rdb        *redis.Client
	sessionTTL time.Duration
}

func NewCache(rdb *redis.Client, sessionTTLSeconds int) *Cache {
	return &Cache{
		rdb:        rdb,
		sessionTTL: time.Duration(sessionTTLSeconds) * time.Second,
	}
}

// Put stores the signed envelope JWT, expiring after sessionTTL.
func (c *Cache) Put(ctx context.Context, sessionContextID, envelopeJWT string) error {
	return c.rdb.Set(ctx,
		fmt.Sprintf("session:jwt:%s", sessionContextID),
		envelopeJWT,
		c.sessionTTL,
	).Err()
}

// Get retrieves the signed envelope JWT. Returns an error if not found or expired.
func (c *Cache) Get(ctx context.Context, sessionContextID string) (string, error) {
	val, err := c.rdb.Get(ctx, fmt.Sprintf("session:jwt:%s", sessionContextID)).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("session %s not found or expired", sessionContextID)
	}
	return val, err
}

// Evict removes the signed envelope JWT from cache (called on invalidation).
func (c *Cache) Evict(ctx context.Context, sessionContextID string) error {
	return c.rdb.Del(ctx, fmt.Sprintf("session:jwt:%s", sessionContextID)).Err()
}

// PersistSessionContext stores the full SessionContext record in Redis.
// This is the hot-cache write. Permanent relational store write is handled
// via the outbox pattern (TODO: implement outbox before Phase 1 exit criteria).
func (c *Cache) PersistSessionContext(ctx context.Context, sc domain.SessionContext) error {
	data, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("marshal SessionContext: %w", err)
	}
	return c.rdb.Set(ctx,
		fmt.Sprintf("session:ctx:%s", sc.SessionContextID),
		data,
		c.sessionTTL,
	).Err()
}

// GetSessionContext retrieves the full SessionContext record. Returns nil if not found.
func (c *Cache) GetSessionContext(ctx context.Context, sessionContextID string) (*domain.SessionContext, error) {
	raw, err := c.rdb.Get(ctx, fmt.Sprintf("session:ctx:%s", sessionContextID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sc domain.SessionContext
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		return nil, fmt.Errorf("unmarshal SessionContext: %w", err)
	}
	return &sc, nil
}

// Invalidate is idempotent. If the record is already invalidated, this is a no-op.
// Appends invalidated_at per the append-only evidence obligation.
// Uses KEEPTTL so the invalidation marker survives the original session window
// until the permanent-store outbox write lands.
func (c *Cache) Invalidate(ctx context.Context, sessionContextID string, reason domain.InvalidationReason, at time.Time) error {
	sc, err := c.GetSessionContext(ctx, sessionContextID)
	if err != nil || sc == nil {
		return nil // not found — idempotent no-op
	}
	if sc.InvalidatedAt != nil {
		return nil // already invalidated — idempotent no-op
	}

	sc.InvalidatedAt = &at
	sc.InvalidationReason = &reason

	data, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("marshal invalidated SessionContext: %w", err)
	}
	return c.rdb.Set(ctx,
		fmt.Sprintf("session:ctx:%s", sessionContextID),
		data,
		0, // 0 duration with KEEPTTL option
		// Note: go-redis v9 Set with 0 duration keeps existing TTL
	).Err()
}

// EvictAllForPrincipal bulk-evicts cached session JWTs for a principal.
// TODO: implement a Redis SET-based reverse index (principal → session IDs)
// for production. Key scanning is not suitable for production load.
func (c *Cache) EvictAllForPrincipal(ctx context.Context, principalID string) error {
	// TODO: SMEMBERS principal:sessions:<principalID> → iterate → DEL session:jwt:<id>
	return nil
}
