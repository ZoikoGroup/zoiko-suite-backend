// Package kafka wires segmentio/kafka-go Readers to the indexer.
//
// THREE THINGS HERE ARE NOT THE LIBRARY'S DEFAULTS, and each of them exists
// because the default produces a consumer that looks healthy while consuming
// nothing:
//
//  1. ErrorLogger is SET. kafka-go discards reader errors when it is nil —
//     group coordination failures, rebalance errors, unreachable brokers —
//     so a reader that can never join its group sits in FetchMessage forever
//     and the process reports ready, logs nothing, and indexes nothing. The
//     one symptom is a metric that stays flat, which reads exactly like a
//     quiet period.
//
//  2. One reader PER TOPIC, in its own goroutine. kafka-go's Reader takes a
//     single topic when a GroupID is set; handing it a comma-joined string is
//     accepted and matches no topic at all.
//
//  3. A partition WATCH on the reader (WatchPartitionChanges), so a topic that
//     grows partitions after this consumer joined is actually consumed rather
//     than silently ignored until the next restart.
//
// Error handling, in the same shape workflow-history-svc's runner settled on:
// a contract violation commits (it will not become valid on a retry, and
// blocking the partition on it stops every other tenant behind one bad
// document); a transient failure is retried a bounded number of times against
// the same message and then dead-lettered so the partition advances. Leaving a
// message uncommitted forever was never actually safe — Kafka consumer group
// offsets are a single per-partition watermark, so a later message's commit
// silently drops it anyway.
package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"

	"zoiko.io/search-indexer-svc/internal/indexer"
	"zoiko.io/search-indexer-svc/internal/projection"
	"zoiko.io/search-indexer-svc/internal/telemetry"
)

// retryAttempts bounds how many times the SAME message is retried against the
// handler within one fetch iteration before it is dead-lettered.
const retryAttempts = 3

// Runner manages one kafka.Reader goroutine per topic.
type Runner struct {
	brokers   []string
	groupID   string
	ix        *indexer.Indexer
	metrics   *telemetry.Metrics
	log       *zap.Logger
	dlqWriter *kafka.Writer

	mu      sync.Mutex
	readers map[string]*kafka.Reader
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func NewRunner(brokers []string, groupID string, ix *indexer.Indexer, metrics *telemetry.Metrics, log *zap.Logger) *Runner {
	return &Runner{
		brokers: brokers,
		groupID: groupID,
		ix:      ix,
		metrics: metrics,
		log:     log,
		dlqWriter: &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Balancer:               &kafka.LeastBytes{},
			AllowAutoTopicCreation: true,
		},
		readers: map[string]*kafka.Reader{},
		cancels: map[string]context.CancelFunc{},
	}
}

// errorLogger adapts zap to kafka-go's Logger interface.
//
// The whole reason this type exists: without it kafka-go's ErrorLogger is nil
// and every group-coordination failure is discarded. Warn rather than Error
// because a rebalance logs through this path too and is routine; the
// distinction that matters is that it is VISIBLE.
type errorLogger struct{ log *zap.Logger }

func (l errorLogger) Printf(format string, args ...any) {
	l.log.Warn("kafka: " + fmt.Sprintf(format, args...))
}

// Subscribe brings the running reader set into line with topics: starts a
// reader for each new topic, stops readers for topics no longer registered.
//
// Idempotent, so it can be called on every registry reload without tracking
// what changed.
func (r *Runner) Subscribe(ctx context.Context, topics []string) {
	want := map[string]bool{}
	for _, t := range topics {
		if t != "" {
			want[t] = true
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for topic, cancel := range r.cancels {
		if !want[topic] {
			r.log.Info("kafka: unsubscribing from a topic with no registered source",
				zap.String("topic", topic))
			cancel()
			delete(r.cancels, topic)
			delete(r.readers, topic)
		}
	}

	for topic := range want {
		if _, running := r.readers[topic]; running {
			continue
		}
		reader := kafka.NewReader(kafka.ReaderConfig{
			Brokers: r.brokers,
			GroupID: r.groupID,
			Topic:   topic,

			MinBytes: 1,
			MaxBytes: 10 << 20,

			// Start from the oldest available message when this group has no
			// committed offset. A search index that begins at the tail is an
			// index missing every record created before it was switched on,
			// and nothing would ever report that as an error.
			StartOffset: kafka.FirstOffset,

			// See the package comment, point 1. This is the line that turns a
			// silent dead consumer into a logged one.
			ErrorLogger: errorLogger{log: r.log.With(zap.String("kafka_topic", topic))},

			// See point 3. Without it a partition added after this reader
			// joined is never assigned to anyone in the group.
			WatchPartitionChanges:  true,
			PartitionWatchInterval: 30 * time.Second,
		})

		topicCtx, cancel := context.WithCancel(ctx)
		r.readers[topic] = reader
		r.cancels[topic] = cancel

		r.wg.Add(1)
		go func(topic string, reader *kafka.Reader) {
			defer r.wg.Done()
			r.run(topicCtx, topic, reader)
		}(topic, reader)

		r.log.Info("kafka: subscribed", zap.String("topic", topic), zap.String("group", r.groupID))
	}
}

func (r *Runner) run(ctx context.Context, topic string, reader *kafka.Reader) {
	log := r.log.With(zap.String("kafka_topic", topic))
	log.Info("kafka consumer loop starting")
	defer log.Info("kafka consumer loop stopped")
	defer func() { _ = reader.Close() }()

	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("kafka fetch error — will retry", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		eventID := extractEventID(msg)
		spanCtx, span := telemetry.StartConsumeSpan(ctx, topic, eventID)
		outcome, handleErr := r.handle(spanCtx, msg)

		// Only OutcomeError is retried. The other four are terminal decisions
		// the indexer has already made, and retrying a decision produces the
		// same decision more slowly.
		for attempt := 1; outcome == indexer.OutcomeError && attempt < retryAttempts; attempt++ {
			select {
			case <-ctx.Done():
				span.End()
				return
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
			outcome, handleErr = r.handle(spanCtx, msg)
		}

		if handleErr != nil {
			span.RecordError(handleErr)
			span.SetStatus(codes.Error, handleErr.Error())
		}
		span.End()

		if outcome == indexer.OutcomeError {
			if dlqErr := r.publishToDLQ(ctx, msg, handleErr); dlqErr != nil {
				r.metrics.MessagesConsumedTotal.WithLabelValues(topic, "error").Inc()
				log.Error("handler failed and DLQ publish also failed — not committing; a restart will retry",
					zap.String("event_id", eventID),
					zap.Int64("offset", msg.Offset),
					zap.Error(handleErr), zap.NamedError("dlq_error", dlqErr))
				continue
			}
			r.metrics.MessagesConsumedTotal.WithLabelValues(topic, "dead_lettered").Inc()
			log.Error("handler failed after retries — dead-lettered and committing to unblock the partition",
				zap.String("event_id", eventID), zap.Int64("offset", msg.Offset), zap.Error(handleErr))
		} else {
			r.metrics.MessagesConsumedTotal.WithLabelValues(topic, string(outcome)).Inc()
			if outcome == indexer.OutcomeQuarantined {
				// Quarantined messages also go to the DLQ, even though they
				// are committed. The commit unblocks the partition; the DLQ
				// copy is the evidence a source owner needs to see what their
				// producer emitted, which is otherwise gone once the retention
				// window passes.
				if dlqErr := r.publishToDLQ(ctx, msg, handleErr); dlqErr != nil {
					log.Warn("quarantined message could not be copied to the DLQ",
						zap.String("event_id", eventID), zap.Error(dlqErr))
				}
			}
		}

		if err := reader.CommitMessages(ctx, msg); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("kafka commit error",
				zap.String("event_id", eventID), zap.Int64("offset", msg.Offset), zap.Error(err))
		}
	}
}

// handle decodes one message and applies it.
func (r *Runner) handle(ctx context.Context, msg kafka.Message) (indexer.Outcome, error) {
	var e projection.Event
	if err := json.Unmarshal(msg.Value, &e); err != nil {
		// A message that is not JSON is not a transient failure. Quarantined,
		// not retried.
		return indexer.OutcomeQuarantined, fmt.Errorf("decode event envelope: %w", err)
	}
	if e.EventID == "" {
		e.EventID = extractEventID(msg)
	}
	return r.ix.Apply(ctx, e)
}

// publishToDLQ republishes a message, unchanged, to "<topic>.dlq" with headers
// recording why and when. The original headers — including X-Event-ID — are
// preserved so the DLQ record stays correlatable back to its source.
func (r *Runner) publishToDLQ(ctx context.Context, msg kafka.Message, cause error) error {
	reason := "unknown"
	if cause != nil {
		reason = cause.Error()
	}
	headers := append([]kafka.Header{}, msg.Headers...)
	headers = append(headers,
		kafka.Header{Key: "X-DLQ-Reason", Value: []byte(reason)},
		kafka.Header{Key: "X-DLQ-Source-Topic", Value: []byte(msg.Topic)},
		kafka.Header{Key: "X-DLQ-Source-Partition", Value: []byte(fmt.Sprintf("%d", msg.Partition))},
		kafka.Header{Key: "X-DLQ-Source-Offset", Value: []byte(fmt.Sprintf("%d", msg.Offset))},
		kafka.Header{Key: "X-DLQ-Dead-Lettered-At", Value: []byte(time.Now().UTC().Format(time.RFC3339Nano))},
	)
	// Topic on the message rather than on the writer: one writer serves every
	// subscribed topic's DLQ, and kafka-go rejects a Message that names a
	// topic when the Writer already has one — so the Writer deliberately has
	// none.
	return r.dlqWriter.WriteMessages(ctx, kafka.Message{
		Topic:   msg.Topic + ".dlq",
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
	})
}

// extractEventID prefers the X-Event-ID header and falls back to the message
// coordinates. The fallback dedupes broker redelivery of the same offset but
// not a producer-side retry that landed on a different one — the expected
// at-least-once posture, and the reason this service's own publisher always
// sets the header.
func extractEventID(msg kafka.Message) string {
	for _, h := range msg.Headers {
		if h.Key == "X-Event-ID" && len(h.Value) > 0 {
			return string(h.Value)
		}
	}
	return fmt.Sprintf("%s:%d:%d", msg.Topic, msg.Partition, msg.Offset)
}

// Close stops every reader and waits for the loops to exit.
func (r *Runner) Close() {
	r.mu.Lock()
	for _, cancel := range r.cancels {
		cancel()
	}
	r.cancels = map[string]context.CancelFunc{}
	r.readers = map[string]*kafka.Reader{}
	r.mu.Unlock()

	r.wg.Wait()
	if err := r.dlqWriter.Close(); err != nil {
		r.log.Error("kafka DLQ writer close error", zap.Error(err))
	}
}
