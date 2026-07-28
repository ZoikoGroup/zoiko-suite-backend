package events

import (
	"context"
	"encoding/json"
	"time"

	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// ContractEvent is the canonical event published to the contract events topic.
type ContractEvent struct {
	EventID    string      `json:"event_id"`
	EventType  string      `json:"event_type"`
	ContractID string      `json:"contract_id"`
	TenantID   string      `json:"tenant_id"`
	OccurredAt time.Time   `json:"occurred_at"`
	Payload    interface{} `json:"payload"`
}

// Publisher defines the contract event publishing interface.
type Publisher interface {
	Publish(ctx context.Context, eventType string, contractID string, tenantID string, payload interface{}) error
}

// KafkaPublisher publishes contract events to a Kafka topic.
type KafkaPublisher struct {
	writer *kafka.Writer
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

// Publish sends a contract domain event to Kafka.
func (p *KafkaPublisher) Publish(ctx context.Context, eventType string, contractID string, tenantID string, payload interface{}) error {
	evt := ContractEvent{
		EventID:    "evt-" + eventType + "-" + contractID,
		EventType:  eventType,
		ContractID: contractID,
		TenantID:   tenantID,
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(contractID),
		Value: data,
	})
	if err != nil {
		p.logger.Warn("kafka publish failed — event dropped", zap.String("event_type", eventType), zap.Error(err))
	}
	return nil
}
