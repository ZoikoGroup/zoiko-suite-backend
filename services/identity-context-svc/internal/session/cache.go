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
//
//	session:jwt:<session_context_id>  → signed envelope JWT (TTL = session TTL)
//	session:ctx:<session_context_id>  → full SessionContext JSON (KEEPTTL on invalidation)
//	principal:sessions:<principal_id> → SET of session_context_ids for that principal
//	entity:sessions:<legal_entity_id> → SET of session_context_ids scoped to that entity
//
// The reverse index exists so EvictAllForPrincipal can find a principal's live
// sessions without scanning the keyspace. Its TTL is refreshed on every write,
// so the set always outlives its newest member; members whose session has
// already expired are pruned on the next eviction, and deleting an
// already-expired key is a no-op, so a stale member is harmless rather than a
// correctness problem.
//
// Evidence obligation: SessionContext records are NEVER deleted. Invalidation
// appends invalidated_at. Redis holds these only for the session window — the
// durable copy is Postgres session_contexts, written by session.DurableCache.
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
		sessionJWTKey(sessionContextID),
		envelopeJWT,
		c.sessionTTL,
	).Err()
}

// Get retrieves the signed envelope JWT. Returns an error if not found or expired.
func (c *Cache) Get(ctx context.Context, sessionContextID string) (string, error) {
	val, err := c.rdb.Get(ctx, sessionJWTKey(sessionContextID)).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("session %s not found or expired", sessionContextID)
	}
	return val, err
}

// Evict removes the signed envelope JWT from cache (called on invalidation).
func (c *Cache) Evict(ctx context.Context, sessionContextID string) error {
	return c.rdb.Del(ctx, sessionJWTKey(sessionContextID)).Err()
}

// PersistSessionContext stores the full SessionContext record in Redis and
// records the session in the principal's reverse index.
//
// This is the hot-cache write only. The durable evidence write is Postgres,
// performed by session.DurableCache which wraps this type.
//
// Both writes go in one transactional pipeline: a context record with no index
// entry is a session that revocation cannot find, which is precisely the
// failure the index exists to prevent.
func (c *Cache) PersistSessionContext(ctx context.Context, sc domain.SessionContext) error {
	data, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("marshal SessionContext: %w", err)
	}

	pIdx := principalSessionsKey(sc.PrincipalID)
	pipe := c.rdb.TxPipeline()
	pipe.Set(ctx, sessionCtxKey(sc.SessionContextID), data, c.sessionTTL)
	pipe.SAdd(ctx, pIdx, sc.SessionContextID)
	pipe.Expire(ctx, pIdx, c.sessionTTL)

	// The entity index answers entity.updated, where the affected sessions are
	// identified by the entity they were scoped to rather than by their holder.
	if sc.LegalEntityID != "" {
		eIdx := entitySessionsKey(sc.LegalEntityID)
		pipe.SAdd(ctx, eIdx, sc.SessionContextID)
		pipe.Expire(ctx, eIdx, c.sessionTTL)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("persist SessionContext: %w", err)
	}
	return nil
}

// GetSessionContext retrieves the full SessionContext record. Returns nil if not found.
func (c *Cache) GetSessionContext(ctx context.Context, sessionContextID string) (*domain.SessionContext, error) {
	raw, err := c.rdb.Get(ctx, sessionCtxKey(sessionContextID)).Result()
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
	// redis.KeepTTL (-1) maps to the server's KEEPTTL flag. A 0 expiration —
	// which this previously passed — does the opposite of the comment that
	// accompanied it: SET with 0 CLEARS the TTL, leaving every invalidated
	// session record in Redis permanently. The durable copy lives in Postgres,
	// so the cache entry should age out on the session's own clock.
	return c.rdb.Set(ctx,
		sessionCtxKey(sessionContextID),
		data,
		redis.KeepTTL,
	).Err()
}

// SessionIDsForPrincipal returns the session ids in the principal's reverse
// index. Members whose session has already expired may still be listed; the
// caller treats the list as candidates, not as live sessions.
//
// Reads the index rather than scanning: SCAN over a shared Redis holding every
// tenant's sessions is O(keyspace) per revocation.
func (c *Cache) SessionIDsForPrincipal(ctx context.Context, principalID string) ([]string, error) {
	ids, err := c.rdb.SMembers(ctx, principalSessionsKey(principalID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session index for principal %s: %w", principalID, err)
	}
	return ids, nil
}

// ClearPrincipalIndex drops the reverse index after a bulk revocation. The
// SessionContext records it pointed at are untouched — they are evidence.
func (c *Cache) ClearPrincipalIndex(ctx context.Context, principalID string) error {
	return c.rdb.Del(ctx, principalSessionsKey(principalID)).Err()
}

// SessionIDsForEntity returns the session ids scoped to a legal entity. Like
// SessionIDsForPrincipal these are candidates: expired members may still be
// listed and are harmless to act on.
func (c *Cache) SessionIDsForEntity(ctx context.Context, legalEntityID string) ([]string, error) {
	ids, err := c.rdb.SMembers(ctx, entitySessionsKey(legalEntityID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session index for entity %s: %w", legalEntityID, err)
	}
	return ids, nil
}

// ClearEntityIndex drops the entity reverse index after a bulk revocation.
func (c *Cache) ClearEntityIndex(ctx context.Context, legalEntityID string) error {
	return c.rdb.Del(ctx, entitySessionsKey(legalEntityID)).Err()
}

// ── Key helpers ──────────────────────────────────────────────────────────────

func sessionJWTKey(sessionContextID string) string {
	return fmt.Sprintf("session:jwt:%s", sessionContextID)
}

func sessionCtxKey(sessionContextID string) string {
	return fmt.Sprintf("session:ctx:%s", sessionContextID)
}

func principalSessionsKey(principalID string) string {
	return fmt.Sprintf("principal:sessions:%s", principalID)
}

func entitySessionsKey(legalEntityID string) string {
	return fmt.Sprintf("entity:sessions:%s", legalEntityID)
}
