// Package events builds this service's Kafka event envelopes and writes
// already-built messages to Kafka.
//
// Events are never published from the request path. The service layer calls
// NewRecommendationEvent to build the message, the store writes it to the
// outbox table in the same transaction as the state change, and the outbox
// relay (internal/outbox) hands it to Publisher.WriteRaw later. A crash
// between commit and publish therefore delays an event; it never loses one,
// and an event is never published for a change that rolled back.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"

	"zoiko.io/decision-support-svc/internal/domain"
)

// Event types, one per lifecycle step.
const (
	EventRecommendationProposed   = "decision_support.recommendation.proposed"
	EventRecommendationAccepted   = "decision_support.recommendation.accepted"
	EventRecommendationRejected   = "decision_support.recommendation.rejected"
	EventRecommendationExpired    = "decision_support.recommendation.expired"
	EventRecommendationSuperseded = "decision_support.recommendation.superseded"
)

// EventTypeForStatus names the event announcing a move into status.
func EventTypeForStatus(status domain.Status) string {
	switch status {
	case domain.StatusAccepted:
		return EventRecommendationAccepted
	case domain.StatusRejected:
		return EventRecommendationRejected
	case domain.StatusExpired:
		return EventRecommendationExpired
	case domain.StatusSuperseded:
		return EventRecommendationSuperseded
	default:
		return EventRecommendationProposed
	}
}

// Envelope is this platform's event contract (Doc 03 §19): event name,
// version, timestamp, tenant, legal entity, actor, correlation, source
// service and payload schema version.
type Envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// NewRecommendationEvent builds the outbox message announcing that rec is
// now in rec.Status, caused by actorID. The payload is the full
// recommendation snapshot, so consumers never need to call back.
func NewRecommendationEvent(eventType string, rec domain.Recommendation, actorID string) (domain.OutboxMessage, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return domain.OutboxMessage{}, fmt.Errorf("marshal event payload: %w", err)
	}

	envelope := Envelope{
		// A fresh UUID per event — see docs/architecture/known-gaps.md's
		// event_id collision writeup.
		EventID:       "evt-" + uuid.NewString(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "decision-support-svc",
		TenantID:      rec.TenantID,
		LegalEntityID: rec.LegalEntityID,
		ActorID:       actorID,
		CorrelationID: rec.CorrelationID,
		Payload:       payload,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return domain.OutboxMessage{}, fmt.Errorf("marshal event envelope: %w", err)
	}

	return domain.OutboxMessage{
		OutboxID:    uuid.NewString(),
		TenantID:    rec.TenantID,
		Destination: domain.OutboxDestinationKafka,
		MessageType: eventType,
		MessageKey:  rec.RecommendationID,
		Payload:     body,
	}, nil
}

// MessageWriter is the one method Publisher needs from *kafka.Writer,
// narrowed so tests can substitute a fake.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher writes pre-built messages to the configured topic.
type Publisher struct {
	producer MessageWriter
}

// NewPublisher wraps a Kafka writer. A nil writer is "dry run": WriteRaw
// succeeds without sending, so local runs without a broker still drain the
// outbox.
func NewPublisher(producer MessageWriter) *Publisher {
	return &Publisher{producer: producer}
}

// WriteRaw sends one message with the given key and value.
func (p *Publisher) WriteRaw(ctx context.Context, key string, value []byte) error {
	if p.producer == nil {
		return nil
	}
	if err := p.producer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: value}); err != nil {
		return fmt.Errorf("kafka write: %w", err)
	}
	return nil
}
