package events

import (
	"context"
	"encoding/json"
	"time"

	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

type Event struct {
	EventID        string      `json:"event_id"`
	EventType      string      `json:"event_type"`
	CounterpartyID string      `json:"counterparty_id"`
	TenantID       string      `json:"tenant_id"`
	OccurredAt     time.Time   `json:"occurred_at"`
	Payload        interface{} `json:"payload"`
}

type Publisher interface {
	Publish(ctx context.Context, eventType string, counterpartyID string, tenantID string, payload interface{}) error
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

func (p *KafkaPublisher) Publish(ctx context.Context, eventType string, counterpartyID string, tenantID string, payload interface{}) error {
	evt := Event{
		EventID:        "evt-" + eventType + "-" + counterpartyID,
		EventType:      eventType,
		CounterpartyID: counterpartyID,
		TenantID:       tenantID,
		OccurredAt:     time.Now().UTC(),
		Payload:        payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(counterpartyID),
		Value: data,
	})
	if err != nil {
		p.logger.Warn("kafka publish failed — event dropped", zap.String("event_type", eventType), zap.Error(err))
	}
	return nil
}
