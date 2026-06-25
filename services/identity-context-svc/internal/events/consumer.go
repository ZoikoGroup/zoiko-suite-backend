package events

import (
	"context"
	"sync"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/session"
)

// Consumer handles incoming domain events that require identity-context-svc
// to invalidate caches or update local state.
//
// ALL handlers are IDEMPOTENT — duplicate events are deduplicated on EventID
// before any side-effect (doctrine §3.7).
//
// Consumed events (per spec):
//   topic: zoiko.identity.events (inbound — subscribed from upstream services)
//   - tenant.created          → pre-warm tenant cache
//   - entity.updated          → invalidate entity scope cache for affected principals
//   - authority.delegated     → invalidate delegation cache for delegate
//   - authority.revoked       → immediately invalidate ALL sessions for delegate (hard evict)
//   - authority.expired       → invalidate delegation; next resolve() picks up expiry
//   - role.updated            → invalidate role-profile cache for affected principals
type Consumer struct {
	log         *zap.Logger
	sessions    *session.Cache
	riskSignals *session.RiskSignalCache

	// processedEvents is an in-memory deduplication set.
	// TODO: replace with Redis SETNX for distributed correctness before production.
	mu              sync.Mutex
	processedEvents map[string]struct{}
}

func NewConsumer(log *zap.Logger, sessions *session.Cache, riskSignals *session.RiskSignalCache) *Consumer {
	return &Consumer{
		log:             log,
		sessions:        sessions,
		riskSignals:     riskSignals,
		processedEvents: make(map[string]struct{}),
	}
}

// HandleAuthorityRevoked triggers immediate bulk-eviction of all active sessions
// for the delegate principal. This is the hardest case — active sessions must
// no longer carry stale delegation grants.
func (c *Consumer) HandleAuthorityRevoked(ctx context.Context, eventID, delegatePrincipalID, correlationID string) {
	if !c.dedupe(eventID) {
		return
	}
	c.log.Warn("authority.revoked — evicting all sessions for delegate",
		zap.String("delegate_principal_id", delegatePrincipalID),
		zap.String("correlation_id", correlationID),
	)
	if err := c.sessions.EvictAllForPrincipal(ctx, delegatePrincipalID); err != nil {
		c.log.Error("failed to evict sessions for principal",
			zap.String("principal_id", delegatePrincipalID),
			zap.Error(err),
		)
	}
}

// HandleRoleUpdated invalidates the role-profile cache for all principals
// holding an assignment to the updated role.
func (c *Consumer) HandleRoleUpdated(ctx context.Context, eventID, roleID, correlationID string) {
	if !c.dedupe(eventID) {
		return
	}
	c.log.Info("role.updated — role-profile cache invalidation required",
		zap.String("role_id", roleID),
		zap.String("correlation_id", correlationID),
	)
	// TODO: SMEMBERS role:principals:<roleID> → iterate → DEL role-profile cache keys
}

// HandleEntityUpdated invalidates cached sessions scoped to the updated entity.
func (c *Consumer) HandleEntityUpdated(ctx context.Context, eventID, legalEntityID, correlationID string) {
	if !c.dedupe(eventID) {
		return
	}
	c.log.Info("entity.updated — session cache invalidation required",
		zap.String("legal_entity_id", legalEntityID),
		zap.String("correlation_id", correlationID),
	)
	// TODO: SMEMBERS entity:sessions:<legalEntityID> → iterate → invalidate each session
}

// HandleAuthorityDelegated invalidates the delegation cache for the delegate
// principal so the next resolve() fetches the new grant from upstream.
// No session invalidation is needed — new delegations ADD access, never remove it.
func (c *Consumer) HandleAuthorityDelegated(ctx context.Context, eventID, delegatePrincipalID, correlationID string) {
	if !c.dedupe(eventID) {
		return
	}
	c.log.Info("authority.delegated — delegation cache refresh required",
		zap.String("delegate_principal_id", delegatePrincipalID),
		zap.String("correlation_id", correlationID),
	)
	// TODO: DEL delegation:cache:<delegatePrincipalID> so next resolve() re-fetches
}

// HandleRiskSignalUpdate writes a new risk signal into the cache.
// Called by the async risk-rules consumer — never on the HTTP hot path (Q3).
func (c *Consumer) HandleRiskSignalUpdate(ctx context.Context, signal domain.RiskSignalCache) {
	if err := c.riskSignals.UpsertSignal(ctx, signal); err != nil {
		c.log.Error("failed to upsert risk signal",
			zap.String("principal_id", signal.PrincipalID),
			zap.Error(err),
		)
	}
}

// dedupe returns true if this eventID is new (first time seen).
// Returns false if the event was already processed — caller should skip.
func (c *Consumer) dedupe(eventID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.processedEvents[eventID]; seen {
		c.log.Debug("duplicate event — skipped", zap.String("event_id", eventID))
		return false
	}
	c.processedEvents[eventID] = struct{}{}
	return true
}
