// Package events publishes outbox rows to Kafka. Domain events are never
// published directly from a handler any more: they are inserted into
// outbox_events in the state change's own transaction (internal/outbox) and
// this publisher is the relay's sink.
package events

import (
	"context"

	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

type Publisher interface {
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

// PublishOutbox publishes one outbox row, preserving the stable outbox event
// id as the X-Event-ID header across every retry (consumers de-duplicate on it).
func (p *KafkaPublisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	msg := kafka.Message{
		Key:     []byte(aggregateID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(outboxEventID)}},
	}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.logger.Warn("outbox kafka write failed", zap.String("outbox_event_id", outboxEventID), zap.Error(err))
		return err
	}
	return nil
}
