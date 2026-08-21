// Package events contains the domain event publisher and consumer.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.Tenant is a top-level
// object with no legal_entity_id of its own — correctly omitted for
// tenant.created. Every other domain struct here (LegalEntity, Workspace,
// EntityHierarchy, EntityJurisdictionAssignment) already carries real
// Created/UpdatedByPrincipalID actor fields.
// LegalEntity's PrimaryJurisdictionID and EntityJurisdictionAssignment's
// JurisdictionID are genuine jurisdiction context, surfaced at the
// envelope level. entity.status.changed has no domain object at its call
// site — actor_id is threaded through explicitly from the caller's
// already-resolved principal.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	Jurisdiction  string          `json:"jurisdiction,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher implements EventPublisher against the Kafka event backbone.
//
// Published events (zoiko.entity.events topic):
//   - tenant.created
//   - entity.created
//   - entity.updated
//   - entity.hierarchy.changed
//   - entity.jurisdiction.changed
//   - entity.status.changed  ← Q4 addition: enables identity-context-svc to cache status locally
//
// Evidence obligation: all publishes are non-blocking from the service's perspective
// (callers should invoke in goroutines or outbox). The DB write is NOT rolled back
// on Kafka publish failure — the outbox pattern retries delivery.
type Publisher struct {
	log    *zap.Logger
	topic  string
	writer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, writer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, writer: writer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, writer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, writer: writer}
}

func (p *Publisher) PublishTenantCreated(ctx context.Context, tenant *domain.Tenant, correlationID string) {
	p.emit(ctx, "tenant.created", correlationID, tenant.TenantID, "", "", tenant.CreatedByPrincipalID, tenant.TenantID, map[string]any{
		"tenant_id":       tenant.TenantID,
		"tenant_code":     tenant.TenantCode,
		"legal_name":      tenant.LegalName,
		"lifecycle_state": tenant.LifecycleState,
	})
}

func (p *Publisher) PublishEntityCreated(ctx context.Context, entity *domain.LegalEntity, correlationID string) {
	p.emit(ctx, "entity.created", correlationID, entity.TenantID, entity.LegalEntityID, entity.PrimaryJurisdictionID, entity.CreatedByPrincipalID, entity.LegalEntityID, map[string]any{
		"tenant_id":                entity.TenantID,
		"legal_entity_id":          entity.LegalEntityID,
		"entity_code":              entity.EntityCode,
		"entity_type":              entity.EntityType,
		"entity_status":            entity.EntityStatus,
		"primary_jurisdiction_id":  entity.PrimaryJurisdictionID,
		"data_residency_policy_id": entity.DataResidencyPolicyID,
	})
}

func (p *Publisher) PublishWorkspaceCreated(ctx context.Context, workspace *domain.Workspace, correlationID string) {
	legalEntityID := ""
	if workspace.LegalEntityID != nil {
		legalEntityID = *workspace.LegalEntityID
	}
	p.emit(ctx, "workspace.created", correlationID, workspace.TenantID, legalEntityID, "", workspace.CreatedByPrincipalID, workspace.WorkspaceID, map[string]any{
		"tenant_id":              workspace.TenantID,
		"workspace_id":           workspace.WorkspaceID,
		"legal_entity_id":        workspace.LegalEntityID,
		"billing_classification": workspace.BillingClassification,
		"billing_source":         workspace.BillingSource,
	})
}

func (p *Publisher) PublishEntityUpdated(ctx context.Context, entity *domain.LegalEntity, correlationID string) {
	p.emit(ctx, "entity.updated", correlationID, entity.TenantID, entity.LegalEntityID, entity.PrimaryJurisdictionID, entity.UpdatedByPrincipalID, entity.LegalEntityID, map[string]any{
		"tenant_id":       entity.TenantID,
		"legal_entity_id": entity.LegalEntityID,
	})
}

// PublishEntityStatusChanged publishes entity.status.changed on every entity_status transition.
//
// Q4 resolution: identity-context-svc subscribes to this event and caches entity status locally.
// This removes entity status resolution from the steady-state hot-path — the live probe endpoint
// GET /v1/entities/{entityID}/status is only called on cache miss or cold start.
//
// Payload includes previous_status to allow consumers to update their cached state correctly.
func (p *Publisher) PublishEntityStatusChanged(
	ctx context.Context,
	tenantID, legalEntityID, actorID string,
	previousStatus, newStatus domain.EntityStatus,
	correlationID string,
) {
	p.emit(ctx, "entity.status.changed", correlationID, tenantID, legalEntityID, "", actorID, legalEntityID, map[string]any{
		"tenant_id":       tenantID,
		"legal_entity_id": legalEntityID,
		"previous_status": previousStatus,
		"new_status":      newStatus,
	})
}

func (p *Publisher) PublishEntityHierarchyChanged(
	ctx context.Context,
	hierarchy *domain.EntityHierarchy,
	changeType string,
	correlationID string,
) {
	p.emit(ctx, "entity.hierarchy.changed", correlationID, hierarchy.TenantID, hierarchy.ChildLegalEntityID, "", hierarchy.UpdatedByPrincipalID, hierarchy.HierarchyID, map[string]any{
		"tenant_id":              hierarchy.TenantID,
		"hierarchy_id":           hierarchy.HierarchyID,
		"parent_legal_entity_id": hierarchy.ParentLegalEntityID,
		"child_legal_entity_id":  hierarchy.ChildLegalEntityID,
		"relationship_type":      hierarchy.RelationshipType,
		"change_type":            changeType, // "CREATED" | "END_DATED"
		"effective_from":         hierarchy.EffectiveFrom,
		"effective_to":           hierarchy.EffectiveTo,
	})
}

func (p *Publisher) PublishEntityJurisdictionChanged(
	ctx context.Context,
	assignment *domain.EntityJurisdictionAssignment,
	changeType string,
	correlationID string,
) {
	p.emit(ctx, "entity.jurisdiction.changed", correlationID, assignment.TenantID, assignment.LegalEntityID, assignment.JurisdictionID, assignment.UpdatedByPrincipalID, assignment.AssignmentID, map[string]any{
		"legal_entity_id": assignment.LegalEntityID,
		"assignment_id":   assignment.AssignmentID,
		"jurisdiction_id": assignment.JurisdictionID,
		"assignment_type": assignment.AssignmentType,
		"change_type":     changeType, // "ASSIGNED" | "END_DATED"
		"effective_from":  assignment.EffectiveFrom,
		"effective_to":    assignment.EffectiveTo,
	})
}

// emit serializes the payload into the canonical envelope and writes to Kafka.
//
// Signature note: emit() and every PublishX method above are void by design
// (existing contract, unchanged here) — publish failures are logged, not
// propagated to callers. Widening this to return error would ripple into
// registry.Service's call sites and tests; that's a separate, larger change
// than "wire the producer," so it's left as-is.
func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, jurisdiction, actorID, key string, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "tenant-entity-registry-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		Jurisdiction:  jurisdiction,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, _ := json.Marshal(env)

	// Topic is set on the Writer itself (main.go), not here — kafka-go
	// rejects a Message that also specifies Topic when the Writer already has one.
	msg := kafka.Message{Key: []byte(key), Value: data}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.log.Error("event publish failed",
			zap.String("event_type", eventType),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		return
	}

	p.log.Info("event published",
		zap.String("event_type", eventType),
		zap.String("topic", p.topic),
		zap.String("correlation_id", correlationID),
	)
}
