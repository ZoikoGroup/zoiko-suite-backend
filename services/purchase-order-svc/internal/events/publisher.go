// Package events relays this service's domain events to Kafka.
//
// Events are no longer published from request handlers. They are written to the
// transactional outbox (internal/outbox) in the same database transaction as
// the state change and delivered by the relay through Publisher.PublishOutbox,
// which returns the error so a failed delivery is retried instead of dropped —
// the previous best-effort publish logged the failure and moved on, so a Kafka
// outage silently lost events for state changes that had committed.
package events

import (
	"context"
	"fmt"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// MessageWriter is the one method Publisher needs from *kafka.Writer, narrowed
// so tests can assert delivery without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher implements outbox.Publisher against the Kafka event backbone.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher with a caller-supplied MessageWriter.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishOutbox delivers one outbox row's already-built envelope. The stable
// outbox event id travels as the X-Event-ID header across every retry.
func (p *Publisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	msg := kafka.Message{
		Key:     []byte(aggregateID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(outboxEventID)}},
	}
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		p.log.Warn("outbox kafka write failed", zap.String("outbox_event_id", outboxEventID),
			zap.String("topic", p.topic), zap.Error(err))
		return fmt.Errorf("kafka write: %w", err)
	}
	return nil
}
