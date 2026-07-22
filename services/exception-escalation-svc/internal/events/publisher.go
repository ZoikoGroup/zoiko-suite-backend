package events

import (
	"context"
	"encoding/json"
	"time"

	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Event is the canonical fact emitted by exception-escalation-svc.
// Events are facts, not commands — append-only, never mutating source truth.
type Event struct {
	EventID          string      `json:"event_id"`
	EventType        string      `json:"event_type"`
	ExceptionCaseID  string      `json:"exception_case_id"`
	TenantID         string      `json:"tenant_id"`
	OccurredAt       time.Time   `json:"occurred_at"`
	Payload          interface{} `json:"payload"`
}

// Publisher is the interface for emitting domain events.
type Publisher interface {
	Publish(ctx context.Context, eventType, caseID, tenantID string, payload interface{}) error
}

// KafkaPublisher is the Kafka-backed implementation.
type KafkaPublisher struct {
	writer *kafka.Writer
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

func (p *KafkaPublisher) Publish(ctx context.Context, eventType, caseID, tenantID string, payload interface{}) error {
	evt := Event{
		EventID:         "evt-" + eventType + "-" + caseID,
		EventType:       eventType,
		ExceptionCaseID: caseID,
		TenantID:        tenantID,
		OccurredAt:      time.Now().UTC(),
		Payload:         payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	if err := p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(caseID),
		Value: data,
	}); err != nil {
		p.logger.Warn("kafka publish failed — event dropped",
			zap.String("event_type", eventType),
			zap.Error(err),
		)
	}
	return nil
}
