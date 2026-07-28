package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

type Event struct {
	ID        string      `json:"id"`
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
	writer := &kafka.Writer{
		Addr:     kafka.TCP(brokers...),
		Topic:    topic,
		Balancer: &kafka.LeastBytes{},
	}

	return &Publisher{
		writer: writer,
		logger: logger,
	}
}

func (p *Publisher) Publish(ctx context.Context, eventType, tenantID string, data interface{}) error {
	evt := Event{
		Type:      eventType,
		TenantID:  tenantID,
		Source:    "forecasting-svc",
		Timestamp: time.Now().UTC(),
		Data:      data,
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}

	msg := kafka.Message{
		Key:   []byte(tenantID),
		Value: payload,
	}

	p.logger.Info("Publishing event", zap.String("type", eventType), zap.String("tenant_id", tenantID))
	
	// Best-effort publish in local development mode
	go func() {
		_ = p.writer.WriteMessages(context.Background(), msg)
	}()

	return nil
}

func (p *Publisher) Close() error {
	return p.writer.Close()
}
