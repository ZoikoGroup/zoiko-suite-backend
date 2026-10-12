package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version.
//
// TenantID/LegalEntityID/Jurisdiction are correctly omitted, not
// fabricated: capabilities and their release state are platform-wide
// reference data — domain.Capability and domain.Release have no tenant_id
// field at all, same as jurisdiction-rules-svc's platform-scoped objects.
type Event struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	EntityID      string      `json:"entity_id"`
	TenantID      string      `json:"tenant_id,omitempty"`
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
	ActorID       string
	CorrelationID string
	Payload       interface{}
}

// Publisher is the interface for emitting domain events.
type Publisher interface {
	Publish(ctx context.Context, params PublishParams) error
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

func NewEvent(params PublishParams) Event {
	return Event{
		EventID:       "evt-" + uuid.NewString(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "capability-registry-svc",
		EntityID:      params.EntityID,
		TenantID:      params.TenantID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}
}

func (p *KafkaPublisher) Publish(ctx context.Context, params PublishParams) error {
	evt := NewEvent(params)
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	return p.PublishOutbox(ctx, params.EntityID, data)
}

// PublishOutbox sends the already-serialized event so retries preserve its
// original event ID, timestamp, and payload. Consumers use the envelope event
// ID for deduplication, so the Kafka header must match the envelope.
func (p *KafkaPublisher) PublishOutbox(ctx context.Context, entityID string, payload []byte) error {
	if !json.Valid(payload) {
		return fmt.Errorf("publish outbox event: invalid JSON payload")
	}
	var event struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("publish outbox event: decode event ID: %w", err)
	}
	if event.EventID == "" {
		return fmt.Errorf("publish outbox event: event_id is required")
	}
	err := p.writer.WriteMessages(ctx, kafka.Message{
		Key:     []byte(entityID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(event.EventID)}},
	})
	if err != nil {
		p.logger.Warn("kafka outbox publish failed", zap.String("entity_id", entityID), zap.Error(err))
		return fmt.Errorf("publish outbox event for %s: %w", entityID, err)
	}
	return nil
}

func (p *KafkaPublisher) Close() error {
	if closer, ok := p.writer.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}
