// Package outbox implements the transactional outbox (Enterprise Event
// Catalogue §6): a domain event is written to outbox_events in the SAME
// database transaction as the business change, and a background Relay
// publishes committed rows to Kafka afterwards.
//
// This guarantees "event emitted only after commit" and "no lost event": if the
// transaction rolls back the event row disappears with it, and if Kafka is down
// the row stays Pending and is retried with the same event_id.
package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/forecasting-svc/internal/events"
)

// MaxPublishAttempts is how many times the relay retries a row before marking
// it Failed for operator attention.
const MaxPublishAttempts = 10

// Record describes one event to enqueue.
type Record struct {
	AggregateType    string // "forecast", "forecast_assist_job", "forecast_model_release"
	AggregateID      string
	AggregateVersion int
	Params           events.PublishParams
}

// Enqueue inserts the event into outbox_events using the caller's transaction.
// It must be called inside the transaction that performs the business change;
// the transaction must already have declared app.tenant_id.
func Enqueue(ctx context.Context, tx pgx.Tx, record Record) error {
	event := events.BuildEvent(record.Params)
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal outbox event: %w", err)
	}
	digest := sha256.Sum256(payload)

	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (
			outbox_id, event_id, event_type, tenant_id,
			aggregate_type, aggregate_id, aggregate_version, payload, payload_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		uuid.New(), event.EventID, event.EventType, event.TenantID,
		record.AggregateType, record.AggregateID, record.AggregateVersion, payload, hex.EncodeToString(digest[:]))
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// RawPublisher is what the relay needs from events.Publisher.
type RawPublisher interface {
	PublishRaw(ctx context.Context, partitionKey string, value []byte) error
}

// Relay polls Pending outbox rows and publishes them.
type Relay struct {
	pool      *pgxpool.Pool
	publisher RawPublisher
	interval  time.Duration
	batchSize int
	logger    *zap.Logger
}

func NewRelay(pool *pgxpool.Pool, publisher RawPublisher, interval time.Duration, batchSize int, logger *zap.Logger) *Relay {
	return &Relay{pool: pool, publisher: publisher, interval: interval, batchSize: batchSize, logger: logger}
}

// Run polls until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			published, err := r.RelayOnce(ctx)
			if err != nil {
				r.logger.Warn("outbox relay cycle failed", zap.Error(err))
			} else if published > 0 {
				r.logger.Info("outbox relay published events", zap.Int("count", published))
			}
		}
	}
}

type pendingRow struct {
	outboxID string
	tenantID string
	attempts int
	payload  []byte
}

// RelayOnce publishes one batch and returns how many rows were published.
//
// Steps:
//  1. Begin a transaction and declare app.outbox_relay = 'on' (the RLS policy
//     on outbox_events admits the relay only with this flag).
//  2. Lock up to batchSize due Pending rows with FOR UPDATE SKIP LOCKED so
//     several replicas never publish the same row concurrently.
//  3. Publish each row; mark it Published, or schedule a retry with backoff.
//  4. Commit.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.outbox_relay', 'on', true)`); err != nil {
		return 0, err
	}

	rows, err := tx.Query(ctx, `
		SELECT outbox_id::text, tenant_id, attempt_count, payload::text
		FROM outbox_events
		WHERE publish_state = 'Pending' AND next_attempt_at <= NOW()
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, r.batchSize)
	if err != nil {
		return 0, err
	}
	var batch []pendingRow
	for rows.Next() {
		var row pendingRow
		var payloadText string
		if err := rows.Scan(&row.outboxID, &row.tenantID, &row.attempts, &payloadText); err != nil {
			rows.Close()
			return 0, err
		}
		row.payload = []byte(payloadText)
		batch = append(batch, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	published := 0
	for _, row := range batch {
		publishCtx, cancelPublish := context.WithTimeout(ctx, 10*time.Second)
		publishErr := r.publisher.PublishRaw(publishCtx, row.tenantID, row.payload)
		cancelPublish()
		if publishErr == nil {
			if _, err := tx.Exec(ctx, `
				UPDATE outbox_events
				SET publish_state = 'Published', published_at = NOW(), attempt_count = attempt_count + 1, last_error = NULL
				WHERE outbox_id = $1`, row.outboxID); err != nil {
				return published, err
			}
			published++
			continue
		}
		if err := r.scheduleRetry(ctx, tx, row, publishErr); err != nil {
			return published, err
		}
	}
	return published, tx.Commit(ctx)
}

// scheduleRetry records a failed attempt with exponential backoff, capped at
// five minutes; after MaxPublishAttempts the row is marked Failed.
func (r *Relay) scheduleRetry(ctx context.Context, tx pgx.Tx, row pendingRow, publishErr error) error {
	nextAttempt := row.attempts + 1
	newState := "Pending"
	if nextAttempt >= MaxPublishAttempts {
		newState = "Failed"
	}
	backoff := time.Duration(1<<uint(minInt(nextAttempt, 8))) * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	_, err := tx.Exec(ctx, `
		UPDATE outbox_events
		SET publish_state = $2, attempt_count = $3, last_error = $4, next_attempt_at = NOW() + $5::interval
		WHERE outbox_id = $1`,
		row.outboxID, newState, nextAttempt, publishErr.Error(), fmt.Sprintf("%d seconds", int(backoff.Seconds())))
	return err
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
