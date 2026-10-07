// Package events contains document-vault-svc's Kafka publisher — the
// relay-facing half of the transactional outbox (see internal/outbox).
//
// Unlike some of this platform's older per-service publishers, there are
// deliberately no direct/synchronous PublishXxx methods here: every
// domain event for this service is written to outbox_events inside the
// same transaction as its business mutation (see internal/store/pg_store.go),
// and this Publisher only exists to drain that table. Adding a parallel
// synchronous publish path would just be a second, easier-to-forget way to
// emit the same event.
package events

import (
	"context"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert message
// content without a live broker — *kafka.Writer satisfies it, so
// cmd/server passes its writer unchanged.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher implements the outbox.Publisher contract against the real
// Kafka event backbone.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// PublishOutbox sends a pre-serialized outbox event payload to Kafka with
// the aggregateID as the message key and X-Event-ID as a header. Kafka
// errors are returned so the outbox relay can track them for retry.
func (p *Publisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	msg := kafka.Message{
		Topic: p.topic,
		Key:   []byte(aggregateID),
		Value: payload,
		Headers: []kafka.Header{
			{Key: "X-Event-ID", Value: []byte(outboxEventID)},
		},
	}
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		p.log.Error("failed to publish outbox event to kafka",
			zap.String("outbox_event_id", outboxEventID),
			zap.String("aggregate_id", aggregateID),
			zap.String("topic", p.topic),
			zap.Error(err),
		)
		return err
	}
	return nil
}

// LogOnlyPublisher satisfies outbox.Publisher without a broker. Selected
// only when KAFKA_BROKERS is explicitly empty (local/dev), same posture as
// accounts-payable-svc's own LogOnlyPublisher.
type LogOnlyPublisher struct {
	log *zap.Logger
}

func NewLogOnlyPublisher(log *zap.Logger) *LogOnlyPublisher {
	log.Warn("no Kafka brokers configured — outbox events will be logged, not published")
	return &LogOnlyPublisher{log: log}
}

func (p *LogOnlyPublisher) PublishOutbox(_ context.Context, outboxEventID, aggregateID string, _ []byte) error {
	p.log.Info("outbox event not published (no broker configured)",
		zap.String("outbox_event_id", outboxEventID),
		zap.String("aggregate_id", aggregateID),
	)
	return nil
}
