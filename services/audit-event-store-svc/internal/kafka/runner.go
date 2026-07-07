// Package kafka wires github.com/segmentio/kafka-go Readers to the
// audit-event-store-svc consumer.Handler.
//
// Design notes:
//
//   - kafka-go's kafka.Reader only supports a single topic per reader.  Because
//     this service consumes TWO topics (zoiko.identity.events for
//     identity.context.resolved and zoiko.entity.events for
//     entity.status.changed), one Reader goroutine is started per topic.
//
//   - Event ID extraction: every message is expected to carry an "X-Event-ID"
//     Kafka header (a convention mirroring the HTTP correlation header).  If
//     the header is absent, a stable synthetic ID is derived from
//     "<topic>:<partition>:<offset>" so that the upstream dedup INSERT …
//     ON CONFLICT DO NOTHING still functions correctly.
//
//   - Error handling:
//       • Validation errors (Handler returns nil)  → commit & continue.
//       • Store errors (Handler returns non-nil)   → log & do NOT commit;
//         the broker will re-deliver after the consumer restarts.
//       • Context cancelled (shutdown)             → exit cleanly.
//
//   - TODO (production): TLS/SASL broker auth, StartOffset configuration,
//     per-topic DLQ routing, consumer group lag Prometheus metrics, and
//     configurable MinBytes/MaxBytes/MaxWait before production cutover.
package kafka

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/consumer"
)

// Runner manages the lifecycle of one kafka.Reader goroutine for one topic.
type Runner struct {
	reader  *kafka.Reader
	handler *consumer.Consumer
	topic   string
	log     *zap.Logger
}

// NewRunner constructs a Runner for a single topic.
func NewRunner(brokers []string, groupID, topic string, h *consumer.Consumer, log *zap.Logger) *Runner {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		GroupID: groupID,
		Topic:   topic,

		// Fetch at least 1 byte; wait up to 1 s for messages before returning
		// an empty batch (keeps the loop responsive without busy-polling).
		MinBytes: 1,
		MaxBytes: 10 << 20, // 10 MiB — generous cap for JSONB payloads

		// If this consumer group has no committed offset yet, start from the
		// oldest available message so no events are silently skipped on first boot.
		StartOffset: kafka.FirstOffset,

		// TODO (production): set Dialer with TLS + SASL credentials.
	})

	return &Runner{
		reader:  r,
		handler: h,
		topic:   topic,
		log:     log.With(zap.String("kafka_topic", topic)),
	}
}

// Run blocks reading messages from the topic until ctx is cancelled.
// It is designed to be called in its own goroutine.
func (r *Runner) Run(ctx context.Context) {
	r.log.Info("kafka consumer loop starting")
	defer r.log.Info("kafka consumer loop stopped")

	for {
		// FetchMessage blocks until a message arrives or ctx is cancelled.
		msg, err := r.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Clean shutdown requested.
				return
			}
			r.log.Error("kafka fetch error — will retry",
				zap.Error(err),
				zap.Duration("backoff", time.Second),
			)
			// Brief back-off on transient fetch errors so we don't spin.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		eventID := extractEventID(msg)

		r.log.Debug("kafka message received",
			zap.String("event_id", eventID),
			zap.Int64("offset", msg.Offset),
			zap.Int("partition", msg.Partition),
		)

		if err := r.handler.Handle(ctx, eventID, msg.Value); err != nil {
			// A non-nil error from Handle means a store (DB) failure.
			// Do NOT commit so the broker re-delivers after restart.
			r.log.Error("handler returned store error — not committing offset",
				zap.String("event_id", eventID),
				zap.Int64("offset", msg.Offset),
				zap.Error(err),
			)
			continue
		}

		// Commit after successful handling (or validated-rejection).
		// CommitMessages is a synchronous, exactly-once commit for the
		// consumer group.
		if err := r.reader.CommitMessages(ctx, msg); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.log.Error("kafka commit error",
				zap.String("event_id", eventID),
				zap.Int64("offset", msg.Offset),
				zap.Error(err),
			)
		}
	}
}

// Close shuts down the underlying kafka.Reader gracefully.
// It should be deferred after Run() has returned.
func (r *Runner) Close() {
	if err := r.reader.Close(); err != nil {
		r.log.Error("kafka reader close error", zap.Error(err))
	}
}

// extractEventID pulls the event_id from the "X-Event-ID" Kafka header.
// If absent, it falls back to a deterministic synthetic ID from the message
// coordinates so the upstream ON CONFLICT DO NOTHING dedup still works.
func extractEventID(msg kafka.Message) string {
	for _, h := range msg.Headers {
		if h.Key == "X-Event-ID" && len(h.Value) > 0 {
			return string(h.Value)
		}
	}
	// Synthetic fallback — stable across re-deliveries of the same offset.
	return fmt.Sprintf("%s:%d:%d", msg.Topic, msg.Partition, msg.Offset)
}
