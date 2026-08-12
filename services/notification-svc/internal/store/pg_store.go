package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// CreateNotification inserts a new notification in PENDING status, idempotent
// on (tenant_id, correlation_id): a retry finds the existing row instead of
// sending a second notification for the same request.
func (s *PgStore) CreateNotification(ctx context.Context, n *domain.Notification) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO notifications (
				notification_id, tenant_id, legal_entity_id, recipient_principal_id,
				channel, subject, body, status, source_event_type, source_reference,
				correlation_id, created_by_principal_id, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, n.NotificationID, tenantID, n.LegalEntityID, n.RecipientPrincipalID,
			n.Channel, n.Subject, n.Body, n.Status, n.SourceEventType, n.SourceReference,
			n.CorrelationID, n.CreatedByPrincipalID, n.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}

		// Conflict: a notification for this (tenant_id, correlation_id)
		// already exists — fetch it so the caller replays the original
		// send outcome instead of sending a second, divergent one.
		row := tx.QueryRow(ctx, `
			SELECT notification_id, legal_entity_id, recipient_principal_id, channel, subject, body,
			       status, COALESCE(source_event_type, ''), COALESCE(source_reference, ''),
			       COALESCE(failure_reason, ''), created_by_principal_id, created_at, sent_at
			FROM notifications WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, n.CorrelationID)
		return row.Scan(
			&n.NotificationID, &n.LegalEntityID, &n.RecipientPrincipalID, &n.Channel, &n.Subject, &n.Body,
			&n.Status, &n.SourceEventType, &n.SourceReference,
			&n.FailureReason, &n.CreatedByPrincipalID, &n.CreatedAt, &n.SentAt,
		)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) GetNotification(ctx context.Context, id string) (*domain.Notification, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var n domain.Notification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT notification_id, tenant_id, legal_entity_id, recipient_principal_id, channel,
			       subject, body, status, COALESCE(source_event_type, ''), COALESCE(source_reference, ''),
			       correlation_id, COALESCE(failure_reason, ''), created_by_principal_id, created_at, sent_at
			FROM notifications
			WHERE notification_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&n.NotificationID, &n.TenantID, &n.LegalEntityID, &n.RecipientPrincipalID, &n.Channel,
			&n.Subject, &n.Body, &n.Status, &n.SourceEventType, &n.SourceReference,
			&n.CorrelationID, &n.FailureReason, &n.CreatedByPrincipalID, &n.CreatedAt, &n.SentAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotificationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *PgStore) ListNotifications(ctx context.Context, legalEntityID, recipientPrincipalID, status string) ([]domain.Notification, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Notification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT notification_id, tenant_id, legal_entity_id, recipient_principal_id, channel,
			       subject, body, status, COALESCE(source_event_type, ''), COALESCE(source_reference, ''),
			       correlation_id, COALESCE(failure_reason, ''), created_by_principal_id, created_at, sent_at
			FROM notifications
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if recipientPrincipalID != "" {
			args = append(args, recipientPrincipalID)
			query += fmt.Sprintf(" AND recipient_principal_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var n domain.Notification
			if err := rows.Scan(
				&n.NotificationID, &n.TenantID, &n.LegalEntityID, &n.RecipientPrincipalID, &n.Channel,
				&n.Subject, &n.Body, &n.Status, &n.SourceEventType, &n.SourceReference,
				&n.CorrelationID, &n.FailureReason, &n.CreatedByPrincipalID, &n.CreatedAt, &n.SentAt,
			); err != nil {
				return err
			}
			out = append(out, n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CompleteDelivery records the final SENT/FAILED status and delivery
// timestamp, mirroring spend-controls-svc's CompleteCheck pattern:
// creation and status-transition are separate operations.
func (s *PgStore) CompleteDelivery(ctx context.Context, id, newStatus, failureReason string, sentAt *time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE notifications
			SET status = $1, failure_reason = $2, sent_at = $3
			WHERE notification_id = $4 AND tenant_id = $5
		`, newStatus, failureReason, sentAt, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrNotificationNotFound
		}
		return nil
	})
}
