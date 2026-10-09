// Package outbox provides a background worker that polls the transactional
// outbox table and publishes events to Kafka — satisfying the
// "transactional outbox; event loss not allowed" requirement (Event
// Catalogue §6, GCP §2 invariant #10) that the synchronous publisher does not.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

const (
	// outboxPollInterval is how often the worker polls for new events.
	outboxPollInterval = 500 * time.Millisecond
	// outboxBatchSize is the maximum number of events to process in one batch.
	outboxBatchSize = 100
)

// outboxEvent represents a row from the outbox table.
type outboxEvent struct {
	OutboxID        int64
	EventType       string
	Payload         []byte
	TenantID        string
	LegalEntityID   string
	ActorID         string
	CorrelationID   string
	IdempotencyKey  string
	CreatedAt       time.Time
	PublishedAt     *time.Time
	Attempts        int
	LastError       *string
}

// Worker polls the outbox table and publishes events to Kafka.
type Worker struct {
	pool       *pgxpool.Pool
	producer   *kafka.Writer
	topic      string
	log        *zap.Logger
	stopCh     chan struct{}
	doneCh     chan struct{}
}

// NewWorker constructs an outbox worker.
func NewWorker(pool *pgxpool.Pool, producer *kafka.Writer, topic string, log *zap.Logger) *Worker {
	return &Worker{
		pool:     pool,
		producer: producer,
		topic:    topic,
		log:      log,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// Start begins the outbox polling loop in a background goroutine.
func (w *Worker) Start(ctx context.Context) {
	go w.run(ctx)
}

// Stop signals the worker to stop and waits for it to finish.
func (w *Worker) Stop() {
	close(w.stopCh)
	<-w.doneCh
}

// run is the main polling loop.
func (w *Worker) run(ctx context.Context) {
	defer close(w.doneCh)

	ticker := time.NewTicker(outboxPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			w.log.Info("outbox worker stopped")
			return
		case <-ticker.C:
			w.processBatch(ctx)
		case <-ctx.Done():
			w.log.Info("outbox worker context cancelled")
			return
		}
	}
}

// processBatch fetches and publishes a batch of pending outbox events.
func (w *Worker) processBatch(ctx context.Context) {
	// Fetch pending events (those not yet published, ordered by created_at)
	const fetchQuery = `
		SELECT outbox_id, event_type, payload, tenant_id, legal_entity_id, actor_id, correlation_id, idempotency_key, created_at, published_at, attempts, last_error
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY created_at
		LIMIT $1;`

	rows, err := w.pool.Query(ctx, fetchQuery, outboxBatchSize)
	if err != nil {
		w.log.Error("outbox: fetch failed", zap.Error(err))
		return
	}
	defer rows.Close()

	var events []outboxEvent
	for rows.Next() {
		var e outboxEvent
		var publishedAt *time.Time
		var lastError *string
		if err := rows.Scan(
			&e.OutboxID, &e.EventType, &e.Payload, &e.TenantID, &e.LegalEntityID, &e.ActorID, &e.CorrelationID, &e.IdempotencyKey, &e.CreatedAt, &publishedAt, &e.Attempts, &lastError,
		); err != nil {
			w.log.Error("outbox: scan failed", zap.Error(err))
			return
		}
		e.PublishedAt = publishedAt
		e.LastError = lastError
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		w.log.Error("outbox: rows error", zap.Error(err))
		return
	}

	if len(events) == 0 {
		return
	}

	w.log.Debug("outbox: processing batch", zap.Int("count", len(events)))

	// Publish each event
	for _, e := range events {
		if err := w.publishEvent(ctx, e); err != nil {
			w.log.Error("outbox: publish failed",
				zap.Int64("outbox_id", e.OutboxID),
				zap.String("event_type", e.EventType),
				zap.Error(err),
			)
			// Increment attempts and record error
			if incErr := w.incrementAttempts(ctx, e.OutboxID, err.Error()); incErr != nil {
				w.log.Error("outbox: increment attempts failed", zap.Int64("outbox_id", e.OutboxID), zap.Error(incErr))
			}
			// Continue with other events; failed event will be retried on next poll
			continue
		}

		// Mark as published
		if err := w.markPublished(ctx, e.OutboxID); err != nil {
			w.log.Error("outbox: mark published failed",
				zap.Int64("outbox_id", e.OutboxID),
				zap.Error(err),
			)
		}
	}
}

// publishEvent publishes a single event to Kafka.
func (w *Worker) publishEvent(ctx context.Context, e outboxEvent) error {
	// Parse the payload to get correlation_id for message key
	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}

	correlationID := ""
	if cid, ok := payload["correlation_id"].(string); ok {
		correlationID = cid
	}

	msg := kafka.Message{
		Key:       []byte(correlationID),
		Value:     e.Payload,
		Headers:   []kafka.Header{{Key: "Idempotency-Key", Value: []byte(e.IdempotencyKey)}},
		Topic:     w.topic,
		Time:      time.Now().UTC(),
	}

	return w.producer.WriteMessages(ctx, msg)
}

// markPublished marks an outbox event as published.
func (w *Worker) markPublished(ctx context.Context, outboxID int64) error {
	const query = `UPDATE outbox SET published_at = NOW() WHERE outbox_id = $1;`
	_, err := w.pool.Exec(ctx, query, outboxID)
	return err
}

// incrementAttempts increments the attempt counter and records the error.
func (w *Worker) incrementAttempts(ctx context.Context, outboxID int64, errMsg string) error {
	const query = `UPDATE outbox SET attempts = attempts + 1, last_error = $1 WHERE outbox_id = $2`
	_, err := w.pool.Exec(ctx, query, errMsg, outboxID)
	return err
}