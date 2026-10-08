package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
	"zoiko.io/notification-svc/internal/ncd"
)

// ZS-SVC-Y-001 NCD-03 6.1 and 6.6: server-authoritative timing and cancellation (migration
// 000028). Both hang on one rule: only a communication that is QUEUED (PENDING, scheduled, and
// not being submitted) may be cancelled or expired. One that is claimed by a worker, being
// submitted, or concluded is left alone, so a withdrawal can never race an in-flight provider
// call into a "cancelled" message that was actually sent.

// CancelNotification withdraws a queued communication, recording who and why.
func (s *PgStore) CancelNotification(ctx context.Context, id, tenantID, actor, reason string, at time.Time) (*domain.Notification, error) {
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if reason == "" {
		return nil, domain.ErrCancelReasonRequired
	}
	var out domain.Notification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		err := scanNotification(tx.QueryRow(ctx, `
			UPDATE notifications
			SET status = 'CANCELLED', next_attempt_at = NULL, sent_at = $1,
			    cancelled_by = $2, cancelled_at = $1, cancel_reason = $3
			WHERE notification_id::text = $4 AND tenant_id = $5
			  AND status = 'PENDING' AND next_attempt_at IS NOT NULL AND submitting_since IS NULL
			RETURNING `+notificationColumns, at, actor, reason, id, tenantID), &out)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if qerr := tx.QueryRow(ctx, `SELECT true FROM notifications WHERE notification_id::text = $1 AND tenant_id = $2`, id, tenantID).Scan(&exists); qerr != nil {
				return domain.ErrNotificationNotFound
			}
			return domain.ErrCancelNotAllowed
		}
		if err != nil {
			return err
		}
		ev, err := events.CommunicationCancelled(out.CorrelationID, out)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return &out, nil
}

// ExpireNotification concludes a communication that passed expires_at before it was submitted
// (NCD-015). It returns false, without error, when the row is not expirable (already concluded,
// being submitted, or not yet due), so a sweeper and the submit gate can both call it safely.
func (s *PgStore) ExpireNotification(ctx context.Context, id, tenantID string, now time.Time) (bool, error) {
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	expired := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var out domain.Notification
		err := scanNotification(tx.QueryRow(ctx, `
			UPDATE notifications
			SET status = 'EXPIRED', next_attempt_at = NULL, sent_at = $1,
			    failure_reason = $2
			WHERE notification_id::text = $3 AND tenant_id = $4
			  AND status = 'PENDING' AND submitting_since IS NULL
			  AND expires_at IS NOT NULL AND expires_at <= $1
			RETURNING `+notificationColumns,
			now, ncd.Format(ncd.DeliveryExpired)+": the communication passed its expires_at before it was submitted, so it was never sent", id, tenantID), &out)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		expired = true
		failed, err := events.Failed(out.CorrelationID, out, out.FailureReason)
		if err != nil {
			return err
		}
		if err := enqueue(ctx, tx, tenantID, failed); err != nil {
			return err
		}
		blocked, err := events.CommunicationBlocked(out.CorrelationID, out, ncd.DeliveryExpired, out.DeliveryAttempts, false, "")
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, blocked)
	})
	if err != nil {
		return false, mapPgError(err)
	}
	return expired, nil
}

// FindExpiredQueued lists queued communications past their expires_at, across tenants,
// read-only under the platform scope. A row a worker has already claimed is not listed: the
// worker checks expiry itself at the submit gate.
func (s *PgStore) FindExpiredQueued(ctx context.Context, now time.Time, limit int) ([]domain.DueRetry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT notification_id::text, tenant_id FROM notifications
		WHERE status = 'PENDING' AND next_attempt_at IS NOT NULL AND submitting_since IS NULL
		  AND expires_at IS NOT NULL AND expires_at <= $1
		ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	var out []domain.DueRetry
	for rows.Next() {
		var d domain.DueRetry
		if err := rows.Scan(&d.NotificationID, &d.TenantID); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}
