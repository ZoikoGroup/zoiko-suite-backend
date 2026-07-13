// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
)

// envelope is the standard event wrapper for all events published by this
// service. Mirrors identity-context-svc's and tenant-entity-registry-svc's
// envelope shape exactly — see CONTEXT.md "Event publishing — FINALIZED".
type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
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
	// producer *kafka.Writer  — TODO: inject kafka.Writer before Phase 1 exit criteria
}

// NewPublisher constructs a Publisher bound to the given topic.
func NewPublisher(log *zap.Logger, topic string) *Publisher {
	return &Publisher{log: log, topic: topic}
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
	return p.emit("governance.decision.recorded", d.CorrelationID, map[string]any{
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
func (p *Publisher) emit(eventType, correlationID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "governance-decision-log-svc",
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	// TODO: publish to Kafka topic
	// msg := kafka.Message{Topic: p.topic, Value: data}
	// if err := p.producer.WriteMessages(ctx, msg); err != nil { ... outbox retry ... }

	p.log.Info("event emitted (stub — wire Kafka writer)",
		zap.String("event_type", eventType),
		zap.String("correlation_id", correlationID),
		zap.ByteString("payload", data),
	)
	return nil
}
