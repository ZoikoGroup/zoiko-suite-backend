// Package events delivers this service's outbox rows to Kafka.
//
// Nothing is published from the request path: events are written to the
// transactional outbox (internal/outbox) in the same transaction as the state
// change, and the outbox Relay calls PublishOutbox to deliver them, retrying on
// failure. Pattern: purchase-request-svc/internal/events.
package events

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

// MessageWriter is the one method Publisher needs from *kafka.Writer, so tests
// can assert content without a broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher delivers outbox rows to the Kafka event backbone.
type Publisher struct {
	topic    string
	producer MessageWriter
}

// NewKafkaPublisher builds a publisher for a comma-separated broker list.
func NewKafkaPublisher(brokers, topic string) (*Publisher, func() error) {
	w := &kafka.Writer{
		Addr:                   kafka.TCP(strings.Split(brokers, ",")...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
		// One message per relay row; do not wait a second for a batch to fill.
		BatchTimeout: 10 * time.Millisecond,
		RequiredAcks: kafka.RequireAll,
	}
	return &Publisher{topic: topic, producer: w}, w.Close
}

// NewPublisherWithWriter is used by tests to substitute a fake writer.
func NewPublisherWithWriter(topic string, producer MessageWriter) *Publisher {
	return &Publisher{topic: topic, producer: producer}
}

// PublishOutbox publishes one already-built envelope. The stable outbox event
// id travels as the X-Event-ID header across every retry so consumers can
// de-duplicate at-least-once delivery. A failure is RETURNED (the relay keeps
// the row and retries) — never swallowed.
func (p *Publisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	if err := p.producer.WriteMessages(ctx, kafka.Message{
		Key:     []byte(aggregateID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(outboxEventID)}},
	}); err != nil {
		return fmt.Errorf("kafka write to %s: %w", p.topic, err)
	}
	return nil
}
