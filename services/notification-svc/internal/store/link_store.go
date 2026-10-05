package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// ZS-SVC-Y-001 INV-02 (migration 000016): one logical communication, one identity.
//
// LinkIntentToNotification joins the two halves of a communication, a ledger
// message intent and the direct-path notification that carries its delivery. Both
// columns are set in one transaction, so the link never exists on one side only.
// It is idempotent: linking the same pair again changes nothing. Nothing calls it
// yet; the ledger pipeline starts to in a later step, behind a flag.
func (s *PgStore) LinkIntentToNotification(ctx context.Context, intentID, notificationID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Both rows are locked and read first, so "not found", "already linked
		// elsewhere" and "already linked to this" are told apart rather than
		// guessed from a row count.
		var intentLink *string
		err := tx.QueryRow(ctx, `SELECT notification_id::text FROM message_intents
			WHERE message_intent_id::text = $1 AND tenant_id = $2 FOR UPDATE`, intentID, tenantID).Scan(&intentLink)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrLinkTargetNotFound
		}
		if err != nil {
			return fmt.Errorf("link intent: read intent: %w", err)
		}
		var notifLink *string
		err = tx.QueryRow(ctx, `SELECT message_intent_id::text FROM notifications
			WHERE notification_id::text = $1 AND tenant_id = $2 FOR UPDATE`, notificationID, tenantID).Scan(&notifLink)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrLinkTargetNotFound
		}
		if err != nil {
			return fmt.Errorf("link intent: read notification: %w", err)
		}

		intentDone := intentLink != nil && *intentLink == notificationID
		notifDone := notifLink != nil && *notifLink == intentID
		if intentDone && notifDone {
			return nil // already linked to each other
		}
		if (intentLink != nil && !intentDone) || (notifLink != nil && !notifDone) {
			return domain.ErrAlreadyLinked
		}

		if _, err := tx.Exec(ctx, `UPDATE message_intents SET notification_id = $2::uuid
			WHERE message_intent_id::text = $1 AND tenant_id = $3`, intentID, notificationID, tenantID); err != nil {
			return fmt.Errorf("link intent: set intent side: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE notifications SET message_intent_id = $2::uuid
			WHERE notification_id::text = $1 AND tenant_id = $3`, notificationID, intentID, tenantID); err != nil {
			return fmt.Errorf("link intent: set notification side: %w", err)
		}
		return nil
	})
}

// NotificationIDForIntent returns the notification an intent is linked to, or ""
// when it has none.
func (s *PgStore) NotificationIDForIntent(ctx context.Context, intentID string) (string, error) {
	return s.linkedID(ctx, `SELECT COALESCE(notification_id::text, '') FROM message_intents
		WHERE message_intent_id::text = $1 AND tenant_id = $2`, intentID)
}

// IntentIDForNotification returns the intent a notification is linked to, or ""
// when it has none (every direct send made before the ledger path links).
func (s *PgStore) IntentIDForNotification(ctx context.Context, notificationID string) (string, error) {
	return s.linkedID(ctx, `SELECT COALESCE(message_intent_id::text, '') FROM notifications
		WHERE notification_id::text = $1 AND tenant_id = $2`, notificationID)
}

func (s *PgStore) linkedID(ctx context.Context, query, id string) (string, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return "", domain.ErrIdentityMissing
	}
	var out string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, tenantID).Scan(&out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrLinkTargetNotFound
	}
	return out, err
}
