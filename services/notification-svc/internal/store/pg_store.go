package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// mapPgError translates the Postgres failures that are really caller mistakes
// into domain errors, so they stop arriving at the handler as "the store is
// unavailable".
//
// notification_id is a uuid column, so a mistyped id compared against it dies
// inside the driver as SQLSTATE 22P02 before any row is examined, and used to
// reach the caller as 503 store_unavailable — an outage status for a typo in a
// URL. An id that cannot be a UUID names no notification, which is exactly
// what "not found" means. Same fix, same reasoning, as financial-close-svc,
// general-ledger-svc and accounts-payable-svc.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return domain.ErrNotificationNotFound
	}
	return err
}

// notificationColumns is the projection every read of the register uses.
//
// It is one constant rather than the same nineteen names written out at each
// call site. The three reads had already drifted before this existed — Get and
// List selected tenant_id and correlation_id, the idempotency replay inside
// Create did not — so a column added for one read reached the others only if
// someone noticed all three. scanNotification below is the matching half:
// the order here and the order there cannot diverge without failing every
// test in the package at once, which is the point.
const notificationColumns = `
	notification_id, tenant_id, legal_entity_id, recipient_principal_id,
	COALESCE(recipient_address, ''), COALESCE(recipient_address_source, ''),
	channel, subject, body, status,
	COALESCE(source_event_type, ''), COALESCE(source_reference, ''),
	correlation_id, COALESCE(failure_reason, ''), COALESCE(provider_response, ''),
	created_by_principal_id, created_at, sent_at, read_at,
	delivery_attempts, next_attempt_at, last_attempt_at`

// scannable is satisfied by both pgx.Row and pgx.Rows.
type scannable interface{ Scan(dest ...any) error }

func scanNotification(s scannable, n *domain.Notification) error {
	return s.Scan(
		&n.NotificationID, &n.TenantID, &n.LegalEntityID, &n.RecipientPrincipalID,
		&n.RecipientAddress, &n.RecipientAddressSource,
		&n.Channel, &n.Subject, &n.Body, &n.Status,
		&n.SourceEventType, &n.SourceReference,
		&n.CorrelationID, &n.FailureReason, &n.ProviderResponse,
		&n.CreatedByPrincipalID, &n.CreatedAt, &n.SentAt, &n.ReadAt,
		&n.DeliveryAttempts, &n.NextAttemptAt, &n.LastAttemptAt,
	)
}

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
				recipient_address, recipient_address_source,
				channel, subject, body, status, source_event_type, source_reference,
				correlation_id, created_by_principal_id, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, n.NotificationID, tenantID, n.LegalEntityID, n.RecipientPrincipalID,
			nullIfEmpty(n.RecipientAddress), nullIfEmpty(n.RecipientAddressSource),
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
			SELECT `+notificationColumns+`
			FROM notifications WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, n.CorrelationID)
		return scanNotification(row, n)
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
		return scanNotification(tx.QueryRow(ctx, `
			SELECT `+notificationColumns+`
			FROM notifications
			WHERE notification_id = $1 AND tenant_id = $2
		`, id, tenantID), &n)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotificationNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &n, nil
}

func (s *PgStore) ListNotifications(ctx context.Context, f domain.ListFilter) ([]domain.Notification, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Notification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + notificationColumns + `
			FROM notifications
			WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.LegalEntityID != "" {
			args = append(args, f.LegalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if f.RecipientPrincipalID != "" {
			args = append(args, f.RecipientPrincipalID)
			query += fmt.Sprintf(" AND recipient_principal_id = $%d", len(args))
		}
		if f.Status != "" {
			args = append(args, f.Status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if f.UnreadOnly {
			// Read state is an IN_APP concept — see MarkRead. Filtering
			// unread without also constraining the channel would report every
			// email ever sent as "unread", since read_at is NULL for all of
			// them and always will be.
			args = append(args, domain.ChannelInApp)
			query += fmt.Sprintf(" AND read_at IS NULL AND channel = $%d", len(args))
		}

		// created_at alone is not a total order — two notifications recorded in
		// the same transaction share a timestamp, and Postgres is free to
		// return them in either order, so a paged read could show one row twice
		// and skip another. notification_id breaks the tie.
		query += " ORDER BY created_at DESC, notification_id DESC"
		args = append(args, f.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
		args = append(args, f.Offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var n domain.Notification
			if err := scanNotification(rows, &n); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CompleteDelivery records the final SENT/FAILED status, the delivery
// timestamp and whatever the provider offered as acceptance evidence,
// mirroring spend-controls-svc's CompleteCheck pattern: creation and
// status-transition are separate operations.
//
// The guard on status is what makes the transition one-way. Without it a
// second delivery attempt — or a replayed request that slipped past the
// idempotency index — could move a concluded notification back through SENT,
// rewriting sent_at and the evidence with a later attempt's. A notification
// concludes once.
func (s *PgStore) CompleteDelivery(ctx context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE notifications
			SET status            = $1,
			    failure_reason    = $2,
			    provider_response = $3,
			    sent_at           = $4,
			    delivery_attempts = delivery_attempts + 1,
			    last_attempt_at   = $4,
			    -- Cleared unconditionally. A concluded notification with a
			    -- retry still scheduled would have the worker re-send a message
			    -- that already went out, which is the duplicate-notice failure
			    -- ZS-SVC-Y-001 §0.4 names directly. The schema refuses that
			    -- combination too; this is what keeps the schema satisfied.
			    next_attempt_at   = NULL
			WHERE notification_id = $5 AND tenant_id = $6 AND status = 'PENDING'
		`, newStatus, nullIfEmpty(failureReason), nullIfEmpty(providerResponse), sentAt, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrNotificationNotFound
		}
		return nil
	})
}

// ScheduleRetry records a failed attempt that is worth making again.
//
// The notification stays PENDING — it has not concluded — and carries the
// reason for the attempt that just failed, so a notification waiting on its
// fourth try still says what went wrong on the third.
//
// Guarded on status = 'PENDING' like CompleteDelivery: a notification that
// concluded while this attempt was in flight must not be dragged back into the
// retry queue.
func (s *PgStore) ScheduleRetry(ctx context.Context, id, tenantID, failureReason string, attemptedAt, nextAttemptAt time.Time) error {
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE notifications
			SET failure_reason    = $1,
			    delivery_attempts = delivery_attempts + 1,
			    last_attempt_at   = $2,
			    next_attempt_at   = $3
			WHERE notification_id = $4 AND tenant_id = $5 AND status = 'PENDING'
		`, nullIfEmpty(failureReason), attemptedAt, nextAttemptAt, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrNotificationNotFound
		}
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	return nil
}

// FindDueRetries lists notifications whose next attempt is due, across every
// tenant.
//
// This is the one read in the service that crosses tenants, and it is built to
// give up as little as possible for that:
//
//   - It runs under app.platform_scope, which migration 000004 honours with a
//     SELECT-ONLY policy. Nothing is written here. The claim is a separate,
//     tenant-scoped statement — see ClaimRetry — so the hatch never has to
//     authorise a write.
//   - It projects notification_id and tenant_id and nothing else. Subjects,
//     bodies and recipient addresses never cross the hatch; the worker
//     re-reads each notification tenant-scoped before doing anything with it.
//   - set_config(..., true) is transaction-local, so the flag cannot survive
//     on a pooled connection into somebody's request.
func (s *PgStore) FindDueRetries(ctx context.Context, now time.Time, limit int) ([]domain.DueRetry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT notification_id, tenant_id
		FROM notifications
		WHERE status = 'PENDING'
		  AND next_attempt_at IS NOT NULL
		  AND next_attempt_at <= $1
		ORDER BY next_attempt_at
		LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, err
	}

	var due []domain.DueRetry
	for rows.Next() {
		var d domain.DueRetry
		if err := rows.Scan(&d.NotificationID, &d.TenantID); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}
	return due, nil
}

// SetRecipientAddress fills in an address a first attempt could not resolve,
// for a notification that is still PENDING.
//
// Guarded on an empty current value, not just on status: the address on a
// notification is a delivery snapshot, and overwriting one that a previous
// attempt already used would rewrite the record of where a message actually
// went. Only the absent case is fillable.
func (s *PgStore) SetRecipientAddress(ctx context.Context, id, tenantID, address, source string) error {
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE notifications
			SET recipient_address = $1, recipient_address_source = $2
			WHERE notification_id = $3
			  AND tenant_id = $4
			  AND status = 'PENDING'
			  AND (recipient_address IS NULL OR recipient_address = '')
		`, address, source, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrNotificationNotFound
		}
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	return nil
}

// ClaimRetry takes ownership of one due notification, returning whether this
// caller got it.
//
// Tenant-scoped, deliberately: the platform-scope hatch is read-only, so every
// write in the retry path runs under the notification's own tenant exactly as
// a request would. The hatch buys the worker the ability to FIND work and
// nothing more.
//
// The claim is the `next_attempt_at IS NOT NULL` predicate. Whoever's UPDATE
// affects a row has taken it; a second replica polling the same second affects
// zero rows and is told so. That is why this needs no SKIP LOCKED and no
// advisory lock — the row itself is the claim, and clearing next_attempt_at
// leaves the notification "in flight" (PENDING, nothing scheduled) until the
// attempt concludes or re-schedules it.
func (s *PgStore) ClaimRetry(ctx context.Context, id, tenantID string) (bool, error) {
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	var claimed bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE notifications SET next_attempt_at = NULL
			WHERE notification_id = $1
			  AND tenant_id = $2
			  AND status = 'PENDING'
			  AND next_attempt_at IS NOT NULL
		`, id, tenantID)
		if err != nil {
			return err
		}
		claimed = res.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, mapPgError(err)
	}
	return claimed, nil
}

// MarkRead records that the recipient has seen an in-app notice.
//
// Two constraints are enforced in the statement rather than left to the
// handler, because both are about what the row is allowed to become:
//
//   - recipient_principal_id must match. Read state is the recipient's own
//     assertion; nobody else's read marks it, including an administrator
//     holding NOTIFICATION_VIEW over the whole legal entity. Someone reading
//     the register is not the recipient reading their notice.
//
//   - COALESCE keeps the FIRST read. Re-opening an inbox re-issues the mark,
//     and without this each one would move read_at forward, so "when did they
//     first see this" — the only question read_at can answer — would decay
//     into "when did they last look".
//
// Returns ErrNotificationNotFound when no row matches, which covers both an
// unknown id and one addressed to somebody else; the handler distinguishes
// those beforehand so the caller gets 404 or 403 rather than one code for two
// different facts.
func (s *PgStore) MarkRead(ctx context.Context, id, recipientPrincipalID string, readAt time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE notifications
			SET read_at = COALESCE(read_at, $1)
			WHERE notification_id = $2
			  AND tenant_id = $3
			  AND recipient_principal_id = $4
			  AND channel = $5
		`, readAt, id, tenantID, recipientPrincipalID, domain.ChannelInApp)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrNotificationNotFound
		}
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	return nil
}

// CountUnread returns how many in-app notices the principal has not opened.
//
// Scoped to IN_APP for the same reason MarkRead is: this service cannot know
// whether an email was read. Counting emails here would produce a badge that
// only ever grows, and clearing it would require asserting something untrue.
func (s *PgStore) CountUnread(ctx context.Context, recipientPrincipalID string) (int, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}

	var count int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM notifications
			WHERE tenant_id = $1
			  AND recipient_principal_id = $2
			  AND channel = $3
			  AND read_at IS NULL
		`, tenantID, recipientPrincipalID, domain.ChannelInApp).Scan(&count)
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// nullIfEmpty writes SQL NULL for an empty optional string.
//
// The columns it guards are nullable and read back through COALESCE, so an
// empty string and NULL are indistinguishable on the way out. They are not
// indistinguishable to a CHECK constraint: notifications_failed_has_reason
// tests `failure_reason IS NOT NULL AND failure_reason <> ''`, and a partial
// index or a future NOT NULL would treat '' as a present value. Storing the
// absence as absence keeps the column honest.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
