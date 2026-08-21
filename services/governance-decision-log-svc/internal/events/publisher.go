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

	"zoiko.io/governance-decision-log-svc/internal/domain"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.GovernanceDecision carries
// real TenantID/LegalEntityID/ActorID; it has no jurisdiction field of its
// own — RuleBasis is used inside the payload (not the envelope) as the
// closest available jurisdiction-adjacent reference, per this service's
// existing documented convention.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
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
// Evidence obligation: after every successful (first-time) write, this
// service publishes governance.decision.recorded as a fact, not a command.
// Publishing is stubbed (logged, not written to Kafka) until a kafka.Writer
// is injected — see CONTEXT.md.
type Publisher struct {
	log   *zap.Logger
	topic string

	// producer is nil only in local development with no broker configured,
	// in which case emit drops the event and says so. main.go refuses to
	// start with a nil producer in production or staging.
	producer MessageWriter
}

// NewPublisher constructs a Publisher bound to the given topic and writer.
// A nil producer makes every publish a logged no-op — see the struct comment.
func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer — storing that directly into the
	// MessageWriter interface field would make the interface itself
	// non-nil, defeating the p.producer == nil check in emit().
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishDecisionRecorded publishes governance.decision.recorded for a
// newly-inserted decision. Callers must only invoke this on the first
// insert of a decision_id — a replayed idempotent POST must not re-emit
// the event.
//
// Payload includes tenant_id, legal_entity_id, actor_id, and jurisdiction
// context (populated from rule_basis, the closest thing this schema has to
// a jurisdiction reference) per CONTEXT.md's finalized payload shape, plus
// the remaining decision fields so consumers don't need a follow-up read.
func (p *Publisher) PublishDecisionRecorded(ctx context.Context, d domain.GovernanceDecision) error {
	return p.emit(ctx, "governance.decision.recorded", d.CorrelationID, d.TenantID, d.LegalEntityID, d.ActorID, map[string]any{
		"decision_id":          d.DecisionID,
		"tenant_id":            d.TenantID,
		"legal_entity_id":      d.LegalEntityID,
		"actor_id":             d.ActorID,
		"action_type":          d.ActionType,
		"outcome":              d.Outcome,
		"rule_basis":           d.RuleBasis,
		"jurisdiction_context": d.RuleBasis,
		"correlation_id":       d.CorrelationID,
		"decided_at":           d.DecidedAt,
	})
}

// emit serialises the payload into the canonical envelope and writes to
// Kafka. Stub: logs structured JSON until kafka.Writer is injected.
func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, legalEntityID, actorID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "governance-decision-log-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	if p.producer == nil {
		p.log.Debug("event dropped — no Kafka brokers configured",
			zap.String("event_type", eventType),
			zap.String("correlation_id", correlationID),
		)
		return nil
	}

	// Topic is set on the Writer, not the Message — kafka-go rejects a
	// Message carrying a Topic when the Writer already has one.
	msg := kafka.Message{Key: []byte(correlationID), Value: data}
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
