package store

import (
	"context"
	"fmt"
	"time"
)

// OutboxEvent is a domain event written in the same transaction as the state
// change it describes (transactional outbox). The relay publishes it later;
// if the transaction rolls back, the event never existed.
type OutboxEvent struct {
	ID            string
	TenantID      string
	EventType     string
	AggregateType string
	AggregateID   string
	Envelope      []byte // the complete JSON message published to Kafka
	CreatedAt     time.Time
	Attempts      int
}

// EnqueueEvent inserts an event into the outbox inside the tenant transaction.
func (q *TenantQueries) EnqueueEvent(ctx context.Context, event OutboxEvent) error {
	_, err := q.tx.Exec(ctx, `
		INSERT INTO risk_outbox_events (id, tenant_id, event_type, aggregate_type, aggregate_id, envelope, created_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb, $7)`,
		event.ID, q.TenantID, event.EventType, event.AggregateType, event.AggregateID, string(event.Envelope), event.CreatedAt)
	if err != nil {
		return fmt.Errorf("enqueue outbox event: %w", err)
	}
	return nil
}

// OutboxBatch is a set of claimed, unpublished events plus the transaction
// that holds their row locks. Call Finish exactly once.
type OutboxBatch struct {
	Events []OutboxEvent
	finish func(ctx context.Context, published []string, failed map[string]string) error
}

// Finish marks published events as delivered and schedules a retry for the
// failed ones (with exponential backoff capped at five minutes), then commits.
func (b *OutboxBatch) Finish(ctx context.Context, published []string, failed map[string]string) error {
	return b.finish(ctx, published, failed)
}

// ClaimOutboxBatch locks up to limit due, unpublished events across all
// tenants. FOR UPDATE SKIP LOCKED lets several replicas relay concurrently
// without publishing the same row twice.
func (s *Store) ClaimOutboxBatch(ctx context.Context, limit int) (*OutboxBatch, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}

	// The outbox is under FORCE RLS (migration 000003); the relay is the one
	// caller admitted across tenants, via app.outbox_relay.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT id::text, tenant_id, event_type, aggregate_type, aggregate_id, envelope::text, created_at, attempts
		FROM risk_outbox_events
		WHERE published_at IS NULL AND next_attempt_at <= NOW()
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}

	var events []OutboxEvent
	for rows.Next() {
		var event OutboxEvent
		if err := rows.Scan(&event.ID, &event.TenantID, &event.EventType, &event.AggregateType,
			&event.AggregateID, &event.Envelope, &event.CreatedAt, &event.Attempts); err != nil {
			rows.Close()
			_ = tx.Rollback(ctx)
			return nil, err
		}
		events = append(events, event)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}

	batch := &OutboxBatch{Events: events}
	batch.finish = func(ctx context.Context, published []string, failed map[string]string) error {
		defer func() { _ = tx.Rollback(ctx) }()
		for _, id := range published {
			if _, err := tx.Exec(ctx, `UPDATE risk_outbox_events
				SET published_at = NOW(), attempts = attempts + 1, last_error = NULL
				WHERE id = $1::uuid`, id); err != nil {
				return err
			}
		}
		for id, reason := range failed {
			if _, err := tx.Exec(ctx, `UPDATE risk_outbox_events
				SET attempts = attempts + 1, last_error = $2,
				    next_attempt_at = NOW() + LEAST(INTERVAL '5 minutes', INTERVAL '1 second' * power(2, attempts))
				WHERE id = $1::uuid`, id, reason); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	return batch, nil
}

// CountPendingOutboxEvents is used by tests and diagnostics. It runs in the
// tenant's RLS scope, so it can only ever count that tenant's events.
func (s *Store) CountPendingOutboxEvents(ctx context.Context, tenantID string) (int, error) {
	var count int
	err := s.InTenantTx(ctx, tenantID, func(q *TenantQueries) error {
		return q.tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM risk_outbox_events WHERE tenant_id = $1 AND published_at IS NULL`, tenantID).Scan(&count)
	})
	return count, err
}
