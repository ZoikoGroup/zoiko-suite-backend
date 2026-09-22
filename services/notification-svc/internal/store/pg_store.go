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
	"zoiko.io/notification-svc/internal/events"
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
// timestamp and whatever the provider offered as acceptance evidence, and
// enqueues the domain event for that conclusion in the SAME transaction.
//
// It mirrors spend-controls-svc's CompleteCheck pattern: creation and
// status-transition are separate operations.
//
// The guard on status is what makes the transition one-way. Without it a
// second delivery attempt — or a replayed request that slipped past the
// idempotency index — could move a concluded notification back through SENT,
// rewriting sent_at and the evidence with a later attempt's. A notification
// concludes once.
//
// ── Why the event is a parameter rather than something the caller does after ──
//
// Because it used to be something the caller did after, and that is the defect
// migration 000005 exists to close. Both call sites ran
//
//	if err := store.CompleteDelivery(...); err != nil { ...503... }
//	publisher.PublishSent(ctx, ...)   // logs on failure, returns, loses it
//
// so a broker outage at the moment a notice concluded produced a correctly
// recorded delivery, a correct 201, and no consumer anywhere learning that the
// notice went out or that it did not. Nothing reported the loss, because the
// thing that would have reported it was the event that was lost.
//
// Taking the sealed envelope here makes the two atomic: a notification that
// concluded always has its event, and an event that exists always has its
// conclusion. Delivery to the broker becomes internal/outbox's problem, where
// a failure is a retry.
//
// ev is REQUIRED, not optional. An Outbound with no EventType is refused
// before the UPDATE runs, so "conclude a delivery and tell nobody" is not a
// state a caller can reach by forgetting an argument — which is precisely how
// the old shape failed.
//
// THE TRADE, STATED. Because the enqueue shares the transaction, a failure to
// enqueue fails the whole conclusion — and by then the provider has already
// accepted the message. The row stays PENDING in flight, SweepStranded
// eventually reclaims it, and the recipient may get a second copy. That is the
// worse-looking of the two outcomes and still the right one: the enqueue can
// only fail if event_outbox is missing or its CHECK rejects the event type,
// which are deploy faults that affect every send equally and want to be loud.
// Committing the conclusion and dropping the event is the failure that is
// silent, permanent, and indistinguishable from success.
func (s *PgStore) CompleteDelivery(ctx context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time, ev events.Outbound) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if ev.EventType == "" {
		return fmt.Errorf("conclude %s: no event supplied — a delivery may not conclude without one", id)
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
			// No transition happened, so no event describes one. Returning
			// before the enqueue is what keeps that true: a concluded
			// notification re-concluded by a racing replica must not emit a
			// second notification.sent for a delivery that happened once.
			return domain.ErrNotificationNotFound
		}
		return enqueue(ctx, tx, tenantID, ev)
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

// FindStrandedDeliveries lists notifications that are in flight and have been
// for longer than any attempt could plausibly take, across every tenant.
//
// WHAT A STRANDED ROW IS. PENDING with next_attempt_at NULL means "in flight
// right now" (domain.Notification says so, and ClaimRetry creates that state
// deliberately). Nothing ever moves such a row on its own: FindDueRetries
// requires next_attempt_at IS NOT NULL, so a notification left in flight is
// invisible to the retry path forever — never delivered, never failed, never
// re-attempted, and showing in the register as PENDING, which reads as
// progress rather than as a problem.
//
// FOUR WAYS A ROW GETS THERE, all of them real:
//
//  1. The process dies between CreateNotification and the statement that
//     concludes or reschedules the send.
//  2. ScheduleRetry itself fails. The handler answers 503 and returns, leaving
//     the row it just created in flight.
//  3. CompleteDelivery fails, identically.
//  4. The request context is cancelled mid-attempt. Both the delivery call and
//     the store write that records its outcome run on r.Context(), and the
//     server's WriteTimeout is 15s, so a provider that takes longer than the
//     request lives means the outcome cannot be written.
//
// Measured on the dev database on 2026-09-08: five notifications from
// 2026-09-02 in exactly this state, delivery_attempts = 0, six days old, with
// no mechanism in the service that would ever have touched them again.
// internal/retry's own RunOnce comment said a claimed row left "PENDING with
// nothing scheduled ... is what the sweep below is for". There was no sweep.
//
// SAFETY, which is the whole reason for the staleBefore parameter. A row that
// is genuinely being attempted right now looks identical to a stranded one —
// the difference is only how long it has looked that way. Reviving a row
// another replica is mid-SMTP on would send the message twice. staleBefore
// must therefore be older than the longest attempt the service can make: the
// SMTP provider's own timeout defaults to 10s and the HTTP server's
// WriteTimeout is 15s, so an attempt cannot outlive ~30s, and the default
// threshold is 15 minutes — two orders of magnitude of headroom.
//
// COALESCE(last_attempt_at, created_at) is the in-flight-since clock: a row
// stranded before its first attempt has no last_attempt_at at all, which is
// the majority of the case above and would be skipped by a predicate on
// last_attempt_at alone.
//
// Same tenant posture as FindDueRetries, for the same reasons: platform-scope
// SELECT only, transaction-local, and a projection of nothing but the id and
// the tenant, so no subject, body or recipient address crosses the hatch.
func (s *PgStore) FindStrandedDeliveries(ctx context.Context, staleBefore time.Time, limit int) ([]domain.DueRetry, error) {
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
		  AND next_attempt_at IS NULL
		  AND COALESCE(last_attempt_at, created_at) <= $1
		ORDER BY COALESCE(last_attempt_at, created_at)
		LIMIT $2
	`, staleBefore, limit)
	if err != nil {
		return nil, err
	}

	var stranded []domain.DueRetry
	for rows.Next() {
		var d domain.DueRetry
		if err := rows.Scan(&d.NotificationID, &d.TenantID); err != nil {
			rows.Close()
			return nil, err
		}
		stranded = append(stranded, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}
	return stranded, nil
}

// ReviveStranded puts a stranded notification back on the retry schedule,
// returning whether this caller was the one that did it.
//
// It schedules rather than concludes, deliberately. The service does not know
// whether a stranded attempt reached the provider — that is exactly the
// information the crash destroyed — so the choice is between a notice that may
// arrive twice and one that certainly never arrives. For a governed
// notification the first is the right way to be wrong, and the staleness
// threshold keeps it rare; SENT rows are never touched, so a delivery that DID
// record its success is never re-sent.
//
// Tenant-scoped, like every other write in the retry path: the platform-scope
// hatch is read-only and buys the worker the ability to FIND work, never to
// change it.
//
// The staleness predicate is repeated inside the UPDATE and is the claim. A
// row that stopped being stranded between the find and this statement — a
// replica concluded it, or a fresh attempt updated last_attempt_at — no longer
// matches, so this affects zero rows and says so, rather than dragging a
// live notification back onto the schedule. Same shape as ClaimRetry, where
// the row itself is the claim and no advisory lock is needed.
func (s *PgStore) ReviveStranded(ctx context.Context, id, tenantID string, staleBefore, nextAttemptAt time.Time) (bool, error) {
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	var revived bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE notifications SET next_attempt_at = $1
			WHERE notification_id = $2
			  AND tenant_id = $3
			  AND status = 'PENDING'
			  AND next_attempt_at IS NULL
			  AND COALESCE(last_attempt_at, created_at) <= $4
		`, nextAttemptAt, id, tenantID, staleBefore)
		if err != nil {
			return err
		}
		revived = res.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, mapPgError(err)
	}
	return revived, nil
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

// ── transactional outbox ─────────────────────────────────────────────────────

// OutboxRecord is one claimed, unpublished event.
type OutboxRecord struct {
	OutboxID  int64
	EventType string
	Key       string
	Body      []byte
}

// enqueue writes one sealed envelope into event_outbox on the caller's open
// transaction.
//
// Sharing the transaction is the whole point — see CompleteDelivery, which is
// the only caller, for what the alternative cost.
//
// It is unexported and takes a tx rather than a context, so there is no way to
// enqueue an event OUTSIDE the transaction that records the fact it describes.
// An exported EnqueueEvent(ctx, ...) would have re-created the original defect
// with a table in the middle of it.
func enqueue(ctx context.Context, tx pgx.Tx, tenantID string, out events.Outbound) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO event_outbox (tenant_id, event_type, aggregate_key, payload)
		VALUES ($1, $2, $3, $4)
	`, tenantID, out.EventType, out.Key, out.Body)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", out.EventType, err)
	}
	return nil
}

// withRelay runs fn with app.outbox_relay installed instead of a tenant.
//
// The relay is the one write path in this service that legitimately crosses
// tenants: it drains every tenant's backlog from a single loop. Rather than
// letting it run unscoped — which under FORCE ROW LEVEL SECURITY would simply
// see nothing, and would present as a relay that publishes nothing while
// reporting no error at all — it names itself, so migration 000005's policy
// admits it by an explicit, auditable disjunct rather than by the absence of a
// control.
//
// A DIFFERENT flag from app.platform_scope, which 000004 grants over
// `notifications`. That hatch is SELECT-only and exists so the retry worker can
// discover work; this one must also UPDATE, to mark a batch published. Reusing
// one name would have silently widened the retry hatch from read to write
// across every tenant's notification bodies.
//
// set_config(..., true) is transaction-local, so the flag cannot survive on a
// pooled connection into somebody's request.
func (s *PgStore) withRelay(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin relay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return fmt.Errorf("set relay context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit relay transaction: %w", err)
	}
	return nil
}

// ClaimOutbox takes up to limit unpublished events, oldest first, locking them
// for the duration of the caller's drain.
//
// FOR UPDATE SKIP LOCKED is what makes more than one replica safe: a second
// relay claims the next batch instead of blocking on, or duplicating, this one.
func (s *PgStore) ClaimOutbox(ctx context.Context, limit int, fn func([]OutboxRecord) error) error {
	return s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT outbox_id, event_type, aggregate_key, payload
			FROM event_outbox
			WHERE published_at IS NULL
			ORDER BY created_at, outbox_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		`, limit)
		if err != nil {
			return fmt.Errorf("claim outbox: %w", err)
		}
		var claimed []OutboxRecord
		for rows.Next() {
			var r OutboxRecord
			if err := rows.Scan(&r.OutboxID, &r.EventType, &r.Key, &r.Body); err != nil {
				rows.Close()
				return fmt.Errorf("scan outbox row: %w", err)
			}
			claimed = append(claimed, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read outbox rows: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}

		// fn publishes. It runs while the rows are still locked and BEFORE the
		// marking commits, so a crash mid-publish rolls the marking back and
		// the events are re-delivered rather than lost. At-least-once, chosen
		// deliberately: a duplicate notification.sent makes a consumer
		// re-handle an event it has already seen — which the event_id and the
		// notification_id both let it detect — while a lost one leaves a
		// governed notice whose issue nobody was ever told about.
		if err := fn(claimed); err != nil {
			ids := make([]int64, 0, len(claimed))
			for _, r := range claimed {
				ids = append(ids, r.OutboxID)
			}
			// Recorded on the rows themselves, so a stuck event can be
			// diagnosed from the table without correlating against logs.
			if _, uerr := tx.Exec(ctx, `
				UPDATE event_outbox
				SET attempts = attempts + 1, last_error = $2
				WHERE outbox_id = ANY($1)
			`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			// Committed: the attempt count and the error are worth keeping
			// even though the publish failed. published_at is untouched, so
			// the rows are claimed again on the next tick.
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}

		ids := make([]int64, 0, len(claimed))
		for _, r := range claimed {
			ids = append(ids, r.OutboxID)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE event_outbox SET published_at = now() WHERE outbox_id = ANY($1)
		`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
//
// Both, not just the depth. A backlog of ten that is three seconds old is a
// service under load; a backlog of ten that is an hour old is a relay that has
// stopped — and on this service that means an hour of governed notices whose
// issue no consumer has been told about, while the register shows every one of
// them concluded. Depth alone cannot tell those apart, which is why the alert
// rule keys on the age.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		row := tx.QueryRow(ctx, `
			SELECT count(*), min(created_at) FROM event_outbox WHERE published_at IS NULL
		`)
		if err := row.Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}

// nullIfEmpty writes SQL NULL for an empty optional string.
//
// The columns it guards are nullable and read back through COALESCE, so an
// empty string and NULL are indistinguishable on the way out. They are not
// indistinguishable to a CHECK constraint: notifications_failed_has_reason
// tests
//
//	failure_reason IS NOT NULL AND failure_reason <> ''
//
// and a partial index or a future NOT NULL would treat the empty string as a
// present value. Storing the absence as absence keeps the column honest.
//
// The SQL is in an indented block rather than inline, and that is not
// cosmetic. gofmt normalises doc comments (Go 1.19+) and rewrites a bare pair
// of single quotes into a typographic closing quote — so written inline, this
// comment silently became `failure_reason <> ”`, which is not valid SQL and no
// longer describes the constraint. It is why this file failed gofmt for as long
// as it did: the only way to make it pass was to let gofmt corrupt the one
// sentence explaining the function. An indented block is preserved verbatim.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
