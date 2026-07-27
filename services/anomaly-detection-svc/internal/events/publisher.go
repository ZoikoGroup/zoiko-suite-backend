package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

type Publisher interface {
	Publish(ctx context.Context, eventType, subjectID, tenantID string, payload interface{}) error
}

type KafkaPublisher struct {
	writer *kafka.Writer
	logger *zap.Logger
}

func NewKafkaPublisher(brokers []string, topic string, logger *zap.Logger) *KafkaPublisher {
	writer := &kafka.Writer{
		Addr:     kafka.TCP(brokers...),
		Topic:    topic,
		Balancer: &kafka.LeastBytes{},
	}
	return &KafkaPublisher{writer: writer, logger: logger}
}

func (p *KafkaPublisher) Publish(ctx context.Context, eventType, subjectID, tenantID string, payload interface{}) error {
	data, err := json.Marshal(map[string]interface{}{
		"event_type": eventType,
		"subject_id": subjectID,
		"tenant_id":  tenantID,
		"timestamp":  time.Now().UTC(),
		"data":       payload,
	})
	if err != nil {
		return err
	}

	err = p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(subjectID),
		Value: data,
	})
	if err != nil {
		p.logger.Error("failed to publish Kafka event", zap.String("event_type", eventType), zap.Error(err))
		return err
	}
	p.logger.Info("published Kafka event", zap.String("event_type", eventType), zap.String("subject_id", subjectID))
	return nil
}
