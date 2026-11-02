package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/decision-support-svc/internal/domain"
)

// maxOutboxAttempts is how many failed deliveries a message gets before it
// is parked for an operator to look at. Parked rows are kept, never deleted.
const maxOutboxAttempts = 20

// ErrPermanentDelivery wraps a delivery failure that will never succeed on
// retry; the relay parks the message immediately.
var ErrPermanentDelivery = errors.New("permanent outbox delivery failure")

func insertOutboxMessages(ctx context.Context, tx pgx.Tx, messages []domain.OutboxMessage) error {
	for _, m := range messages {
		_, err := tx.Exec(ctx, `
			INSERT INTO outbox (outbox_id, tenant_id, destination, message_type, message_key, payload)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			m.OutboxID, m.TenantID, m.Destination, m.MessageType, m.MessageKey, []byte(m.Payload),
		)
		if err != nil {
			return fmt.Errorf("insert outbox message %s: %w", m.MessageType, err)
		}
	}
	return nil
}

// DeliverPendingOutbox claims up to limit undelivered messages (oldest
// first, skipping rows another relay instance holds), calls deliver for
// each, and records the outcome. It returns how many were delivered.
//
// The relay has no tenant, so instead of app.tenant_id it sets
// app.outbox_relay = 'on', which the outbox RLS policy admits.
func (s *PgStore) DeliverPendingOutbox(ctx context.Context, limit int, deliver func(context.Context, domain.OutboxMessage) error) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin outbox transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'on', true)"); err != nil {
		return 0, fmt.Errorf("set outbox relay context: %w", err)
	}

	pending, err := claimPending(ctx, tx, limit)
	if err != nil {
		return 0, err
	}

	delivered := 0
	for _, message := range pending {
		deliveryErr := deliver(ctx, message)
		if err := recordDeliveryOutcome(ctx, tx, message.OutboxID, deliveryErr); err != nil {
			return delivered, err
		}
		if deliveryErr == nil {
			delivered++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return delivered, fmt.Errorf("commit outbox transaction: %w", err)
	}
	return delivered, nil
}

func claimPending(ctx context.Context, tx pgx.Tx, limit int) ([]domain.OutboxMessage, error) {
	rows, err := tx.Query(ctx, `
		SELECT outbox_id, tenant_id, destination, message_type, message_key, payload
		FROM outbox
		WHERE delivered_at IS NULL AND parked_at IS NULL
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox messages: %w", err)
	}
	defer rows.Close()

	var pending []domain.OutboxMessage
	for rows.Next() {
		var m domain.OutboxMessage
		if err := rows.Scan(&m.OutboxID, &m.TenantID, &m.Destination, &m.MessageType, &m.MessageKey, &m.Payload); err != nil {
			return nil, err
		}
		pending = append(pending, m)
	}
	return pending, rows.Err()
}

// recordDeliveryOutcome marks a message delivered, or counts the failure and
// parks it when it is permanent or out of attempts.
func recordDeliveryOutcome(ctx context.Context, tx pgx.Tx, outboxID string, deliveryErr error) error {
	if deliveryErr == nil {
		_, err := tx.Exec(ctx, "UPDATE outbox SET delivered_at = now(), last_error = NULL WHERE outbox_id = $1", outboxID)
		return err
	}

	parkNow := errors.Is(deliveryErr, ErrPermanentDelivery)
	_, err := tx.Exec(ctx, `
		UPDATE outbox SET
			attempts = attempts + 1,
			last_error = $2,
			parked_at = CASE WHEN $3 OR attempts + 1 >= $4 THEN now() ELSE NULL END
		WHERE outbox_id = $1`,
		outboxID, deliveryErr.Error(), parkNow, maxOutboxAttempts)
	return err
}
