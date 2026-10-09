// Package events delivers this service's domain events to Kafka.
//
// Events are NOT published from the request path any more. They are written to
// the transactional outbox (internal/outbox) in the same transaction as the
// state change, with the platform envelope (Doc 03 §19: event name, version,
// timestamp, tenant, legal entity, actor, correlation, source service, schema
// version) built at write time; the outbox Relay calls PublishOutbox to deliver
// them, retrying on failure. The previous fire-and-forget publish after commit
// lost the only notice of a change when the broker hiccuped.
package events

import (
	"context"
	"fmt"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so tests can assert content without a broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher delivers outbox rows to the Kafka event backbone.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishOutbox publishes one already-built envelope. The stable outbox event
// id travels as the X-Event-ID header across every retry so consumers can
// de-duplicate at-least-once delivery. A failure is RETURNED (the relay keeps
// the row and retries) — it is never swallowed.
func (p *Publisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	err := p.producer.WriteMessages(ctx, kafka.Message{
		Key:     []byte(aggregateID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(outboxEventID)}},
	})
	if err != nil {
		p.log.Warn("outbox kafka write failed",
			zap.String("outbox_event_id", outboxEventID), zap.String("topic", p.topic), zap.Error(err))
		return fmt.Errorf("kafka write: %w", err)
	}
	return nil
}
