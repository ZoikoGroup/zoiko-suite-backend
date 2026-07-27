package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

type Event struct {
	Type      string      `json:"type"`
	TenantID  string      `json:"tenant_id"`
	Source    string      `json:"source"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"`
}

type Publisher struct {
	writer *kafka.Writer
	logger *zap.Logger
}

func NewPublisher(brokers []string, topic string, logger *zap.Logger) *Publisher {
	return &Publisher{
		writer: &kafka.Writer{
			Addr:     kafka.TCP(brokers...),
			Topic:    topic,
			Balancer: &kafka.LeastBytes{},
		},
		logger: logger,
	}
}

func (p *Publisher) Publish(ctx context.Context, eventType, tenantID string, data interface{}) error {
	payload, err := json.Marshal(Event{
		Type: eventType, TenantID: tenantID,
		Source: "migration-integrity-svc", Timestamp: time.Now().UTC(), Data: data,
	})
	if err != nil {
		return err
	}
	p.logger.Info("Publishing event", zap.String("type", eventType))
	go func() {
		_ = p.writer.WriteMessages(context.Background(), kafka.Message{Key: []byte(tenantID), Value: payload})
	}()
	return nil
}

func (p *Publisher) Close() error { return p.writer.Close() }
