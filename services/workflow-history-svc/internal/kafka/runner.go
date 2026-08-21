// Package kafka wires github.com/segmentio/kafka-go Readers to the
// workflow-history-svc consumer.Consumer.
//
// Design notes:
//
//   - kafka-go's kafka.Reader only supports a single topic per reader. This
//     service subscribes to only one topic (zoiko.workflow.events) so a
//     single Runner goroutine handles all five workflow event types.
//
//   - Event ID extraction: the Runner calls extractEventID(msg) to obtain a
//     stable dedup key for every message before passing it to Consumer.Handle.
//     Two dedup paths exist, in preference order:
//
//     (1) X-Event-ID Kafka header — a UUID set by workflow-svc's publisher
//     (internal/events/publisher.go) since the fix in this PR. This is the
//     correct path: the same UUID is preserved across broker-level redeliveries
//     of the same offset, so ON CONFLICT (event_id) DO NOTHING absorbs them.
//
//     (2) Synthetic topic:partition:offset fallback — used when the header is
//     absent (e.g. messages published before the publisher fix, or messages
//     from other producers that don't set the header). This correctly dedupes
//     broker redelivery of the same offset, but a producer-side retry that
//     succeeds on a different offset will produce a distinct synthetic ID and
//     therefore a second row. This is the expected at-least-once posture for
//     the fallback path.
//
//   - Error handling:
//
//   - Validation errors (Consumer.Handle returns nil)  → commit & continue.
//
//   - Store errors (Consumer.Handle returns non-nil)   → retried a bounded
//     number of times against the SAME message (this also covers the
//     out-of-order case: a transition arriving before its workflow.started
//     row usually resolves within this window, since both are produced to
//     the same partition). If every retry still fails, the message is
//     published to "<topic>.dlq" and only then committed, so the partition
//     advances instead of head-of-line blocking every OTHER workflow's
//     events behind one that never resolves — worse for everyone than
//     dead-lettering the one message, and Kafka consumer group offsets are
//     a single per-partition watermark, so leaving it uncommitted forever
//     was never actually safe: a later message's commit would silently
//     drop it anyway (03-microservices.md §19, Doc 01 §2.10).
//     If the DLQ publish itself fails, the original message is left
//     uncommitted (old behavior) so a restart gets another chance.
//
//   - Context cancelled (shutdown)                     → exit cleanly.
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

	"zoiko.io/workflow-history-svc/internal/consumer"
	"zoiko.io/workflow-history-svc/internal/telemetry"
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
			// either a store (DB) failure or an out-of-order delivery that
			// still hasn't resolved. Route to the DLQ and commit past it —
			// see the package doc comment for why leaving it uncommitted
			// forever isn't actually safe here either.
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
// headers (including X-Event-ID) are preserved so the DLQ record stays
// correlatable back to its source.
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
