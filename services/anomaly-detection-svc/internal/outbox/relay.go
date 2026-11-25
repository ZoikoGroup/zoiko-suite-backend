// Package outbox publishes rows of the anomaly_outbox table to Kafka.
//
// Store commands write their domain event (AI.AnomalyDetected,
// AI.AnomalyDispositioned) into anomaly_outbox in the same transaction as
// the state change. The Relay below runs in the background, picks up
// unpublished rows in insertion order, publishes them, and stamps
// published_at. A failed publish is retried with exponential backoff, so an
// event is delivered at least once and never lost when Kafka is down.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/anomaly-detection-svc/internal/events"
)

const (
	pollInterval   = time.Second
	batchSize      = 50
	maxBackoffSecs = 300
)

// Relay moves outbox rows to the event bus.
type Relay struct {
	pool      *pgxpool.Pool
	publisher events.Publisher
	logger    *zap.Logger
}

func NewRelay(pool *pgxpool.Pool, publisher events.Publisher, logger *zap.Logger) *Relay {
	return &Relay{pool: pool, publisher: publisher, logger: logger}
}

// Run polls until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		published, err := r.PublishBatch(ctx)
		if err != nil {
			r.logger.Warn("outbox relay batch failed", zap.Error(err))
		}
		// A full batch means there may be more waiting: go again at once.
		if published == batchSize {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type outboxRow struct {
	outboxID      int64
	eventID       string
	eventType     string
	aggregateID   string
	tenantID      string
	legalEntityID string
	actorID       string
	correlationID string
	payload       []byte
	occurredAt    time.Time
	attempts      int
}

// PublishBatch publishes up to batchSize ready rows and returns how many were
// published. Rows are locked with SKIP LOCKED so several replicas can run a
// relay without publishing the same row twice concurrently.
func (r *Relay) PublishBatch(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin outbox batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// anomaly_outbox is FORCE RLS. The relay is not a request and drains
	// every tenant's backlog, so it names itself with app.outbox_relay and
	// is admitted by the policy's explicit relay disjunct (000004).
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return 0, fmt.Errorf("set outbox relay scope: %w", err)
	}

	rows, err := r.claimReadyRows(ctx, tx)
	if err != nil {
		return 0, err
	}

	published := 0
	for _, row := range rows {
		publishErr := r.publishRow(ctx, row)
		if publishErr != nil {
			if err := r.scheduleRetry(ctx, tx, row, publishErr); err != nil {
				return published, err
			}
			// Stop at the first failure so events of one aggregate are
			// never published out of order.
			break
		}
		if _, err := tx.Exec(ctx,
			`UPDATE anomaly_outbox SET published_at = NOW(), last_error = NULL WHERE outbox_id = $1`,
			row.outboxID); err != nil {
			return published, fmt.Errorf("mark outbox row %d published: %w", row.outboxID, err)
		}
		published++
	}

	if err := tx.Commit(ctx); err != nil {
		return published, fmt.Errorf("commit outbox batch: %w", err)
	}
	return published, nil
}

func (r *Relay) claimReadyRows(ctx context.Context, tx pgx.Tx) ([]outboxRow, error) {
	result, err := tx.Query(ctx, `
		SELECT outbox_id, event_id, event_type, aggregate_id, tenant_id, legal_entity_id,
		       actor_id, correlation_id, payload, occurred_at, attempts
		FROM anomaly_outbox
		WHERE published_at IS NULL AND available_at <= NOW()
		ORDER BY outbox_id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, batchSize)
	if err != nil {
		return nil, fmt.Errorf("claim outbox rows: %w", err)
	}
	defer result.Close()

	var rows []outboxRow
	for result.Next() {
		var row outboxRow
		if err := result.Scan(&row.outboxID, &row.eventID, &row.eventType, &row.aggregateID, &row.tenantID,
			&row.legalEntityID, &row.actorID, &row.correlationID, &row.payload, &row.occurredAt, &row.attempts); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read outbox rows: %w", err)
	}
	return rows, nil
}

func (r *Relay) publishRow(ctx context.Context, row outboxRow) error {
	var payload any
	if err := json.Unmarshal(row.payload, &payload); err != nil {
		return fmt.Errorf("decode outbox payload: %w", err)
	}
	return r.publisher.Publish(ctx, events.PublishParams{
		EventID:       row.eventID,
		OccurredAt:    row.occurredAt,
		EventType:     row.eventType,
		SubjectID:     row.aggregateID,
		TenantID:      row.tenantID,
		LegalEntityID: row.legalEntityID,
		ActorID:       row.actorID,
		CorrelationID: row.correlationID,
		Payload:       payload,
	})
}

// scheduleRetry pushes the row's next attempt out by 2^attempts seconds,
// capped at maxBackoffSecs.
func (r *Relay) scheduleRetry(ctx context.Context, tx pgx.Tx, row outboxRow, cause error) error {
	nextAttempt := row.attempts + 1
	backoffSeconds := math.Min(math.Pow(2, float64(nextAttempt)), maxBackoffSecs)
	_, err := tx.Exec(ctx, `
		UPDATE anomaly_outbox
		SET attempts = attempts + 1,
		    last_error = $2,
		    available_at = NOW() + make_interval(secs => $3)
		WHERE outbox_id = $1`,
		row.outboxID, cause.Error(), backoffSeconds)
	if err != nil {
		return fmt.Errorf("schedule outbox retry for row %d: %w", row.outboxID, err)
	}
	r.logger.Warn("outbox publish failed; will retry",
		zap.String("event_id", row.eventID), zap.String("event_type", row.eventType),
		zap.Float64("retry_in_seconds", backoffSeconds), zap.Error(cause))
	return nil
}
