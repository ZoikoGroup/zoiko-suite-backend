package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Event is the unpersisted outbox record created within a database transaction.
type Event struct {
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       any
	CorrelationID string
	TenantID      *string
}

// StoredEvent represents a persisted outbox record fetched by the relay worker.
type StoredEvent struct {
	ID              string
	AggregateType   string
	AggregateID     string
	EventType       string
	Payload         []byte
	CorrelationID   *string
	TenantID        *string
	PublishAttempts int
}

// Insert inserts an outbox event within the provided database transaction.
// It ensures that domain state changes and the outbox event are committed atomically.
func Insert(ctx context.Context, tx pgx.Tx, e Event) error {
	if tx == nil {
		return errors.New("cannot insert outbox event: transaction is nil")
	}

	payloadBytes, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}

	var corrID *string
	if e.CorrelationID != "" {
		corrID = &e.CorrelationID
	}

	const query = `
		INSERT INTO outbox_events (
			aggregate_type, aggregate_id, event_type, payload, correlation_id, tenant_id
		) VALUES ($1, $2, $3, $4, $5, $6);
	`
	_, err = tx.Exec(ctx, query,
		e.AggregateType,
		e.AggregateID,
		e.EventType,
		payloadBytes,
		corrID,
		e.TenantID,
	)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// Publisher is the interface required by Relay to deliver messages to Kafka.
type Publisher interface {
	PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error
}

// Relay is a background worker that polls outbox_events and publishes them to Kafka.
type Relay struct {
	pool      *pgxpool.Pool
	publisher Publisher
	interval  time.Duration
	batchSize int
	logger    *zap.Logger
}

// NewRelay constructs an outbox Relay worker.
func NewRelay(pool *pgxpool.Pool, pub Publisher, interval time.Duration, batchSize int, logger *zap.Logger) *Relay {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	return &Relay{
		pool:      pool,
		publisher: pub,
		interval:  interval,
		batchSize: batchSize,
		logger:    logger,
	}
}

// Start runs the relay polling loop until ctx is cancelled.
func (r *Relay) Start(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("outbox relay stopping")
			return
		case <-ticker.C:
			count, err := r.RelayOnce(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				r.logger.Error("outbox relay error", zap.Error(err))
			} else if count > 0 {
				r.logger.Debug("outbox relay processed events", zap.Int("count", count))
			}
		}
	}
}

// RelayOnce fetches a batch of unpublished events with SKIP LOCKED, publishes them,
// and marks them published or records the error.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	if r.pool == nil {
		return 0, nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin relay tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const selectQuery = `
		SELECT id, aggregate_type, aggregate_id, event_type, payload, correlation_id, tenant_id, publish_attempts
		FROM outbox_events
		WHERE published_at IS NULL
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED;
	`
	rows, err := tx.Query(ctx, selectQuery, r.batchSize)
	if err != nil {
		return 0, fmt.Errorf("query outbox events: %w", err)
	}
	defer rows.Close()

	var events []StoredEvent
	for rows.Next() {
		var se StoredEvent
		if err := rows.Scan(
			&se.ID,
			&se.AggregateType,
			&se.AggregateID,
			&se.EventType,
			&se.Payload,
			&se.CorrelationID,
			&se.TenantID,
			&se.PublishAttempts,
		); err != nil {
			return 0, fmt.Errorf("scan outbox event: %w", err)
		}
		events = append(events, se)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate outbox rows: %w", err)
	}
	rows.Close()

	if len(events) == 0 {
		return 0, nil
	}

	for _, se := range events {
		pubErr := r.publisher.PublishOutbox(ctx, se.ID, se.AggregateID, se.Payload)
		if pubErr != nil {
			r.logger.Warn("outbox publish failed, will retry",
				zap.String("outbox_id", se.ID),
				zap.String("event_type", se.EventType),
				zap.Error(pubErr),
			)
			_, _ = tx.Exec(ctx, `
				UPDATE outbox_events
				SET publish_attempts = publish_attempts + 1,
				    last_error = $2
				WHERE id = $1;
			`, se.ID, pubErr.Error())
		} else {
			_, _ = tx.Exec(ctx, `
				UPDATE outbox_events
				SET published_at = now()
				WHERE id = $1;
			`, se.ID)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit relay tx: %w", err)
	}

	return len(events), nil
}
