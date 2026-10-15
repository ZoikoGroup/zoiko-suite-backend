// Package outbox provides the background worker that publishes events from
// the transactional outbox table to Kafka.
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Worker polls the outbox table and publishes events to Kafka.
type Worker struct {
	pool        *pgxpool.Pool
	kafkaWriter *kafka.Writer
	log         *zap.Logger
	pollInterval time.Duration
	batchSize   int
	stopCh      chan struct{}
}

// NewWorker creates a new outbox worker.
func NewWorker(pool *pgxpool.Pool, kafkaWriter *kafka.Writer, log *zap.Logger) *Worker {
	return &Worker{
		pool:         pool,
		kafkaWriter:  kafkaWriter,
		log:          log,
		pollInterval: 5 * time.Second,
		batchSize:    100,
		stopCh:       make(chan struct{}),
	}
}

// Start begins the outbox worker loop.
func (w *Worker) Start(ctx context.Context) {
	w.log.Info("outbox worker starting")
	go w.run(ctx)
}

// Stop signals the worker to stop.
func (w *Worker) Stop() {
	close(w.stopCh)
}

func (w *Worker) run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("outbox worker stopped (context cancelled)")
			return
		case <-w.stopCh:
			w.log.Info("outbox worker stopped (signal received)")
			return
		case <-ticker.C:
			w.processBatch(ctx)
		}
	}
}

func (w *Worker) processBatch(ctx context.Context) {
	// Claim unpublished events
	rows, err := w.claimEvents(ctx)
	if err != nil {
		w.log.Error("outbox: failed to claim events", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}

	var publishedIDs []string
	var failedIDs []string
	var failedErr error

	for _, row := range rows {
		var payload map[string]any
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			w.log.Error("outbox: failed to unmarshal payload",
				zap.String("outbox_id", row.OutboxID), zap.Error(err))
			failedIDs = append(failedIDs, row.OutboxID)
			failedErr = err
			continue
		}

		// Reconstruct envelope
		env := EventEnvelope{
			EventID:       row.OutboxID,
			EventType:     row.EventType,
			EventVersion:  "1.0",
			EmittedAt:     row.CreatedAt,
			SchemaVersion: "1.0",
			SourceService: "evidence-requirements-svc",
			TenantID:      payload["tenant_id"].(string),
			LegalEntityID: payload["legal_entity_id"].(string),
			ActorID:       payload["evaluated_for_principal_id"].(string),
			CorrelationID: payload["correlation_id"].(string),
			Payload:       row.Payload,
		}
		envBytes, err := json.Marshal(env)
		if err != nil {
			w.log.Error("outbox: failed to marshal envelope",
				zap.String("outbox_id", row.OutboxID), zap.Error(err))
			failedIDs = append(failedIDs, row.OutboxID)
			failedErr = err
			continue
		}

		// Publish to Kafka
		msg := kafka.Message{
			Key:   []byte(row.AggregateID),
			Value: envBytes,
		}
		if err := w.kafkaWriter.WriteMessages(ctx, msg); err != nil {
			w.log.Error("outbox: failed to publish to Kafka",
				zap.String("outbox_id", row.OutboxID), zap.Error(err))
			failedIDs = append(failedIDs, row.OutboxID)
			failedErr = err
			continue
		}

		publishedIDs = append(publishedIDs, row.OutboxID)
	}

	// Mark published events
	if len(publishedIDs) > 0 {
		if err := w.markPublished(ctx, publishedIDs); err != nil {
			w.log.Error("outbox: failed to mark published", zap.Error(err))
		}
	}

	// Mark failed events (increment attempt count, record error)
	if len(failedIDs) > 0 && failedErr != nil {
		if err := w.markFailed(ctx, failedIDs, failedErr.Error()); err != nil {
			w.log.Error("outbox: failed to mark failed", zap.Error(err))
		}
	}
}

type outboxRow struct {
	OutboxID      string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	CreatedAt     time.Time
}

func (w *Worker) claimEvents(ctx context.Context) ([]outboxRow, error) {
	rows, err := w.pool.Query(ctx, `SELECT * FROM claim_outbox_events($1)`, w.batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.OutboxID, &r.AggregateType, &r.AggregateID, &r.EventType, &r.Payload, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (w *Worker) markPublished(ctx context.Context, ids []string) error {
	_, err := w.pool.Exec(ctx, `SELECT mark_outbox_published($1)`, ids)
	return err
}

func (w *Worker) markFailed(ctx context.Context, ids []string, errMsg string) error {
	_, err := w.pool.Exec(ctx, `SELECT mark_outbox_failed($1, $2)`, ids, errMsg)
	return err
}

// EventEnvelope is the domain event envelope (copied from events/publisher.go)
type EventEnvelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}