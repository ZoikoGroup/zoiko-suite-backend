package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event is this platform's event contract (Doc 03 §19 / ZS-EVENT-001's
// predecessor — see docs/architecture/master-register-findings for the
// pending estate-wide envelope migration, not undertaken by this service).
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

// PublishParams carries the envelope-level fields a call site supplies.
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

// MessageWriter is the one method KafkaPublisher needs from *kafka.Writer —
// narrowed so tests can substitute a fake without a live broker.
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
// caller-supplied MessageWriter — used by tests.
func NewKafkaPublisherWithWriter(writer MessageWriter, topic string, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, topic: topic, logger: logger}
}

func (p *KafkaPublisher) Publish(ctx context.Context, params PublishParams) error {
	evt := Event{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "privacy-purpose-registry-svc",
		EntityID:      params.EntityID,
		TenantID:      params.TenantID,
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
		Key:   []byte(params.EntityID),
		Value: data,
	})
	if err != nil {
		// A broker outage never blocks the HTTP response — same posture as
		// every other publisher in this codebase. Privacy state changes are
		// still recorded authoritatively in this service's own store; the
		// event is a downstream notification, not the system of record.
		p.logger.Warn("kafka publish failed — event dropped", zap.String("event_type", params.EventType), zap.Error(err))
	}
	return nil
}
