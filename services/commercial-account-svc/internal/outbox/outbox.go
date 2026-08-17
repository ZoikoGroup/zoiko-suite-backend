// Package outbox implements the transactional-outbox pattern (doc7
// backlog item 32): an event is written to outbox_events inside the SAME
// database transaction as the business write it describes, so the two
// can never diverge — either both commit or neither does. A separate
// Relay then polls unpublished rows and publishes them to Kafka,
// decoupling "the fact is durably true" from "Kafka happened to be
// reachable at that exact moment."
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
)

// Event is one row to be written to outbox_events.
type Event struct {
	OutboxEventID string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       interface{}
	TenantID      *string
}

// Insert writes e into outbox_events using tx — the caller's OWN
// transaction, the same one performing the business write. This is the
// entire point: Insert must never open its own transaction, or the
// atomicity guarantee this package exists for is lost.
func Insert(ctx context.Context, tx pgx.Tx, e Event) error {
	if e.OutboxEventID == "" {
		e.OutboxEventID = uuid.NewString()
	}
	payloadJSON, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type, payload, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, e.OutboxEventID, e.AggregateType, e.AggregateID, e.EventType, payloadJSON, e.TenantID)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// storedEvent is one row read back for relaying.
type storedEvent struct {
	OutboxEventID string
	EventType     string
	AggregateID   string
	TenantID      *string
	Payload       json.RawMessage
	Attempts      int
}

// Publisher is the narrow interface Relay depends on — matches
// events.Publisher's shape without importing that package, so this file
// stays copyable to any other service unchanged.
type Publisher interface {
	Publish(ctx context.Context, eventType, entityID, tenantID string, payload interface{}) error
}

// Relay polls outbox_events for unpublished rows and publishes them.
// Deliberately simple (fixed-interval polling, not LISTEN/NOTIFY): doc7
// item 32's requirement is "an event is never silently dropped," not
// "an event is delivered with sub-second latency."
type Relay struct {
	pool      PgxPool
	publisher Publisher
	interval  time.Duration
	batchSize int
	log       *zap.Logger
}

// PgxPool is the subset of *pgxpool.Pool the relay needs.
type PgxPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func NewRelay(pool PgxPool, publisher Publisher, interval time.Duration, batchSize int, log *zap.Logger) *Relay {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	return &Relay{pool: pool, publisher: publisher, interval: interval, batchSize: batchSize, log: log}
}

// Start runs the relay loop until ctx is cancelled. Intended to be run in
// its own goroutine from main.go.
func (r *Relay) Start(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.relayOnce(ctx)
		}
	}
}

func (r *Relay) relayOnce(ctx context.Context) {
	rows, err := r.pool.Query(ctx, `
		SELECT outbox_event_id, event_type, aggregate_id, tenant_id, payload, publish_attempts
		FROM outbox_events
		WHERE published_at IS NULL
		ORDER BY created_at ASC
		LIMIT $1
	`, r.batchSize)
	if err != nil {
		r.log.Error("outbox relay: poll failed", zap.Error(err))
		return
	}
	var pending []storedEvent
	for rows.Next() {
		var e storedEvent
		var tenantID *string
		if scanErr := rows.Scan(&e.OutboxEventID, &e.EventType, &e.AggregateID, &tenantID, &e.Payload, &e.Attempts); scanErr != nil {
			r.log.Error("outbox relay: scan failed", zap.Error(scanErr))
			rows.Close()
			return
		}
		e.TenantID = tenantID
		pending = append(pending, e)
	}
	rows.Close()

	for _, e := range pending {
		tenantID := ""
		if e.TenantID != nil {
			tenantID = *e.TenantID
		}
		var payload interface{}
		_ = json.Unmarshal(e.Payload, &payload)

		if pubErr := r.publisher.Publish(ctx, e.EventType, e.AggregateID, tenantID, payload); pubErr != nil {
			r.markFailed(ctx, e.OutboxEventID, pubErr)
			continue
		}
		r.markPublished(ctx, e.OutboxEventID)
	}
}

func (r *Relay) markPublished(ctx context.Context, outboxEventID string) {
	_, err := r.pool.Exec(ctx, `UPDATE outbox_events SET published_at = NOW() WHERE outbox_event_id = $1`, outboxEventID)
	if err != nil {
		r.log.Error("outbox relay: failed to mark published", zap.String("outbox_event_id", outboxEventID), zap.Error(err))
	}
}

func (r *Relay) markFailed(ctx context.Context, outboxEventID string, publishErr error) {
	_, err := r.pool.Exec(ctx, `
		UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = $2
		WHERE outbox_event_id = $1
	`, outboxEventID, publishErr.Error())
	if err != nil {
		r.log.Error("outbox relay: failed to record publish failure", zap.String("outbox_event_id", outboxEventID), zap.Error(err))
	}
	r.log.Warn("outbox relay: publish failed, will retry next tick", zap.String("outbox_event_id", outboxEventID), zap.Error(publishErr))
}
