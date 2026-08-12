// Package events contains the domain event publisher for this service.
//
// Only the two events docs/architecture/03-microservices.md §8.6 names are
// published: evidence.requirement.missing and
// evidence.requirement.satisfied. Nothing else is invented alongside them —
// no evidence.requirement.created, no retirement event — because the spec
// does not name them and events are part of a service's public contract.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

type envelope struct {
	EventType     string          `json:"event_type"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
//
// Narrowed to an interface purely so publisher_test.go can assert which
// outcomes actually reach the broker and which do not — in particular that
// NO_REQUIREMENTS_DEFINED emits nothing. *kafka.Writer satisfies it, so
// cmd/server passes its writer unchanged and this stays identical in shape to
// every other producer on the platform.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher implements event publishing against the Kafka event backbone.
// Publish failures are logged, never returned/propagated — same posture as
// every other producer in this platform (an outbox pattern handles
// redelivery; DB writes are never rolled back on a publish failure).
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishEvaluation emits the §8.6 event matching the evaluation's outcome.
//
// NO_REQUIREMENTS_DEFINED emits nothing: neither §8.6 event is true of it
// (nothing was satisfied, nothing was missing), and minting a third event
// would extend this service's published contract beyond the spec. It is
// logged instead, at warn level, because a permanently unconfigured gate is
// an operational problem worth seeing.
func (p *Publisher) PublishEvaluation(ctx context.Context, e domain.EvidenceEvaluation) {
	payload := map[string]any{
		"evaluation_id":   e.EvaluationID,
		"tenant_id":       e.TenantID,
		"legal_entity_id": e.LegalEntityID,
		"domain_code":     e.DomainCode,
		"action_type":     e.ActionType,
		"outcome":         string(e.Outcome),
	}

	switch e.Outcome {
	case domain.OutcomeSatisfied:
		p.emit(ctx, "evidence.requirement.satisfied", e.CorrelationID, e.EvaluationID, payload)
	case domain.OutcomeMissing:
		// The unmet detail rides along: a consumer reacting to a blocked
		// finalization needs to know WHAT is missing, not just that
		// something is.
		payload["unmet"] = json.RawMessage(e.UnmetPayload)
		p.emit(ctx, "evidence.requirement.missing", e.CorrelationID, e.EvaluationID, payload)
	default:
		p.log.Warn("evidence evaluation had no requirements defined — no event published",
			zap.String("evaluation_id", e.EvaluationID),
			zap.String("domain_code", e.DomainCode),
			zap.String("action_type", e.ActionType),
		)
	}
}

func (p *Publisher) emit(ctx context.Context, eventType, correlationID, key string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event payload", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	env := envelope{
		EventType:     eventType,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "evidence-requirements-svc",
		CorrelationID: correlationID,
		Payload:       raw,
	}
	body, err := json.Marshal(env)
	if err != nil {
		p.log.Error("failed to marshal event envelope", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: body}); err != nil {
		p.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("topic", p.topic),
			zap.Error(fmt.Errorf("kafka write: %w", err)),
		)
	}
}
