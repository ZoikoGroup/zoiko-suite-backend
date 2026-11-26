// Package events contains the domain event publisher for this service.
package events

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.TaxReturn carries real
// LegalEntityID and JurisdictionID fields.
type Event struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	ReturnID      string      `json:"return_id"`
	TenantID      string      `json:"tenant_id"`
	LegalEntityID string      `json:"legal_entity_id,omitempty"`
	Jurisdiction  string      `json:"jurisdiction,omitempty"`
	ActorID       string      `json:"actor_id,omitempty"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	OccurredAt    time.Time   `json:"occurred_at"`
	Payload       interface{} `json:"payload"`
}

// PublishParams carries the envelope-level fields a call site supplies,
// alongside the payload-level business object.
type PublishParams struct {
	EventType     string
	ReturnID      string
	TenantID      string
	LegalEntityID string
	Jurisdiction  string
	ActorID       string
	CorrelationID string
	Payload       interface{}
}

// MessageWriter is the one method KafkaPublisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// KafkaPublisher is the Kafka-backed implementation.
type KafkaPublisher struct {
	writer MessageWriter
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}
	return &KafkaPublisher{writer: w, logger: logger}
}

// NewKafkaPublisherWithWriter is NewKafkaPublisher but with a
// caller-supplied MessageWriter — used by tests to substitute a fake.
func NewKafkaPublisherWithWriter(writer MessageWriter, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, logger: logger}
}

// NewEvent builds the envelope for an event. It is called inside the business
// transaction and the result is written to the outbox as-is, so the delivered
// message carries the actor and correlation of the request that caused the
// change. The shape is unchanged from the former direct publish.
func NewEvent(params PublishParams) Event {
	return Event{
		// A fresh UUID per event, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "corporate-tax-svc",
		ReturnID:      params.ReturnID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		Jurisdiction:  params.Jurisdiction,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}
}

// PublishOutbox delivers one outbox row (an already-built envelope) to the
// events topic, keyed by the aggregate as before. The stable outbox event id
// travels as the X-Event-ID header across every retry so consumers can
// de-duplicate at-least-once delivery. A failure is RETURNED so the relay
// keeps the row and retries; it is never swallowed.
func (p *KafkaPublisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	err := p.writer.WriteMessages(ctx, kafka.Message{
		Key:     []byte(aggregateID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(outboxEventID)}},
	})
	if err != nil {
		p.logger.Warn("outbox kafka write failed", zap.String("outbox_event_id", outboxEventID), zap.Error(err))
		return fmt.Errorf("kafka write: %w", err)
	}
	return nil
}
