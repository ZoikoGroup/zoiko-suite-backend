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
	EventID    string      `json:"event_id"`
	EventType  string      `json:"event_type"`
	EntityID   string      `json:"entity_id"`
	TenantID   string      `json:"tenant_id"`
	OccurredAt time.Time   `json:"occurred_at"`
	Payload    interface{} `json:"payload"`
}

type Publisher interface {
	Publish(ctx context.Context, eventType string, entityID string, tenantID string, payload interface{}) error
}

type KafkaPublisher struct {
	writer *kafka.Writer
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

func (p *KafkaPublisher) Publish(ctx context.Context, eventType string, entityID string, tenantID string, payload interface{}) error {
	evt := Event{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:    "evt-" + uuid.New().String(),
		EventType:  eventType,
		EntityID:   entityID,
		TenantID:   tenantID,
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(entityID),
		Value: data,
	})
	if err != nil {
		// Every existing call site discards this return value (`_ =
		// h.publisher.Publish(...)`), so returning the real error here
		// changes nothing for them — they keep behaving exactly as
		// before, fire-and-forget. It matters for internal/outbox's
		// Relay, which is a NEW caller that actually checks this error to
		// decide whether to mark an outbox row published or retry it;
		// swallowing the error here would make that retry logic dead
		// code.
		p.logger.Warn("kafka publish failed", zap.String("event_type", eventType), zap.Error(err))
		return err
	}
	return nil
}
