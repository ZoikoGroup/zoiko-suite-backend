package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

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

type PublishParams struct {
	EventType     string
	EntityID      string
	TenantID      string
	ActorID       string
	CorrelationID string
	Payload       interface{}
}

type Publisher interface {
	Publish(ctx context.Context, params PublishParams) error
	PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error
}

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

func NewKafkaPublisherWithWriter(writer MessageWriter, topic string, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{writer: writer, topic: topic, logger: logger}
}

func (p *KafkaPublisher) Publish(ctx context.Context, params PublishParams) error {
	evt := Event{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "payment-authorization-svc",
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
		p.logger.Warn("kafka publish failed — event dropped", zap.String("event_type", params.EventType), zap.Error(err))
	}
	return nil
}

// PublishOutbox publishes an event from the transactional outbox relay, preserving
// the stable outboxEventID as the X-Event-ID Kafka header across all retries.
func (p *KafkaPublisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	msg := kafka.Message{
		Key:   []byte(aggregateID),
		Value: payload,
		Headers: []kafka.Header{
			{Key: "X-Event-ID", Value: []byte(outboxEventID)},
		},
	}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.logger.Warn("outbox kafka write failed",
			zap.String("outbox_event_id", outboxEventID),
			zap.String("aggregate_id", aggregateID),
			zap.Error(err),
		)
		return err
	}
	p.logger.Info("outbox event published",
		zap.String("outbox_event_id", outboxEventID),
		zap.String("aggregate_id", aggregateID),
		zap.String("topic", p.topic),
	)
	return nil
}
