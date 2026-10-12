// Package consumer provides the event handler implementation for
// policy-svc's Kafka consumer. It handles entity.created, role.updated,
// and authority.delegated events by invalidating relevant caches and
// updating internal state.
package consumer

import (
	"context"

	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/store"
)

// Handler implements EventHandler for policy-svc's consumer.
type Handler struct {
	store *store.PgStore
	log   *zap.Logger
}

// NewHandler constructs a new consumer event handler.
func NewHandler(store *store.PgStore, log *zap.Logger) *Handler {
	return &Handler{
		store: store,
		log:   log,
	}
}

// HandleEntityCreated handles entity.created events.
// When a new legal entity is created, we may need to invalidate any
// cached policy resolutions that could now apply to it.
func (h *Handler) HandleEntityCreated(ctx context.Context, event EntityCreatedEvent) error {
	h.log.Info("consumer: received entity.created",
		zap.String("event_id", event.EventID),
		zap.String("tenant_id", event.TenantID),
		zap.String("legal_entity_id", event.LegalEntityID),
		zap.String("correlation_id", event.CorrelationID),
	)

	// TODO: Invalidate decision cache for this tenant/legal_entity
	// This would require a cache implementation (see decision cache gap)

	// For now, just log — the store doesn't need immediate action
	// since policy evaluation queries the database directly.
	return nil
}

// HandleRoleUpdated handles role.updated events.
// When roles change, any cached authorization decisions or policy
// evaluations that depend on role-based access may be stale.
func (h *Handler) HandleRoleUpdated(ctx context.Context, event RoleUpdatedEvent) error {
	h.log.Info("consumer: received role.updated",
		zap.String("event_id", event.EventID),
		zap.String("tenant_id", event.TenantID),
		zap.String("legal_entity_id", event.LegalEntityID),
		zap.String("correlation_id", event.CorrelationID),
	)

	// TODO: Invalidate decision cache for this tenant/legal_entity
	return nil
}

// HandleAuthorityDelegated handles authority.delegated events.
// When delegations change, policy evaluations that depend on
// delegated authority may be affected.
func (h *Handler) HandleAuthorityDelegated(ctx context.Context, event AuthorityDelegatedEvent) error {
	h.log.Info("consumer: received authority.delegated",
		zap.String("event_id", event.EventID),
		zap.String("tenant_id", event.TenantID),
		zap.String("legal_entity_id", event.LegalEntityID),
		zap.String("correlation_id", event.CorrelationID),
	)

	// TODO: Invalidate decision cache for this tenant/legal_entity
	return nil
}

// Ensure Handler implements EventHandler.
var _ EventHandler = (*Handler)(nil)