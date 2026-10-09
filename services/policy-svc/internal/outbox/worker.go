// Package outbox provides a background worker that polls the transactional
// outbox table and publishes events to Kafka — satisfying the
// "transactional outbox; event loss not allowed" requirement (Event
// Catalogue §6, V-001 §20.2) that the synchronous publisher does not.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/events"
)

const (
	// outboxPollInterval is how often the worker polls for new events.
	outboxPollInterval = 500 * time.Millisecond
	// outboxBatchSize is the maximum number of events to process in one batch.
	outboxBatchSize = 100
)

// outboxEvent represents a row from the outbox table.
type outboxEvent struct {
	ID              string
	EventType       string
	EventVersion    string
	SchemaVersion   string
	SourceService   string
	TenantID        *string
	LegalEntityID   *string
	ActorID         string
	CorrelationID   string
	IdempotencyKey  string
	Payload         []byte
	CreatedAt       time.Time
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
		SELECT outbox_id, event_type, event_version, schema_version, source_service,
		       tenant_id, legal_entity_id, actor_id, correlation_id, idempotency_key, payload, created_at
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
		var tenantID, legalEntityID *string
		if err := rows.Scan(
			&e.ID, &e.EventType, &e.EventVersion, &e.SchemaVersion, &e.SourceService,
			&tenantID, &legalEntityID, &e.ActorID, &e.CorrelationID, &e.IdempotencyKey, &e.Payload, &e.CreatedAt,
		); err != nil {
			w.log.Error("outbox: scan failed", zap.Error(err))
			return
		}
		e.TenantID = tenantID
		e.LegalEntityID = legalEntityID
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
				zap.String("outbox_id", e.ID),
				zap.String("event_type", e.EventType),
				zap.Error(err),
			)
			// Continue with other events; failed event will be retried on next poll
			continue
		}

		// Mark as published
		if err := w.markPublished(ctx, e.ID); err != nil {
			w.log.Error("outbox: mark published failed",
				zap.String("outbox_id", e.ID),
				zap.Error(err),
			)
		}
	}
}

// publishEvent publishes a single event to Kafka using the canonical envelope.
func (w *Worker) publishEvent(ctx context.Context, e outboxEvent) error {
	env := events.Envelope{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     e.EventType,
		EventVersion:  e.EventVersion,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: e.SchemaVersion,
		SourceService: e.SourceService,
		TenantID:      deref(e.TenantID),
		LegalEntityID: deref(e.LegalEntityID),
		ActorID:       e.ActorID,
		CorrelationID: e.CorrelationID,
		Payload:       json.RawMessage(e.Payload),
	}

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	msg := kafka.Message{
		Key:       []byte(e.CorrelationID),
		Value:     data,
		Headers:   []kafka.Header{{Key: "Idempotency-Key", Value: []byte(e.IdempotencyKey)}},
		Topic:     w.topic,
		Time:      time.Now().UTC(),
	}

	return w.producer.WriteMessages(ctx, msg)
}

// markPublished marks an outbox event as published.
func (w *Worker) markPublished(ctx context.Context, outboxID string) error {
	const query = `UPDATE outbox SET published_at = NOW() WHERE outbox_id = $1;`
	_, err := w.pool.Exec(ctx, query, outboxID)
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Ensure the Envelope type is accessible. We need to export it from events package.
var _ = events.Envelope{}