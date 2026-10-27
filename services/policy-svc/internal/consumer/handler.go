// Package consumer provides the event handler implementation for
// policy-svc's Kafka consumer. It handles entity.created, role.updated and
// authority.delegated (Doc 03 §8.1 Consumed Events) by dropping the
// affected tenant's cached resolution candidates.
//
// None of these events changes a policy version, and the resolution cache is
// already kept exact by the database watermark, so this is a cheap refresh
// rather than a correctness mechanism. Handling is idempotent: a duplicate
// delivery (GCP domain negative path #29) clears an already-cleared entry.
package consumer

import (
	"context"

	"go.uber.org/zap"
)

// Invalidator drops a tenant's cached resolution state.
type Invalidator interface {
	InvalidateTenant(tenantID string)
}

// Handler implements EventHandler for policy-svc's consumer.
type Handler struct {
	cache Invalidator
	log   *zap.Logger
}

// NewHandler constructs a new consumer event handler.
func NewHandler(cache Invalidator, log *zap.Logger) *Handler {
	return &Handler{cache: cache, log: log}
}

func (h *Handler) invalidate(eventType, eventID, tenantID string) {
	if tenantID != "" {
		h.cache.InvalidateTenant(tenantID)
	}
	h.log.Debug("consumer: tenant resolution cache refreshed",
		zap.String("event_type", eventType),
		zap.String("event_id", eventID),
		zap.String("tenant_id", tenantID),
	)
}

// HandleEntityCreated handles entity.created events.
func (h *Handler) HandleEntityCreated(_ context.Context, event EntityCreatedEvent) error {
	h.invalidate("entity.created", event.EventID, event.TenantID)
	return nil
}

// HandleRoleUpdated handles role.updated events.
func (h *Handler) HandleRoleUpdated(_ context.Context, event RoleUpdatedEvent) error {
	h.invalidate("role.updated", event.EventID, event.TenantID)
	return nil
}

// HandleAuthorityDelegated handles authority.delegated events.
func (h *Handler) HandleAuthorityDelegated(_ context.Context, event AuthorityDelegatedEvent) error {
	h.invalidate("authority.delegated", event.EventID, event.TenantID)
	return nil
}

// Ensure Handler implements EventHandler.
var _ EventHandler = (*Handler)(nil)
