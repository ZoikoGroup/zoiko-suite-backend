// Package consumer handles incoming domain events and persists them to the
// Audit Event Store.
//
// Architectural constraints (doctrine.md, 03-microservices.md §3.7):
//
//   - Evidence services are read-only consumers of every other domain's events.
//     They NEVER mutate the source truth of any other service.
//   - Every consumer handler is idempotent: the store layer guarantees that
//     two deliveries of the same event_id produce exactly one stored row.
//   - Malformed events (missing required fields) are rejected and logged.
//     They are NOT stored partially and they do NOT crash the consumer loop.
//     Failure mode: fail-safe / degrade per event, not per consumer.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/store"
)

// envelope is the canonical event wrapper shared by all ZoikoSuite services
// (see identity-context-svc/internal/events/publisher.go).
// Every event arriving on the Kafka topic is expected to conform to this shape.
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     string          `json:"emitted_at"` // RFC3339 string from producer
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	Payload       json.RawMessage `json:"payload"`
}

// contextResolvedPayload is the canonical payload for the
// identity.context.resolved event published by identity-context-svc.
// All fields are required — absence of any field means the event is malformed
// and MUST be rejected.
//
// Shape (from identity-context-svc/internal/events/publisher.go):
//
//	{
//	  "principal_id":       "...",
//	  "tenant_id":          "...",
//	  "legal_entity_id":    "...",
//	  "session_context_id": "...",
//	  "correlation_id":     "..."
//	}
type contextResolvedPayload struct {
	PrincipalID      string `json:"principal_id"`
	TenantID         string `json:"tenant_id"`
	LegalEntityID    string `json:"legal_entity_id"`
	SessionContextID string `json:"session_context_id"`
	CorrelationID    string `json:"correlation_id"`
}

// entityStatusChangedPayload is the payload for the entity.status.changed
// event published by tenant-entity-registry-svc.
//
// Shape (from tenant-entity-registry-svc/internal/events/publisher.go):
//
//	{
//	  "tenant_id":       "...",
//	  "legal_entity_id": "...",
//	  "previous_status": "...",
//	  "new_status":      "..."
//	}
type entityStatusChangedPayload struct {
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	// previous_status and new_status are preserved in the JSONB payload;
	// they do not need to be promoted to top-level columns.
}

// Consumer receives raw event messages (bytes), validates them, and delegates
// to the Store for append-only persistence.
type Consumer struct {
	store store.Store
	log   *zap.Logger
}

// New returns a Consumer wired to the given Store and logger.
func New(s store.Store, log *zap.Logger) *Consumer {
	return &Consumer{store: s, log: log}
}

// Handle dispatches a raw message to the appropriate handler based on event_type.
//
// Contract:
//   - If the message cannot be parsed or the event_type is unknown, the message
//     is rejected (logged at error level) and nil is returned.  The consumer
//     loop must NOT requeue unrecognised events — they would loop forever.
//   - If a required field is missing the message is rejected without partial storage.
//   - Duplicate event_ids (same eventID delivered twice) are handled atomically
//     by the store layer (INSERT … ON CONFLICT DO NOTHING).
//
// eventID is the globally unique event identifier supplied by the message broker.
// In the Kafka integration it will come from a dedicated header or a top-level
// envelope field; during stub / test mode callers pass it explicitly.
func (c *Consumer) Handle(ctx context.Context, eventID string, raw []byte) error {
	if eventID == "" {
		c.log.Error("rejected: event_id is empty",
			zap.ByteString("raw_message", raw),
		)
		return nil // non-fatal: do not crash the consumer loop
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Error("rejected: cannot unmarshal event envelope",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return nil
	}

	if env.EventType == "" {
		c.log.Error("rejected: event_type is missing",
			zap.String("event_id", eventID),
		)
		return nil
	}
	if env.SourceService == "" {
		c.log.Error("rejected: source_service is missing",
			zap.String("event_id", eventID),
			zap.String("event_type", env.EventType),
		)
		return nil
	}
	if env.SchemaVersion == "" {
		c.log.Error("rejected: schema_version is missing",
			zap.String("event_id", eventID),
			zap.String("event_type", env.EventType),
		)
		return nil
	}

	switch env.EventType {
	case "identity.context.resolved":
		return c.handleContextResolved(ctx, eventID, env)
	case "entity.status.changed":
		return c.handleEntityStatusChanged(ctx, eventID, env)
	default:
		c.log.Warn("unknown event_type — skipped",
			zap.String("event_id", eventID),
			zap.String("event_type", env.EventType),
		)
		return nil
	}
}

// handleContextResolved processes the identity.context.resolved event.
//
// Required payload fields: principal_id, tenant_id, legal_entity_id,
// session_context_id, correlation_id.  Any missing field causes rejection.
func (c *Consumer) handleContextResolved(ctx context.Context, eventID string, env envelope) error {
	var p contextResolvedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		c.log.Error("rejected: cannot unmarshal identity.context.resolved payload",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return nil
	}

	if err := validateContextResolved(p); err != nil {
		c.log.Error("rejected: identity.context.resolved payload validation failed",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return nil
	}

	evt := store.AuditEvent{
		EventID:       eventID,
		EventType:     env.EventType,
		TenantID:      p.TenantID,
		LegalEntityID: p.LegalEntityID,
		PrincipalID:   p.PrincipalID,
		SourceService: env.SourceService,
		SchemaVersion: env.SchemaVersion,
		Payload:       env.Payload,
	}

	if err := c.store.Store(ctx, evt); err != nil {
		// Store errors are returned so the caller can decide on retry/DLQ.
		return fmt.Errorf("handleContextResolved: store: %w", err)
	}

	c.log.Info("identity.context.resolved stored",
		zap.String("event_id", eventID),
		zap.String("tenant_id", p.TenantID),
		zap.String("principal_id", p.PrincipalID),
		zap.String("correlation_id", p.CorrelationID),
	)
	return nil
}

// handleEntityStatusChanged processes the entity.status.changed event.
//
// Required payload fields: tenant_id, legal_entity_id.
func (c *Consumer) handleEntityStatusChanged(ctx context.Context, eventID string, env envelope) error {
	var p entityStatusChangedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		c.log.Error("rejected: cannot unmarshal entity.status.changed payload",
			zap.String("event_id", eventID),
			zap.Error(err),
		)
		return nil
	}

	if p.TenantID == "" {
		c.log.Error("rejected: entity.status.changed missing tenant_id",
			zap.String("event_id", eventID),
		)
		return nil
	}
	if p.LegalEntityID == "" {
		c.log.Error("rejected: entity.status.changed missing legal_entity_id",
			zap.String("event_id", eventID),
		)
		return nil
	}

	evt := store.AuditEvent{
		EventID:       eventID,
		EventType:     env.EventType,
		TenantID:      p.TenantID,
		LegalEntityID: p.LegalEntityID,
		SourceService: env.SourceService,
		SchemaVersion: env.SchemaVersion,
		Payload:       env.Payload,
	}

	if err := c.store.Store(ctx, evt); err != nil {
		return fmt.Errorf("handleEntityStatusChanged: store: %w", err)
	}

	c.log.Info("entity.status.changed stored",
		zap.String("event_id", eventID),
		zap.String("tenant_id", p.TenantID),
		zap.String("legal_entity_id", p.LegalEntityID),
	)
	return nil
}

// validateContextResolved enforces that all required fields of the
// identity.context.resolved payload are non-empty.
func validateContextResolved(p contextResolvedPayload) error {
	if p.PrincipalID == "" {
		return fmt.Errorf("principal_id is required")
	}
	if p.TenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	if p.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if p.SessionContextID == "" {
		return fmt.Errorf("session_context_id is required")
	}
	if p.CorrelationID == "" {
		return fmt.Errorf("correlation_id is required")
	}
	return nil
}
