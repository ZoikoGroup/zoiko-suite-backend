package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// EnqueueTx inserts an event into event_outbox within the provided database transaction.
// The event is committed atomically with the domain state mutation (§19.1).
func EnqueueTx(ctx context.Context, tx pgx.Tx, params PublishParams) error {
	evt := Event{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     params.EventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "privacy-purpose-registry-svc",
		EntityID:      params.EntityID,
		TenantID:      params.TenantID,
		ActorID:       params.ActorID,
		CorrelationID: params.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       params.Payload,
	}
	payloadBytes, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("outbox.EnqueueTx marshal: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO event_outbox (outbox_event_id, event_type, tenant_id, entity_id, actor_id, correlation_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		evt.EventID, params.EventType, params.TenantID, params.EntityID, params.ActorID, params.CorrelationID, payloadBytes,
	)
	if err != nil {
		return fmt.Errorf("outbox.EnqueueTx insert: %w", err)
	}
	return nil
}

// OutboxRecord models one row in event_outbox.
type OutboxRecord struct {
	EventID       string
	EventType     string
	TenantID      string
	EntityID      string
	ActorID       string
	CorrelationID string
	Payload       []byte
	CreatedAt     time.Time
	PublishedAt   *time.Time
	Attempts      int
	NextAttemptAt time.Time
	LastError     *string
}

// OutboxRelay polls pending outbox events and dispatches them to Kafka with at-least-once delivery.
type OutboxRelay struct {
	pool   *pgxpool.Pool
	writer MessageWriter
	topic  string
	logger *zap.Logger
}

func NewOutboxRelay(pool *pgxpool.Pool, writer MessageWriter, topic string, logger *zap.Logger) *OutboxRelay {
	return &OutboxRelay{
		pool:   pool,
		writer: writer,
		topic:  topic,
		logger: logger,
	}
}

// DrainPending drains up to limit pending events. Safe to call concurrently or directly from tests.
func (r *OutboxRelay) DrainPending(ctx context.Context, limit int) (int, error) {
	if r.pool == nil || r.writer == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 50
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin outbox drain tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Enable outbox relay capability bypass for multi-tenant drainage
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return 0, fmt.Errorf("set app.outbox_relay: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT outbox_event_id, event_type, entity_id, tenant_id, correlation_id, payload, attempts
		FROM event_outbox
		WHERE published_at IS NULL AND next_attempt_at <= NOW()
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`,
		limit,
	)
	if err != nil {
		return 0, fmt.Errorf("query pending outbox: %w", err)
	}

	type pendingItem struct {
		id       string
		typ      string
		entityID string
		tenantID string
		corrID   string
		payload  []byte
		attempts int
	}
	var items []pendingItem
	for rows.Next() {
		var it pendingItem
		if err := rows.Scan(&it.id, &it.typ, &it.entityID, &it.tenantID, &it.corrID, &it.payload, &it.attempts); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()

	successCount := 0
	for _, it := range items {
		msg := kafka.Message{
			Key:   []byte(it.entityID),
			Value: it.payload,
		}
		writeErr := r.writer.WriteMessages(ctx, msg)
		if writeErr == nil {
			tag, err := tx.Exec(ctx, `
				UPDATE event_outbox
				SET published_at = NOW(), last_error = NULL
				WHERE outbox_event_id = $1 AND published_at IS NULL`,
				it.id,
			)
			// RowsAffected is checked because a zero-row UPDATE is not an error
			// to pgx: under FORCE ROW LEVEL SECURITY a bookkeeping write that
			// the policy filters out succeeds silently and the event would be
			// re-sent forever. The tx carries app.outbox_relay, so exactly one
			// row must change.
			switch {
			case err != nil:
				r.logger.Error("outbox mark published error", zap.String("id", it.id), zap.Error(err))
			case tag.RowsAffected() != 1:
				r.logger.Error("outbox mark published affected an unexpected number of rows",
					zap.String("id", it.id), zap.Int64("rows", tag.RowsAffected()))
			default:
				successCount++
			}
		} else {
			r.logger.Warn("outbox dispatch failed — will retry",
				zap.String("id", it.id),
				zap.String("type", it.typ),
				zap.Int("attempts", it.attempts+1),
				zap.Error(writeErr),
			)
			backoffSec := 1 << it.attempts
			if backoffSec > 300 {
				backoffSec = 300
			}
			if _, execErr := tx.Exec(ctx, `
				UPDATE event_outbox
				SET attempts = attempts + 1,
				    last_error = $2,
				    next_attempt_at = NOW() + ($3 * interval '1 second')
				WHERE outbox_event_id = $1`,
				it.id, writeErr.Error(), backoffSec,
			); execErr != nil {
				r.logger.Error("outbox update failure record error", zap.String("id", it.id), zap.Error(execErr))
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit outbox drain: %w", err)
	}
	return successCount, nil
}

// Start runs the relay background worker loop until ctx is cancelled.
func (r *OutboxRelay) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, _ = r.DrainPending(drainCtx, 50)
			cancel()
		}
	}
}
