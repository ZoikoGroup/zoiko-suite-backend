package events

import (
	"context"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/store"
)

// Publisher delivers outbox events. Delivery is at-least-once; consumers
// must be idempotent on the CloudEvents id (Doc 05 §20).
type Publisher interface {
	Publish(ctx context.Context, evs []store.OutboxEvent) error
	Close() error
}

// KafkaPublisher writes to Kafka, one topic per event (Topic field).
type KafkaPublisher struct{ w *kafka.Writer }

// NewKafkaPublisher creates a publisher for brokers.
func NewKafkaPublisher(brokers []string) *KafkaPublisher {
	return &KafkaPublisher{w: &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireAll,
		AllowAutoTopicCreation: false,
		BatchTimeout:           50 * time.Millisecond,
	}}
}

func (k *KafkaPublisher) Publish(ctx context.Context, evs []store.OutboxEvent) error {
	msgs := make([]kafka.Message, 0, len(evs))
	for _, e := range evs {
		msgs = append(msgs, kafka.Message{Topic: e.Topic, Key: []byte(e.EventID), Value: e.Payload,
			Headers: []kafka.Header{{Key: "ce_type", Value: []byte(e.EventType)}, {Key: "content-type", Value: []byte("application/cloudevents+json")}}})
	}
	return k.w.WriteMessages(ctx, msgs...)
}

func (k *KafkaPublisher) Close() error { return k.w.Close() }

// LogPublisher is the local-dev publisher: it logs event type and id only.
type LogPublisher struct{ Log *zap.Logger }

func (l LogPublisher) Publish(_ context.Context, evs []store.OutboxEvent) error {
	for _, e := range evs {
		l.Log.Info("event", zap.String("type", e.EventType), zap.String("id", e.EventID), zap.String("topic", e.Topic))
	}
	return nil
}

func (LogPublisher) Close() error { return nil }

// Relay moves outbox rows to the publisher. After MaxAttempts failures an
// event is diverted to "<topic>.dlq" for privileged, redacted review.
type Relay struct {
	Store       store.Store
	Pub         Publisher
	Log         *zap.Logger
	Batch       int
	MaxAttempts int
}

// Once publishes one batch. It returns how many events were delivered.
func (r *Relay) Once(ctx context.Context) (int, error) {
	evs, err := r.Store.UnpublishedEvents(ctx, r.batch())
	if err != nil || len(evs) == 0 {
		return 0, err
	}
	ids := make([]int64, len(evs))
	for i, e := range evs {
		ids[i] = e.OutboxID
	}
	if err := r.Pub.Publish(ctx, evs); err != nil {
		_ = r.Store.MarkAttempt(ctx, ids)
		var dead []store.OutboxEvent
		var deadIDs []int64
		for _, e := range evs {
			if e.Attempts+1 >= r.maxAttempts() {
				e.Topic += ".dlq"
				dead = append(dead, e)
				deadIDs = append(deadIDs, e.OutboxID)
			}
		}
		if len(dead) > 0 && r.Pub.Publish(ctx, dead) == nil {
			_ = r.Store.MarkPublished(ctx, deadIDs, time.Now().UTC())
			r.logger().Error("events diverted to DLQ", zap.Int("count", len(dead)))
		}
		return 0, err
	}
	return len(evs), r.Store.MarkPublished(ctx, ids, time.Now().UTC())
}

// Run relays until ctx ends, backing off with jitter-free doubling on error.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	wait := interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if _, err := r.Once(ctx); err != nil {
			r.logger().Warn("outbox relay failed", zap.Error(err))
			if wait *= 2; wait > time.Minute {
				wait = time.Minute
			}
			continue
		}
		wait = interval
	}
}

func (r *Relay) batch() int {
	if r.Batch <= 0 {
		return 100
	}
	return r.Batch
}

func (r *Relay) maxAttempts() int {
	if r.MaxAttempts <= 0 {
		return 10
	}
	return r.MaxAttempts
}

func (r *Relay) logger() *zap.Logger {
	if r.Log == nil {
		return zap.NewNop()
	}
	return r.Log
}
