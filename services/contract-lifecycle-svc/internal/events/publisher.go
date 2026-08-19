package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

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

// Publisher defines the contract event publishing interface.
type Publisher interface {
	Publish(ctx context.Context, params PublishParams) error
}

// MessageWriter is the one method KafkaPublisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert envelope
// content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// KafkaPublisher publishes contract events to a Kafka topic.
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

// Publish sends a contract domain event to Kafka.
//
// EventID is a fresh UUID per call — it used to be the deterministic string
// "evt-"+eventType+"-"+contractID, which collides across every repeat
// occurrence of the same event type on the same contract (e.g. a contract
// amended twice produces two contract.updated events with the IDENTICAL
// event_id). Any consumer deduplicating via INSERT ... ON CONFLICT
// (event_id) DO NOTHING — the exact pattern this platform's own evidence/
// history consumers use — would have silently dropped the second, real
// event as if it were a redelivery of the first.
func (p *KafkaPublisher) Publish(ctx context.Context, params PublishParams) error {
	evt := ContractEvent{
		EventID:       uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "contract-lifecycle-svc",
		ContractID:    params.ContractID,
		TenantID:      params.TenantID,
		LegalEntityID: params.LegalEntityID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(params.ContractID),
		Value: data,
	})
	if err != nil {
		p.logger.Warn("kafka publish failed — event dropped", zap.String("event_type", params.EventType), zap.Error(err))
	}
	return nil
}
