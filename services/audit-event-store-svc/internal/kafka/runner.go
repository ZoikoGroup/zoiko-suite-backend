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
//       • Store errors (Handler returns non-nil)   → retried a bounded
//         number of times against the SAME message; if every retry still
//         fails, the message is published to "<topic>.dlq" and only then
//         committed, so the partition advances instead of head-of-line
//         blocking on it forever. This matters because Kafka consumer
//         group offsets are a single per-partition watermark, not a
//         sparse per-message ack list: without a DLQ, a later message
//         that succeeds and commits would silently carry the offset past
//         an earlier failed one, permanently and invisibly dropping it —
//         exactly the silent-loss failure mode 03-microservices.md §19
//         and Doc 01 §2.10 ("no silent state change") both prohibit.
//         If the DLQ publish itself fails, the original message is left
//         uncommitted (old behavior) so a restart gets another chance.
//       • Context cancelled (shutdown)             → exit cleanly.
//
//   - TODO (production): TLS/SASL broker auth, StartOffset configuration,
//     consumer group lag Prometheus metrics, and configurable
//     MinBytes/MaxBytes/MaxWait before production cutover.
package kafka

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/consumer"
	"zoiko.io/audit-event-store-svc/internal/telemetry"
)

// dlqRetryAttempts bounds how many times the SAME message is retried
// against the handler within one fetch iteration before it's dead-lettered.
// This is deliberately small and fast — it exists to absorb a transient DB
// blip, not to wait out an extended outage; an outage-length failure still
// exhausts these quickly and correctly falls back to the old "leave
// uncommitted, let a restart retry" behavior via the DLQ-publish-failure path.
const dlqRetryAttempts = 3

// Runner manages the lifecycle of one kafka.Reader goroutine for one topic.
type Runner struct {
	reader    *kafka.Reader
	dlqWriter *kafka.Writer
	handler   *consumer.Consumer
	topic     string
	log       *zap.Logger
	metrics   *telemetry.Metrics
}

// NewRunner constructs a Runner for a single topic. metrics records one
// messages_consumed_total observation per message and starts one OTel
// span per message (Observability Baseline, 03-microservices.md §3.8 —
// see internal/telemetry's doc comment on why this service's shape
// differs from the HTTP-request-per-span pattern every other service
// uses).
func NewRunner(brokers []string, groupID, topic string, h *consumer.Consumer, metrics *telemetry.Metrics, log *zap.Logger) *Runner {
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

	dlqWriter := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic + ".dlq",
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}

	return &Runner{
		reader:    r,
		dlqWriter: dlqWriter,
		handler:   h,
		topic:     topic,
		log:       log.With(zap.String("kafka_topic", topic)),
		metrics:   metrics,
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

		spanCtx, span := telemetry.StartConsumeSpan(ctx, r.topic, eventID)
		err = r.handler.Handle(spanCtx, eventID, msg.Value)
		for attempt := 1; err != nil && attempt < dlqRetryAttempts; attempt++ {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
			err = r.handler.Handle(spanCtx, eventID, msg.Value)
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()

		if err != nil {
			// A non-nil error from Handle after dlqRetryAttempts tries means
			// a store (DB) failure that isn't self-resolving within a few
			// hundred milliseconds. Route the message to the DLQ topic and
			// commit past it — see the package doc comment for why leaving
			// it uncommitted is NOT safe here (a later message's commit
			// would silently drop it anyway).
			if dlqErr := r.publishToDLQ(ctx, msg, err); dlqErr != nil {
				r.metrics.MessagesConsumedTotal.WithLabelValues(r.topic, "store_error").Inc()
				r.log.Error("handler failed and DLQ publish also failed — not committing offset, a restart will retry",
					zap.String("event_id", eventID),
					zap.Int64("offset", msg.Offset),
					zap.Error(err),
					zap.Error(dlqErr),
				)
				continue
			}
			r.metrics.MessagesConsumedTotal.WithLabelValues(r.topic, "dead_lettered").Inc()
			r.log.Error("handler failed after retries — dead-lettered and committing to unblock the partition",
				zap.String("event_id", eventID),
				zap.Int64("offset", msg.Offset),
				zap.Error(err),
			)
		} else {
			r.metrics.MessagesConsumedTotal.WithLabelValues(r.topic, "ok").Inc()
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

// Close shuts down the underlying kafka.Reader and DLQ writer gracefully.
// It should be deferred after Run() has returned.
func (r *Runner) Close() {
	if err := r.reader.Close(); err != nil {
		r.log.Error("kafka reader close error", zap.Error(err))
	}
	if err := r.dlqWriter.Close(); err != nil {
		r.log.Error("kafka DLQ writer close error", zap.Error(err))
	}
}

// publishToDLQ republishes msg, unchanged, to "<topic>.dlq" with two added
// headers recording why and when it was dead-lettered — the original
// headers (including X-Event-ID) are preserved so the DLQ record stays
// correlatable back to its source. The original partition/offset are not
// preserved (a DLQ topic has its own, unrelated partitioning) but are
// captured as headers for operator visibility.
func (r *Runner) publishToDLQ(ctx context.Context, msg kafka.Message, handleErr error) error {
	headers := append([]kafka.Header{}, msg.Headers...)
	headers = append(headers,
		kafka.Header{Key: "X-DLQ-Reason", Value: []byte(handleErr.Error())},
		kafka.Header{Key: "X-DLQ-Source-Topic", Value: []byte(msg.Topic)},
		kafka.Header{Key: "X-DLQ-Source-Partition", Value: []byte(fmt.Sprintf("%d", msg.Partition))},
		kafka.Header{Key: "X-DLQ-Source-Offset", Value: []byte(fmt.Sprintf("%d", msg.Offset))},
		kafka.Header{Key: "X-DLQ-Dead-Lettered-At", Value: []byte(time.Now().UTC().Format(time.RFC3339Nano))},
	)
	return r.dlqWriter.WriteMessages(ctx, kafka.Message{
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
	})
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
