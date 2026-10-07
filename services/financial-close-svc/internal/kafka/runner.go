// Package kafka wires github.com/segmentio/kafka-go Readers to
// financial-close-svc's own internal/consumer.Consumer — mirrors
// workflow-history-svc's and audit-event-store-svc's own
// internal/kafka.Runner exactly (same reliability posture, same file
// structure), so this platform has one Kafka-consumer pattern, not a
// second bespoke one.
//
// Design notes:
//
//   - One Runner per source topic. This service subscribes to THREE
//     topics (asset-management-svc's, inventory-management-svc's and
//     project-accounting-svc's own events) rather than one, since
//     kafka-go's kafka.Reader only supports a single topic per reader —
//     one goroutine per topic, all three feeding the same
//     internal/consumer.Consumer.
//
//   - Event ID extraction: the Runner calls extractEventID(msg) for a
//     stable dedup key before passing it to Consumer.Handle. Two paths,
//     in preference order: (1) the "X-Event-ID" Kafka header, if the
//     producer sets one; (2) a synthetic topic:partition:offset
//     fallback otherwise — stable across redelivery of the same offset.
//     financial-close-svc's own RecordLineageEdge is idempotent on
//     (tenant_id, from_type, from_id, to_type, to_id) regardless of
//     which dedup key path is used, so this is defense-in-depth, not
//     the primary idempotency mechanism.
//
//   - Error handling: a validation error (a malformed envelope/payload)
//     and a store error are both retried a bounded number of times
//     against the SAME message, then dead-lettered to "<topic>.dlq" and
//     committed past — so one message that will never resolve doesn't
//     block every other event behind it on the same partition. If the
//     DLQ publish itself fails, the message is left uncommitted so a
//     restart gets another chance.
//
//   - Context cancelled (shutdown) → exit cleanly.
//
//   - TODO (production): TLS/SASL broker auth, StartOffset configuration,
//     configurable MinBytes/MaxBytes/MaxWait before production cutover
//     — same deferred items workflow-history-svc's own Runner already
//     flags.
package kafka

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/consumer"
	"zoiko.io/financial-close-svc/internal/telemetry"
)

// dlqRetryAttempts bounds how many times the SAME message is retried
// against the handler within one fetch iteration before it's dead-lettered.
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
// span per message (Observability Baseline, 03-microservices.md §3.8).
func NewRunner(brokers []string, groupID, topic string, h *consumer.Consumer, metrics *telemetry.Metrics, log *zap.Logger) *Runner {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		GroupID: groupID,
		Topic:   topic,

		// Fetch at least 1 byte; wait up to 1 s for messages before returning
		// an empty batch (keeps the loop responsive without busy-polling).
		MinBytes: 1,
		MaxBytes: 10 << 20, // 10 MiB — generous cap for JSON payloads

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
			// either a store (DB) failure or a malformed message that still
			// hasn't resolved. Route to the DLQ and commit past it — see the
			// package doc comment for why leaving it uncommitted forever
			// isn't actually safe here either.
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
// It should be called after Run() has returned.
func (r *Runner) Close() {
	if err := r.reader.Close(); err != nil {
		r.log.Error("kafka reader close error", zap.Error(err))
	}
	if err := r.dlqWriter.Close(); err != nil {
		r.log.Error("kafka DLQ writer close error", zap.Error(err))
	}
}

// publishToDLQ republishes msg, unchanged, to "<topic>.dlq" with added
// headers recording why and when it was dead-lettered — the original
// headers (including X-Event-ID, if the producer set one) are preserved
// so the DLQ record stays correlatable back to its source.
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
