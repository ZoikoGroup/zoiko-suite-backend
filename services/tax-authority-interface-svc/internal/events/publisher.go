package events

import (
	"context"

	"go.uber.org/zap"
)

type Publisher interface {
	Publish(ctx context.Context, eventType, aggregateID, tenantID string, payload interface{}) error
}

type KafkaPublisher struct {
	brokers []string
	topic   string
	logger  *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	return &KafkaPublisher{
		brokers: brokers,
		topic:   topic,
		logger:  logger,
	}
}

func (p *KafkaPublisher) Publish(ctx context.Context, eventType, aggregateID, tenantID string, payload interface{}) error {
	if p.logger != nil {
		p.logger.Info("event published",
			zap.String("event_type", eventType),
			zap.String("aggregate_id", aggregateID),
			zap.String("tenant_id", tenantID),
			zap.String("topic", p.topic),
		)
	}
	return nil
}

type MockPublisher struct {
	Events []map[string]interface{}
}

func NewMockPublisher() *MockPublisher {
	return &MockPublisher{Events: make([]map[string]interface{}, 0)}
}

func (m *MockPublisher) Publish(ctx context.Context, eventType, aggregateID, tenantID string, payload interface{}) error {
	m.Events = append(m.Events, map[string]interface{}{
		"event_type":   eventType,
		"aggregate_id": aggregateID,
		"tenant_id":    tenantID,
		"payload":      payload,
	})
	return nil
}
