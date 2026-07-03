// Package events contains the domain event publisher and consumer.
package events

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// envelope is the standard event wrapper for all events published to zoiko.entity.events.
// Every payload includes the mandatory fields per doctrine §09 event design principles:
// source_service, schema_version, emitted_at, correlation_id.
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
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
	log   *zap.Logger
	topic string
	// writer *kafka.Writer  — TODO: inject kafka.Writer before Phase 1 exit criteria
}

func NewPublisher(log *zap.Logger, topic string) *Publisher {
	return &Publisher{log: log, topic: topic}
}

func (p *Publisher) PublishTenantCreated(ctx context.Context, tenant *domain.Tenant, correlationID string) {
	p.emit("tenant.created", correlationID, map[string]any{
		"tenant_id":       tenant.TenantID,
		"tenant_code":     tenant.TenantCode,
		"legal_name":      tenant.LegalName,
		"lifecycle_state": tenant.LifecycleState,
	})
}

func (p *Publisher) PublishEntityCreated(ctx context.Context, entity *domain.LegalEntity, correlationID string) {
	p.emit("entity.created", correlationID, map[string]any{
		"tenant_id":               entity.TenantID,
		"legal_entity_id":         entity.LegalEntityID,
		"entity_code":             entity.EntityCode,
		"entity_type":             entity.EntityType,
		"entity_status":           entity.EntityStatus,
		"primary_jurisdiction_id": entity.PrimaryJurisdictionID,
		"data_residency_policy_id": entity.DataResidencyPolicyID,
	})
}

func (p *Publisher) PublishEntityUpdated(ctx context.Context, entity *domain.LegalEntity, correlationID string) {
	p.emit("entity.updated", correlationID, map[string]any{
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
	tenantID, legalEntityID string,
	previousStatus, newStatus domain.EntityStatus,
	correlationID string,
) {
	p.emit("entity.status.changed", correlationID, map[string]any{
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
	p.emit("entity.hierarchy.changed", correlationID, map[string]any{
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
	p.emit("entity.jurisdiction.changed", correlationID, map[string]any{
		"legal_entity_id":  assignment.LegalEntityID,
		"assignment_id":    assignment.AssignmentID,
		"jurisdiction_id":  assignment.JurisdictionID,
		"assignment_type":  assignment.AssignmentType,
		"change_type":      changeType, // "ASSIGNED" | "END_DATED"
		"effective_from":   assignment.EffectiveFrom,
		"effective_to":     assignment.EffectiveTo,
	})
}

// emit serializes the payload into the canonical envelope and writes to Kafka.
// Stub: logs structured JSON until kafka.Writer is injected.
func (p *Publisher) emit(eventType, correlationID string, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "tenant-entity-registry-svc",
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, _ := json.Marshal(env)

	// TODO: publish to Kafka topic
	// msg := kafka.Message{Topic: p.topic, Value: data}
	// if err := p.writer.WriteMessages(ctx, msg); err != nil { ... outbox retry ... }

	p.log.Info("event emitted (stub — wire Kafka writer)",
		zap.String("event_type", eventType),
		zap.String("correlation_id", correlationID),
		zap.ByteString("payload", data),
	)
}
