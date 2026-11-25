// Package events defines this service's event envelope and delivers outbox
// rows to Kafka. Events are written to the transactional outbox by the store
// in the same transaction as the change (see internal/outbox); nothing is
// published from the request path.
package events

import (
	"context"
	"fmt"
	"time"

	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version.
//
// Both domain.Clause and domain.ContractTemplate carry real LegalEntityID
// and JurisdictionID fields — this is one of the few services where
// jurisdiction context is actually populated, not correctly-omitted for
// lack of a source.
type Event struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	EntityID      string      `json:"entity_id"`
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
	EntityID      string
	TenantID      string
	LegalEntityID string
	Jurisdiction  string
	ActorID       string
	CorrelationID string
	Payload       interface{}
}

// NewEvent builds the envelope for one event. outboxEventID is the outbox row
// id; the event_id keeps its "evt-<uuid>" shape and is now stable across
// delivery retries.
func NewEvent(outboxEventID string, params PublishParams) Event {
	return Event{
		EventID:       "evt-" + outboxEventID,
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "clause-template-svc",
		EntityID:      params.EntityID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		Jurisdiction:  params.Jurisdiction,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}
}

// MessageWriter is the one method KafkaPublisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type KafkaPublisher struct {
	writer MessageWriter
	topic  string
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}
	return &KafkaPublisher{writer: w, topic: topic, logger: logger}
}

// NewKafkaPublisherWithWriter is NewKafkaPublisher but with a
// caller-supplied MessageWriter — used by tests to substitute a fake.
func NewKafkaPublisherWithWriter(writer MessageWriter, topic string, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, topic: topic, logger: logger}
}

// PublishOutbox publishes one already-built envelope, keyed by the entity id
// as before. The stable outbox event id travels as the X-Event-ID header
// across every retry so consumers can de-duplicate at-least-once delivery. A
// failure is RETURNED (the outbox relay keeps the row and retries) — the old
// Publish logged it and returned nil, dropping the event.
func (p *KafkaPublisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	err := p.writer.WriteMessages(ctx, kafka.Message{
		Key:     []byte(aggregateID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(outboxEventID)}},
	})
	if err != nil {
		p.logger.Warn("outbox kafka write failed",
			zap.String("outbox_event_id", outboxEventID), zap.String("topic", p.topic), zap.Error(err))
		return fmt.Errorf("kafka write: %w", err)
	}
	return nil
}
