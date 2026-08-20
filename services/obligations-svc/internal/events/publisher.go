// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.Obligation carries real
// LegalEntityID and JurisdictionID but no tenant_id field at all (never
// fabricated, correctly omitted) and no actor field beyond
// CreatedByPrincipalID — status-transition events (updated/overdue/
// closed) thread actor_id through explicitly from the handler's
// already-verified principalID.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
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

// Publisher implements event publishing against the Kafka event backbone.
//
// Events are facts, not commands. Published topics are append-only. Same
// posture as identity-context-svc, tenant-entity-registry-svc, and
// policy-svc's producers.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

// NewPublisher constructs a Publisher bound to the given topic and Kafka writer.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishObligationCreated publishes obligation.created for a newly-created
// obligation. Callers must only invoke this on the first insert
// (created=true) — a replayed idempotent POST must not re-emit the event.
func (p *Publisher) PublishObligationCreated(ctx context.Context, o domain.Obligation, correlationID string) error {
	return p.emit(ctx, "obligation.created", correlationID, o.LegalEntityID, o.JurisdictionID, o.CreatedByPrincipalID, o.ObligationID, map[string]any{
		"obligation_id":           o.ObligationID,
		"legal_entity_id":         o.LegalEntityID,
		"jurisdiction_id":         o.JurisdictionID,
		"obligation_code":         o.ObligationCode,
		"obligation_type":         o.ObligationType,
		"due_date":                o.DueDate,
		"severity_level":          o.SeverityLevel,
		"source_reference":        o.SourceReference,
		"created_by_principal_id": o.CreatedByPrincipalID,
		"created_at":              o.CreatedAt,
	})
}

// PublishObligationUpdated publishes obligation.updated for any status
// transition. Callers must only invoke this when transitioned=true (see
// Store.UpdateObligationStatus) — an idempotent no-op must not re-emit.
func (p *Publisher) PublishObligationUpdated(ctx context.Context, o domain.Obligation, actorID, correlationID string) error {
	return p.emit(ctx, "obligation.updated", correlationID, o.LegalEntityID, o.JurisdictionID, actorID, o.ObligationID, map[string]any{
		"obligation_id":     o.ObligationID,
		"obligation_status": o.ObligationStatus,
		"updated_at":        o.UpdatedAt,
	})
}

// PublishObligationOverdue publishes obligation.overdue. Callers must only
// invoke this on an actual transition into OVERDUE.
func (p *Publisher) PublishObligationOverdue(ctx context.Context, o domain.Obligation, actorID, correlationID string) error {
	return p.emit(ctx, "obligation.overdue", correlationID, o.LegalEntityID, o.JurisdictionID, actorID, o.ObligationID, map[string]any{
		"obligation_id": o.ObligationID,
		"due_date":      o.DueDate,
	})
}

// PublishObligationClosed publishes obligation.closed. Callers must only
// invoke this on an actual transition into CLOSED.
func (p *Publisher) PublishObligationClosed(ctx context.Context, o domain.Obligation, actorID, correlationID string) error {
	return p.emit(ctx, "obligation.closed", correlationID, o.LegalEntityID, o.JurisdictionID, actorID, o.ObligationID, map[string]any{
		"obligation_id": o.ObligationID,
		"closed_at":     o.ClosedAt,
	})
}

// emit serialises the payload into the canonical envelope and writes it to
// the Kafka topic set on the Writer (main.go) — not set here, since kafka-go
// rejects a Message that also specifies Topic when the Writer already has one.
func (p *Publisher) emit(ctx context.Context, eventType, correlationID, legalEntityID, jurisdiction, actorID, key string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "obligations-svc",
		LegalEntityID: legalEntityID,
		Jurisdiction:  jurisdiction,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	msg := kafka.Message{Key: []byte(key), Value: data}
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("event %q: kafka write: %w", eventType, err)
	}

	p.log.Info("event published",
		zap.String("event_type", eventType),
		zap.String("topic", p.topic),
		zap.String("correlation_id", correlationID),
	)
	return nil
}
