package store

import (
	"context"
	"encoding/json"
	"fmt"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
)

// PendingOutboxEvent is an outbox row waiting to be published.
type PendingOutboxEvent struct {
	domain.OutboxEvent
	PayloadJSON json.RawMessage
}

// PublishOutboxBatch publishes up to limit unpublished outbox rows.
//
// The rows are locked FOR UPDATE SKIP LOCKED so several service instances
// can relay concurrently without publishing the same row twice. For each
// row, publish is called; success sets published_at, failure increments
// publish_attempts and records last_error (the row is retried next poll).
// Returns how many rows were published successfully.
func (s *PgStore) PublishOutboxBatch(ctx context.Context, limit int, publish func(context.Context, PendingOutboxEvent) error) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin outbox transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// outbox_events is under FORCE RLS (migration 000004); the relay is the
	// one caller admitted across tenants, via app.outbox_relay.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return 0, fmt.Errorf("set outbox relay scope: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT outbox_event_id::text, aggregate_type, aggregate_id, event_type, tenant_id,
		       legal_entity_id, actor_id, correlation_id, payload, created_at
		FROM outbox_events
		WHERE published_at IS NULL
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, fmt.Errorf("select pending outbox events: %w", err)
	}
	var pending []PendingOutboxEvent
	for rows.Next() {
		var event PendingOutboxEvent
		if err := rows.Scan(&event.OutboxEventID, &event.AggregateType, &event.AggregateID, &event.EventType,
			&event.TenantID, &event.LegalEntityID, &event.ActorID, &event.CorrelationID,
			&event.PayloadJSON, &event.CreatedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbox event: %w", err)
		}
		pending = append(pending, event)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate outbox events: %w", err)
	}

	published := 0
	for _, event := range pending {
		if publishErr := publish(ctx, event); publishErr != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = $2
				WHERE outbox_event_id = $1`, event.OutboxEventID, publishErr.Error()); err != nil {
				return published, fmt.Errorf("record outbox publish failure: %w", err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE outbox_events SET published_at = now(), publish_attempts = publish_attempts + 1, last_error = NULL
			WHERE outbox_event_id = $1`, event.OutboxEventID); err != nil {
			return published, fmt.Errorf("mark outbox event published: %w", err)
		}
		published++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit outbox batch: %w", err)
	}
	return published, nil
}
