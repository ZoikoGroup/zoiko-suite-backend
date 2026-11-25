// Package events defines this service's event envelope and delivers outbox
// rows to Kafka.
//
// Events are NOT published from the request path any more. The store builds
// the envelope (NewEvent) and writes it to the transactional outbox
// (internal/outbox) in the same transaction as the state change; the outbox
// Relay calls PublishOutbox to deliver it, retrying on failure. The previous
// publish after commit ignored its error, so a broker outage silently lost
// the only notice of a committed change.
package events

import (
	"context"
	"fmt"
	"time"

	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// SourceService is the source_service stamped on every event.
const SourceService = "contract-lifecycle-svc"

// ContractEvent is this platform's event contract (Doc 03 §19): every
// published event must carry event name, event version, timestamp, tenant
// ID, legal entity ID, jurisdiction context, actor ID, correlation ID,
// source service, and payload schema version.
//
// LegalEntityID/ActorID/Jurisdiction are `omitempty`, not fabricated
// defaults — Jurisdiction is empty here because domain.Contract has no
// jurisdiction field to source it from.
type ContractEvent struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	ContractID    string      `json:"contract_id"`
	TenantID      string      `json:"tenant_id"`
	LegalEntityID string      `json:"legal_entity_id,omitempty"`
	ActorID       string      `json:"actor_id,omitempty"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	Jurisdiction  string      `json:"jurisdiction,omitempty"`
	OccurredAt    time.Time   `json:"occurred_at"`
	Payload       interface{} `json:"payload"`
}

// PublishParams carries the envelope-level fields a call site supplies,
// alongside the payload-level business object.
type PublishParams struct {
	EventType     string
	ContractID    string
	TenantID      string
	LegalEntityID string
	ActorID       string
	CorrelationID string
	Payload       interface{}
}

// NewEvent builds the envelope for one event. eventID must be unique per
// event — the outbox row id is used, so the id is stable across delivery
// retries (it used to be the deterministic "evt-"+eventType+"-"+contractID,
// which collided across repeat events of the same type on one contract and
// made dedup consumers drop the second, real event).
func NewEvent(eventID string, p PublishParams) ContractEvent {
	return ContractEvent{
		EventID:       eventID,
		EventType:     p.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: SourceService,
		ContractID:    p.ContractID,
		TenantID:      p.TenantID,
		LegalEntityID: p.LegalEntityID,
		ActorID:       p.ActorID,
		CorrelationID: p.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       p.Payload,
	}
}

// MessageWriter is the one method KafkaPublisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// KafkaPublisher delivers outbox rows to a Kafka topic.
type KafkaPublisher struct {
	writer MessageWriter
	topic  string
	logger *zap.Logger
}

// NewKafkaPublisher creates a new KafkaPublisher.
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

// PublishOutbox publishes one already-built envelope, keyed by the aggregate
// (contract) id as before. The stable outbox event id travels as the
// X-Event-ID header across every retry so consumers can de-duplicate
// at-least-once delivery. A failure is RETURNED (the relay keeps the row and
// retries) — it is never swallowed.
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
