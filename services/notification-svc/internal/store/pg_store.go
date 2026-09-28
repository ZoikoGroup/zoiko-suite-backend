package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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
	delivery_attempts, next_attempt_at, last_attempt_at,
	COALESCE(idempotency_key, ''), COALESCE(purpose_context, '')`

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
		&n.IdempotencyKey, &n.PurposeContext,
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
// on (tenant_id, idempotency_key): a retry replays the original delivery
// outcome rather than sending a second notification for the same request.
// The idempotency key is purpose-scoped — two different communications
// sharing a correlation_id but with different purposes MUST NOT collide
// (ZS-SVC-Y-001 §3.4).
func (s *PgStore) CreateNotification(ctx context.Context, n *domain.Notification) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	// If no idempotency key provided, derive one from the purpose-scoped components
	if n.IdempotencyKey == "" {
		n.IdempotencyKey = fmt.Sprintf("%s|%s|%s|%s", tenantID, n.LegalEntityID, n.CorrelationID, n.PurposeContext)
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO notifications (
				notification_id, tenant_id, legal_entity_id, recipient_principal_id,
				recipient_address, recipient_address_source,
				channel, subject, body, status, source_event_type, source_reference,
				correlation_id, created_by_principal_id, created_at,
				idempotency_key, purpose_context
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
		`, n.NotificationID, tenantID, n.LegalEntityID, n.RecipientPrincipalID,
			nullIfEmpty(n.RecipientAddress), nullIfEmpty(n.RecipientAddressSource),
			n.Channel, n.Subject, n.Body, n.Status, n.SourceEventType, n.SourceReference,
			n.CorrelationID, n.CreatedByPrincipalID, n.CreatedAt,
			n.IdempotencyKey, n.PurposeContext)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}

		// Conflict: a notification for this (tenant_id, idempotency_key)
		// already exists — fetch it so the caller replays the original
		// send outcome instead of sending a second, divergent one.
		row := tx.QueryRow(ctx, `
			SELECT `+notificationColumns+`
			FROM notifications WHERE tenant_id = $1 AND idempotency_key = $2
		`, tenantID, n.IdempotencyKey)
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

// GetByIdempotencyKey returns the notification for a given purpose-scoped
// idempotency key, or nil if not found. Used by the handler to replay
// the original outcome instead of sending a duplicate.
func (s *PgStore) GetByIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (*domain.Notification, error) {
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var n domain.Notification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanNotification(tx.QueryRow(ctx, `
			SELECT `+notificationColumns+`
			FROM notifications
			WHERE tenant_id = $1 AND idempotency_key = $2
		`, tenantID, idempotencyKey), &n)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &n, nil
}

// CreateAttempt records a durable attempt record per §3.4.
func (s *PgStore) CreateAttempt(ctx context.Context, a *domain.DeliveryAttempt) error {
	if a.AttemptID == "" {
		a.AttemptID = uuid.NewString()
	}
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO delivery_attempts (
				attempt_id, notification_id, attempt_number, channel, provider,
				status, provider_response, failure_reason, retryable, resend_reason, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, a.AttemptID, a.NotificationID, a.AttemptNumber, a.Channel, a.Provider,
			a.Status, nullIfEmpty(a.ProviderResponse), nullIfEmpty(a.FailureReason),
			a.Retryable, nullIfEmpty(a.ResendReason), a.CreatedAt)
		return err
	})
}

// UpdateAttempt updates an attempt record with its conclusion.
func (s *PgStore) UpdateAttempt(ctx context.Context, attemptID, status, failureReason, providerResponse string, concludedAt *time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE delivery_attempts
			SET status = $1, failure_reason = $2, provider_response = $3, concluded_at = $4
			WHERE attempt_id = $5
		`, status, nullIfEmpty(failureReason), nullIfEmpty(providerResponse), concludedAt, attemptID)
		return err
	})
}

// GetAttempts returns all attempt records for a notification, ordered by attempt number.
func (s *PgStore) GetAttempts(ctx context.Context, notificationID string) ([]domain.DeliveryAttempt, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var attempts []domain.DeliveryAttempt
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT attempt_id, notification_id, attempt_number, channel, provider,
			       status, provider_response, failure_reason, retryable, resend_reason, created_at, concluded_at
			FROM delivery_attempts
			WHERE notification_id = $1
			ORDER BY attempt_number
		`, notificationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.DeliveryAttempt
			if err := rows.Scan(&a.AttemptID, &a.NotificationID, &a.AttemptNumber, &a.Channel, &a.Provider,
				&a.Status, &a.ProviderResponse, &a.FailureReason, &a.Retryable, &a.ResendReason,
				&a.CreatedAt, &a.ConcludedAt); err != nil {
				return err
			}
			attempts = append(attempts, a)
		}
		return rows.Err()
	})
	return attempts, err
}

// FindStuckInFlight finds notifications that are in flight (PENDING or UNKNOWN
 // with no next_attempt_at) for longer than the threshold. This is the reconciliation
 // step before re-attempting — ZS-SVC-Y-001 §3.4: "Timeout after submit
 // becomes UNKNOWN, not FAILED; reconcile before re-attempting".
 func (s *PgStore) FindStuckInFlight(ctx context.Context, staleBefore time.Time, limit int) ([]domain.DueRetry, error) {
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
 		WHERE status IN ('PENDING', 'UNKNOWN')
 		  AND next_attempt_at IS NULL
 		  AND COALESCE(last_attempt_at, created_at) <= $1
 		ORDER BY COALESCE(last_attempt_at, created_at)
 		LIMIT $2
 	`, staleBefore, limit)
 	if err != nil {
 		return nil, err
 	}

 	var stuck []domain.DueRetry
 	for rows.Next() {
 		var d domain.DueRetry
 		if err := rows.Scan(&d.NotificationID, &d.TenantID); err != nil {
 			rows.Close()
 			return nil, err
 		}
 		stuck = append(stuck, d)
 	}
 	rows.Close()
 	if err := rows.Err(); err != nil {
 		return nil, err
 	}
 	if err := tx.Commit(ctx); err != nil {
 		return nil, fmt.Errorf("commit transaction: %w", err)
 	}
return stuck, nil
}

// ── NCD-02: Suppression ───────────────────────────────────────────────────────

const suppressionColumns = `
	suppression_id, tenant_id, principal_id, channel, reason,
	created_by, created_at, expires_at`

func scanSuppression(s scannable, sp *domain.Suppression) error {
	return s.Scan(
		&sp.SuppressionID, &sp.TenantID, &sp.PrincipalID, &sp.Channel,
		&sp.Reason, &sp.CreatedBy, &sp.CreatedAt, &sp.ExpiresAt,
	)
}

func (s *PgStore) CreateSuppression(ctx context.Context, sp *domain.Suppression) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if sp.SuppressionID == "" {
		sp.SuppressionID = uuid.NewString()
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO suppressions (suppression_id, tenant_id, principal_id, channel, reason, created_by, created_at, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		`, sp.SuppressionID, tenantID, sp.PrincipalID, sp.Channel, sp.Reason, sp.CreatedBy, sp.CreatedAt, sp.ExpiresAt)
		return err
	})
}

func (s *PgStore) GetSuppression(ctx context.Context, id string) (*domain.Suppression, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var sp domain.Suppression
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanSuppression(tx.QueryRow(ctx, `
			SELECT `+suppressionColumns+`
			FROM suppressions
			WHERE suppression_id = $1 AND tenant_id = $2
		`, id, tenantID), &sp)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSuppressionNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &sp, nil
}

func (s *PgStore) ListSuppressions(ctx context.Context, f domain.SuppressionFilter) ([]domain.Suppression, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Suppression
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + suppressionColumns + ` FROM suppressions WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.PrincipalID != "" {
			args = append(args, f.PrincipalID)
			query += fmt.Sprintf(" AND principal_id = $%d", len(args))
		}
		if f.Channel != "" {
			args = append(args, f.Channel)
			query += fmt.Sprintf(" AND channel = $%d", len(args))
		}
		if f.Reason != "" {
			args = append(args, f.Reason)
			query += fmt.Sprintf(" AND reason = $%d", len(args))
		}
		if f.ActiveOnly {
			query += " AND (expires_at IS NULL OR expires_at > now())"
		}

		query += " ORDER BY created_at DESC"
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
			var sp domain.Suppression
			if err := scanSuppression(rows, &sp); err != nil {
				return err
			}
			out = append(out, sp)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) DeleteSuppression(ctx context.Context, id string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			DELETE FROM suppressions WHERE suppression_id = $1 AND tenant_id = $2
		`, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrSuppressionNotFound
		}
		return nil
	})
}

// ── NCD-02: Preference ────────────────────────────────────────────────────────

const preferenceColumns = `
	preference_id, tenant_id, principal_id, channel, enabled,
	quiet_hours_start, quiet_hours_end, timezone, created_at, updated_at`

func scanPreference(s scannable, p *domain.Preference) error {
	return s.Scan(
		&p.PreferenceID, &p.TenantID, &p.PrincipalID, &p.Channel,
		&p.Enabled, &p.QuietHoursStart, &p.QuietHoursEnd, &p.Timezone,
		&p.CreatedAt, &p.UpdatedAt,
	)
}

func (s *PgStore) UpsertPreference(ctx context.Context, p *domain.Preference) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if p.PreferenceID == "" {
		p.PreferenceID = uuid.NewString()
	}
	p.UpdatedAt = time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO preferences (preference_id, tenant_id, principal_id, channel, enabled, quiet_hours_start, quiet_hours_end, timezone, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (tenant_id, principal_id, channel) DO UPDATE SET
				enabled = EXCLUDED.enabled,
				quiet_hours_start = EXCLUDED.quiet_hours_start,
				quiet_hours_end = EXCLUDED.quiet_hours_end,
				timezone = EXCLUDED.timezone,
				updated_at = EXCLUDED.updated_at
		`, p.PreferenceID, tenantID, p.PrincipalID, p.Channel, p.Enabled,
			p.QuietHoursStart, p.QuietHoursEnd, p.Timezone,
			p.CreatedAt, p.UpdatedAt)
		return err
	})
}

func (s *PgStore) GetPreference(ctx context.Context, tenantID, principalID, channel string) (*domain.Preference, error) {
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var p domain.Preference
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanPreference(tx.QueryRow(ctx, `
			SELECT `+preferenceColumns+`
			FROM preferences
			WHERE tenant_id = $1 AND principal_id = $2 AND channel = $3
		`, tenantID, principalID, channel), &p)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPreferenceNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &p, nil
}

func (s *PgStore) ListPreferences(ctx context.Context, f domain.PreferenceFilter) ([]domain.Preference, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Preference
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + preferenceColumns + ` FROM preferences WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.PrincipalID != "" {
			args = append(args, f.PrincipalID)
			query += fmt.Sprintf(" AND principal_id = $%d", len(args))
		}
		if f.Channel != "" {
			args = append(args, f.Channel)
			query += fmt.Sprintf(" AND channel = $%d", len(args))
		}
		if f.EnabledOnly {
			query += " AND enabled = true"
		}

		query += " ORDER BY created_at DESC"
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
			var p domain.Preference
			if err := scanPreference(rows, &p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) DeletePreference(ctx context.Context, tenantID, principalID, channel string) error {
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			DELETE FROM preferences WHERE tenant_id = $1 AND principal_id = $2 AND channel = $3
		`, tenantID, principalID, channel)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrPreferenceNotFound
		}
		return nil
	})
}

// ── NCD-02: Channel Decision ──────────────────────────────────────────────────

const channelDecisionColumns = `
	decision_id, tenant_id, principal_id, channel, decision,
	suppression_id, preference_id, permission_grant, evaluated_at`

func scanChannelDecision(s scannable, cd *domain.ChannelDecision) error {
	return s.Scan(
		&cd.DecisionID, &cd.TenantID, &cd.PrincipalID, &cd.Channel,
		&cd.Decision, &cd.SuppressionID, &cd.PreferenceID, &cd.PermissionGrant,
		&cd.EvaluatedAt,
	)
}

// EvaluateChannel checks suppression, preference, quiet hours, and permission.
// Returns the decision result and records it for audit.
// Precedence per §5.4: suppression > quiet hours > preference > permission.
func (s *PgStore) EvaluateChannel(ctx context.Context, tenantID, principalID, channel, permissionGrant string) (*domain.ChannelDecisionResult, error) {
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	// We need to run this with the tenant context but also read suppressions/preferences
	// which are tenant-scoped. The logic is:
	// 1. Check active suppression for this channel
	// 2. Check quiet hours (from preference)
	// 3. Check preference enabled
	// 4. Check permission grant (passed in)

	var result domain.ChannelDecisionResult
	result.Decision = domain.ChannelDecisionNoPermission // default if no permission grant

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()

		// 1. Check suppression (highest precedence)
		var sup domain.Suppression
		err := scanSuppression(tx.QueryRow(ctx, `
			SELECT `+suppressionColumns+`
			FROM suppressions
			WHERE tenant_id = $1 AND principal_id = $2 AND channel = $3
			  AND (expires_at IS NULL OR expires_at > $4)
			LIMIT 1
		`, tenantID, principalID, channel, now), &sup)
		if err == nil {
			// Suppression found
			result.Allowed = false
			result.Decision = domain.ChannelDecisionSuppressed
			result.SuppressionID = &sup.SuppressionID
			return s.recordDecision(ctx, tx, tenantID, principalID, channel, result, permissionGrant)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// 2. Check preference for quiet hours
		var pref domain.Preference
		err = scanPreference(tx.QueryRow(ctx, `
			SELECT `+preferenceColumns+`
			FROM preferences
			WHERE tenant_id = $1 AND principal_id = $2 AND channel = $3
		`, tenantID, principalID, channel), &pref)
		if err == nil {
			// Check quiet hours
			if pref.QuietHoursStart != nil && pref.QuietHoursEnd != nil && pref.Timezone != "" {
				loc, tzErr := time.LoadLocation(pref.Timezone)
				if tzErr == nil {
					localNow := now.In(loc)
					start, _ := time.ParseInLocation("15:04:05", *pref.QuietHoursStart, loc)
					end, _ := time.ParseInLocation("15:04:05", *pref.QuietHoursEnd, loc)

					// Compare just the time portion
					localTime := time.Date(1, 1, 1, localNow.Hour(), localNow.Minute(), localNow.Second(), 0, loc)
					startTime := time.Date(1, 1, 1, start.Hour(), start.Minute(), start.Second(), 0, loc)
					endTime := time.Date(1, 1, 1, end.Hour(), end.Minute(), end.Second(), 0, loc)

					inQuietHours := false
					if startTime.Before(endTime) {
						// Same day (e.g., 09:00-17:00)
						inQuietHours = !localTime.Before(startTime) && localTime.Before(endTime)
					} else {
						// Crosses midnight (e.g., 22:00-08:00)
						inQuietHours = !localTime.Before(startTime) || localTime.Before(endTime)
					}

					if inQuietHours && pref.Enabled {
						result.Allowed = false
						result.Decision = domain.ChannelDecisionQuietHours
						result.PreferenceID = &pref.PreferenceID
						return s.recordDecision(ctx, tx, tenantID, principalID, channel, result, permissionGrant)
					}
				}
			}

			// 3. Check preference enabled
			if !pref.Enabled {
				result.Allowed = false
				result.Decision = domain.ChannelDecisionNoPreference
				result.PreferenceID = &pref.PreferenceID
				return s.recordDecision(ctx, tx, tenantID, principalID, channel, result, permissionGrant)
			}

			result.PreferenceID = &pref.PreferenceID
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// 4. Check permission grant (provided by caller)
		if permissionGrant != "" {
			result.Allowed = true
			result.Decision = domain.ChannelDecisionAllowed
			result.PermissionGrant = &permissionGrant
		} else {
			result.Allowed = false
			result.Decision = domain.ChannelDecisionNoPermission
		}

		return s.recordDecision(ctx, tx, tenantID, principalID, channel, result, permissionGrant)
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *PgStore) recordDecision(ctx context.Context, tx pgx.Tx, tenantID, principalID, channel string, result domain.ChannelDecisionResult, permissionGrant string) error {
	decisionID := uuid.NewString()
	_, err := tx.Exec(ctx, `
		INSERT INTO channel_decisions (decision_id, tenant_id, principal_id, channel, decision, suppression_id, preference_id, permission_grant, evaluated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, decisionID, tenantID, principalID, channel, result.Decision,
		result.SuppressionID, result.PreferenceID, result.PermissionGrant, time.Now().UTC())
	return err
}

func (s *PgStore) ListChannelDecisions(ctx context.Context, f domain.ChannelDecisionFilter) ([]domain.ChannelDecision, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ChannelDecision
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + channelDecisionColumns + ` FROM channel_decisions WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.PrincipalID != "" {
			args = append(args, f.PrincipalID)
			query += fmt.Sprintf(" AND principal_id = $%d", len(args))
		}
		if f.Channel != "" {
			args = append(args, f.Channel)
			query += fmt.Sprintf(" AND channel = $%d", len(args))
		}
		if f.Decision != "" {
			args = append(args, f.Decision)
			query += fmt.Sprintf(" AND decision = $%d", len(args))
		}

		query += " ORDER BY evaluated_at DESC"
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
			var cd domain.ChannelDecision
			if err := scanChannelDecision(rows, &cd); err != nil {
				return err
			}
			out = append(out, cd)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── NCD-04: Bounce ────────────────────────────────────────────────────────────

const bounceEventColumns = `
	bounce_id, tenant_id, notification_id, provider, bounce_type,
	bounce_subtype, diagnostic_code, recipient_address, received_at,
	processed_at, action_taken, suppression_id`

func scanBounceEvent(s scannable, b *domain.BounceEvent) error {
	return s.Scan(
		&b.BounceID, &b.TenantID, &b.NotificationID, &b.Provider,
		&b.BounceType, &b.BounceSubtype, &b.DiagnosticCode,
		&b.RecipientAddress, &b.ReceivedAt, &b.ProcessedAt,
		&b.ActionTaken, &b.SuppressionID,
	)
}

func (s *PgStore) CreateBounceEvent(ctx context.Context, b *domain.BounceEvent) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if b.BounceID == "" {
		b.BounceID = uuid.NewString()
	}
	if b.ReceivedAt.IsZero() {
		b.ReceivedAt = time.Now().UTC()
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO bounce_events (bounce_id, tenant_id, notification_id, provider, bounce_type, bounce_subtype, diagnostic_code, recipient_address, received_at, processed_at, action_taken, suppression_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		`, b.BounceID, tenantID, b.NotificationID, b.Provider, b.BounceType, b.BounceSubtype,
			b.DiagnosticCode, b.RecipientAddress, b.ReceivedAt, b.ProcessedAt, b.ActionTaken, b.SuppressionID)
		return err
	})
}

func (s *PgStore) GetBounceEvent(ctx context.Context, id string) (*domain.BounceEvent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var b domain.BounceEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanBounceEvent(tx.QueryRow(ctx, `
			SELECT `+bounceEventColumns+`
			FROM bounce_events
			WHERE bounce_id = $1 AND tenant_id = $2
		`, id, tenantID), &b)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrBounceNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &b, nil
}

func (s *PgStore) ListBounceEvents(ctx context.Context, f domain.BounceEventFilter) ([]domain.BounceEvent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.BounceEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + bounceEventColumns + ` FROM bounce_events WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.NotificationID != "" {
			args = append(args, f.NotificationID)
			query += fmt.Sprintf(" AND notification_id = $%d", len(args))
		}
		if f.RecipientAddr != "" {
			args = append(args, f.RecipientAddr)
			query += fmt.Sprintf(" AND recipient_address = $%d", len(args))
		}
		if f.BounceType != "" {
			args = append(args, f.BounceType)
			query += fmt.Sprintf(" AND bounce_type = $%d", len(args))
		}

		query += " ORDER BY received_at DESC"
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
			var b domain.BounceEvent
			if err := scanBounceEvent(rows, &b); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── NCD-04: Complaint ─────────────────────────────────────────────────────────

const complaintEventColumns = `
	complaint_id, tenant_id, notification_id, provider, complaint_type,
	recipient_address, received_at, processed_at, action_taken, suppression_id`

func scanComplaintEvent(s scannable, c *domain.ComplaintEvent) error {
	return s.Scan(
		&c.ComplaintID, &c.TenantID, &c.NotificationID, &c.Provider,
		&c.ComplaintType, &c.RecipientAddress, &c.ReceivedAt,
		&c.ProcessedAt, &c.ActionTaken, &c.SuppressionID,
	)
}

func (s *PgStore) CreateComplaintEvent(ctx context.Context, c *domain.ComplaintEvent) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if c.ComplaintID == "" {
		c.ComplaintID = uuid.NewString()
	}
	if c.ReceivedAt.IsZero() {
		c.ReceivedAt = time.Now().UTC()
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO complaint_events (complaint_id, tenant_id, notification_id, provider, complaint_type, recipient_address, received_at, processed_at, action_taken, suppression_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, c.ComplaintID, tenantID, c.NotificationID, c.Provider, c.ComplaintType,
			c.RecipientAddress, c.ReceivedAt, c.ProcessedAt, c.ActionTaken, c.SuppressionID)
		return err
	})
}

func (s *PgStore) GetComplaintEvent(ctx context.Context, id string) (*domain.ComplaintEvent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var c domain.ComplaintEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanComplaintEvent(tx.QueryRow(ctx, `
			SELECT `+complaintEventColumns+`
			FROM complaint_events
			WHERE complaint_id = $1 AND tenant_id = $2
		`, id, tenantID), &c)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrComplaintNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &c, nil
}

func (s *PgStore) ListComplaintEvents(ctx context.Context, f domain.ComplaintEventFilter) ([]domain.ComplaintEvent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ComplaintEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + complaintEventColumns + ` FROM complaint_events WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.NotificationID != "" {
			args = append(args, f.NotificationID)
			query += fmt.Sprintf(" AND notification_id = $%d", len(args))
		}
		if f.RecipientAddr != "" {
			args = append(args, f.RecipientAddr)
			query += fmt.Sprintf(" AND recipient_address = $%d", len(args))
		}
		if f.ComplaintType != "" {
			args = append(args, f.ComplaintType)
			query += fmt.Sprintf(" AND complaint_type = $%d", len(args))
		}

		query += " ORDER BY received_at DESC"
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
			var c domain.ComplaintEvent
			if err := scanComplaintEvent(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── NCD-04: Channel Reputation ────────────────────────────────────────────────

const channelReputationColumns = `
	reputation_id, tenant_id, channel, provider, window_start, window_end,
	sent_count, accepted_count, bounced_count, complained_count,
	delivered_count, read_count, reputation_score, created_at, updated_at`

func scanChannelReputation(s scannable, r *domain.ChannelReputation) error {
	return s.Scan(
		&r.ReputationID, &r.TenantID, &r.Channel, &r.Provider,
		&r.WindowStart, &r.WindowEnd, &r.SentCount, &r.AcceptedCount,
		&r.BouncedCount, &r.ComplainedCount, &r.DeliveredCount, &r.ReadCount,
		&r.ReputationScore, &r.CreatedAt, &r.UpdatedAt,
	)
}

func (s *PgStore) UpsertChannelReputation(ctx context.Context, r *domain.ChannelReputation) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if r.ReputationID == "" {
		r.ReputationID = uuid.NewString()
	}
	r.UpdatedAt = time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO channel_reputation (reputation_id, tenant_id, channel, provider, window_start, window_end,
				sent_count, accepted_count, bounced_count, complained_count, delivered_count, read_count,
				reputation_score, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT (tenant_id, channel, provider, window_start) DO UPDATE SET
				window_end = EXCLUDED.window_end,
				sent_count = channel_reputation.sent_count + EXCLUDED.sent_count,
				accepted_count = channel_reputation.accepted_count + EXCLUDED.accepted_count,
				bounced_count = channel_reputation.bounced_count + EXCLUDED.bounced_count,
				complained_count = channel_reputation.complained_count + EXCLUDED.complained_count,
				delivered_count = channel_reputation.delivered_count + EXCLUDED.delivered_count,
				read_count = channel_reputation.read_count + EXCLUDED.read_count,
				reputation_score = EXCLUDED.reputation_score,
				updated_at = EXCLUDED.updated_at
		`, r.ReputationID, tenantID, r.Channel, r.Provider, r.WindowStart, r.WindowEnd,
			r.SentCount, r.AcceptedCount, r.BouncedCount, r.ComplainedCount,
			r.DeliveredCount, r.ReadCount, r.ReputationScore, r.CreatedAt, r.UpdatedAt)
		return err
	})
}

func (s *PgStore) ListChannelReputations(ctx context.Context, f domain.ChannelReputationFilter) ([]domain.ChannelReputation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ChannelReputation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + channelReputationColumns + ` FROM channel_reputation WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.Channel != "" {
			args = append(args, f.Channel)
			query += fmt.Sprintf(" AND channel = $%d", len(args))
		}
		if f.Provider != "" {
			args = append(args, f.Provider)
			query += fmt.Sprintf(" AND provider = $%d", len(args))
		}
		if !f.Since.IsZero() {
			args = append(args, f.Since)
			query += fmt.Sprintf(" AND window_start >= $%d", len(args))
		}
		if !f.Until.IsZero() {
			args = append(args, f.Until)
			query += fmt.Sprintf(" AND window_start <= $%d", len(args))
		}

		query += " ORDER BY window_start DESC"
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
			var r domain.ChannelReputation
			if err := scanChannelReputation(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── NCD-01: Communication Intent ──────────────────────────────────────────────

const communicationIntentColumns = `
	intent_id, tenant_id, legal_entity_id, name, description, category, channels, created_by, created_at, updated_at`

func scanCommunicationIntent(s scannable, i *domain.CommunicationIntent) error {
	return s.Scan(
		&i.IntentID, &i.TenantID, &i.LegalEntityID, &i.Name, &i.Description,
		&i.Category, &i.Channels, &i.CreatedBy, &i.CreatedAt, &i.UpdatedAt,
	)
}

func (s *PgStore) CreateCommunicationIntent(ctx context.Context, i *domain.CommunicationIntent) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if i.IntentID == "" {
		i.IntentID = uuid.NewString()
	}
	if i.CreatedAt.IsZero() {
		i.CreatedAt = time.Now().UTC()
	}
	if i.UpdatedAt.IsZero() {
		i.UpdatedAt = time.Now().UTC()
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO communication_intents (intent_id, tenant_id, legal_entity_id, name, description, category, channels, created_by, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, i.IntentID, tenantID, i.LegalEntityID, i.Name, i.Description, i.Category, i.Channels, i.CreatedBy, i.CreatedAt, i.UpdatedAt)
		return err
	})
}

func (s *PgStore) GetCommunicationIntent(ctx context.Context, id string) (*domain.CommunicationIntent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var i domain.CommunicationIntent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanCommunicationIntent(tx.QueryRow(ctx, `
			SELECT `+communicationIntentColumns+`
			FROM communication_intents
			WHERE intent_id = $1 AND tenant_id = $2
		`, id, tenantID), &i)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntentNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &i, nil
}

func (s *PgStore) ListCommunicationIntents(ctx context.Context, f domain.CommunicationIntentFilter) ([]domain.CommunicationIntent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.CommunicationIntent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + communicationIntentColumns + ` FROM communication_intents WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.LegalEntityID != "" {
			args = append(args, f.LegalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if f.Name != "" {
			args = append(args, f.Name)
			query += fmt.Sprintf(" AND name = $%d", len(args))
		}
		if f.Category != "" {
			args = append(args, f.Category)
			query += fmt.Sprintf(" AND category = $%d", len(args))
		}

		query += " ORDER BY created_at DESC"
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
			var i domain.CommunicationIntent
			if err := scanCommunicationIntent(rows, &i); err != nil {
				return err
			}
			out = append(out, i)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) UpdateCommunicationIntent(ctx context.Context, i *domain.CommunicationIntent) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	i.UpdatedAt = time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE communication_intents
			SET name = $1, description = $2, category = $3, channels = $4, updated_at = $5
			WHERE intent_id = $6 AND tenant_id = $7
		`, i.Name, i.Description, i.Category, i.Channels, i.UpdatedAt, i.IntentID, tenantID)
		return err
	})
}

func (s *PgStore) DeleteCommunicationIntent(ctx context.Context, id string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			DELETE FROM communication_intents WHERE intent_id = $1 AND tenant_id = $2
		`, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrIntentNotFound
		}
		return nil
	})
}

// ── NCD-01: Template ──────────────────────────────────────────────────────────

const templateColumns = `
	template_id, intent_id, tenant_id, legal_entity_id, locale, version,
	subject_template, body_template, variables, status,
	approved_by, approved_at, published_at, effective_from, effective_to,
	created_by, created_at, updated_at`

func scanTemplate(s scannable, t *domain.Template) error {
	var varsJSON []byte
	err := s.Scan(
		&t.TemplateID, &t.IntentID, &t.TenantID, &t.LegalEntityID, &t.Locale, &t.Version,
		&t.SubjectTemplate, &t.BodyTemplate, &varsJSON, &t.Status,
		&t.ApprovedBy, &t.ApprovedAt, &t.PublishedAt, &t.EffectiveFrom, &t.EffectiveTo,
		&t.CreatedBy, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if varsJSON != nil {
		_ = json.Unmarshal(varsJSON, &t.Variables)
	}
	return nil
}

func (s *PgStore) CreateTemplate(ctx context.Context, t *domain.Template) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if t.TemplateID == "" {
		t.TemplateID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now().UTC()
	}
	varsJSON, _ := json.Marshal(t.Variables)
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO templates (template_id, intent_id, tenant_id, legal_entity_id, locale, version,
				subject_template, body_template, variables, status,
				approved_by, approved_at, published_at, effective_from, effective_to,
				created_by, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		`, t.TemplateID, t.IntentID, tenantID, t.LegalEntityID, t.Locale, t.Version,
			t.SubjectTemplate, t.BodyTemplate, varsJSON, t.Status,
			t.ApprovedBy, t.ApprovedAt, t.PublishedAt, t.EffectiveFrom, t.EffectiveTo,
			t.CreatedBy, t.CreatedAt, t.UpdatedAt)
		return err
	})
}

func (s *PgStore) GetTemplate(ctx context.Context, id string) (*domain.Template, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var t domain.Template
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTemplate(tx.QueryRow(ctx, `
			SELECT `+templateColumns+`
			FROM templates
			WHERE template_id = $1 AND tenant_id = $2
		`, id, tenantID), &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &t, nil
}

func (s *PgStore) GetTemplateByIntent(ctx context.Context, intentID, locale string, version int) (*domain.Template, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var t domain.Template
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTemplate(tx.QueryRow(ctx, `
			SELECT `+templateColumns+`
			FROM templates
			WHERE intent_id = $1 AND tenant_id = $2 AND locale = $3 AND version = $4
		`, intentID, tenantID, locale, version), &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &t, nil
}

func (s *PgStore) GetEffectiveTemplate(ctx context.Context, tenantID, intentID, locale string, at time.Time) (*domain.Template, error) {
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var t domain.Template
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTemplate(tx.QueryRow(ctx, `
			SELECT `+templateColumns+`
			FROM templates
			WHERE intent_id = $1 AND tenant_id = $2 AND locale = $3
			  AND status = 'published'
			  AND (effective_from IS NULL OR effective_from <= $4)
			  AND (effective_to IS NULL OR effective_to > $4)
			ORDER BY version DESC
			LIMIT 1
		`, intentID, tenantID, locale, at), &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &t, nil
}

func (s *PgStore) ListTemplates(ctx context.Context, f domain.TemplateFilter) ([]domain.Template, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Template
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + templateColumns + ` FROM templates WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.LegalEntityID != "" {
			args = append(args, f.LegalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if f.IntentID != "" {
			args = append(args, f.IntentID)
			query += fmt.Sprintf(" AND intent_id = $%d", len(args))
		}
		if f.Locale != "" {
			args = append(args, f.Locale)
			query += fmt.Sprintf(" AND locale = $%d", len(args))
		}
		if f.Status != "" {
			args = append(args, f.Status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}

		query += " ORDER BY created_at DESC"
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
			var t domain.Template
			if err := scanTemplate(rows, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) UpdateTemplate(ctx context.Context, t *domain.Template) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	t.UpdatedAt = time.Now().UTC()
	varsJSON, _ := json.Marshal(t.Variables)
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE templates
			SET subject_template = $1, body_template = $2, variables = $3, status = $4,
			    approved_by = $5, approved_at = $6, published_at = $7,
			    effective_from = $8, effective_to = $9, updated_at = $10
			WHERE template_id = $11 AND tenant_id = $12
		`, t.SubjectTemplate, t.BodyTemplate, varsJSON, t.Status,
			t.ApprovedBy, t.ApprovedAt, t.PublishedAt, t.EffectiveFrom, t.EffectiveTo,
			t.UpdatedAt, t.TemplateID, tenantID)
		return err
	})
}

func (s *PgStore) DeleteTemplate(ctx context.Context, id string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			DELETE FROM templates WHERE template_id = $1 AND tenant_id = $2
		`, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrTemplateNotFound
		}
		return nil
	})
}

// ── NCD-01: Template Approval ─────────────────────────────────────────────────

const templateApprovalColumns = `
	approval_id, template_id, tenant_id, requested_by, approved_by,
	status, reason, requested_at, decided_at`

func scanTemplateApproval(s scannable, a *domain.TemplateApproval) error {
	return s.Scan(
		&a.ApprovalID, &a.TemplateID, &a.TenantID, &a.RequestedBy,
		&a.ApprovedBy, &a.Status, &a.Reason, &a.RequestedAt, &a.DecidedAt,
	)
}

func (s *PgStore) CreateTemplateApproval(ctx context.Context, a *domain.TemplateApproval) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if a.ApprovalID == "" {
		a.ApprovalID = uuid.NewString()
	}
	if a.RequestedAt.IsZero() {
		a.RequestedAt = time.Now().UTC()
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO template_approvals (approval_id, template_id, tenant_id, requested_by, approved_by, status, reason, requested_at, decided_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, a.ApprovalID, a.TemplateID, tenantID, a.RequestedBy, a.ApprovedBy, a.Status, a.Reason, a.RequestedAt, a.DecidedAt)
		return err
	})
}

func (s *PgStore) GetTemplateApproval(ctx context.Context, id string) (*domain.TemplateApproval, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var a domain.TemplateApproval
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTemplateApproval(tx.QueryRow(ctx, `
			SELECT `+templateApprovalColumns+`
			FROM template_approvals
			WHERE approval_id = $1 AND tenant_id = $2
		`, id, tenantID), &a)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrApprovalNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &a, nil
}

func (s *PgStore) ListTemplateApprovals(ctx context.Context, f domain.TemplateApprovalFilter) ([]domain.TemplateApproval, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.TemplateApproval
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + templateApprovalColumns + ` FROM template_approvals WHERE tenant_id = $1`
		args := []any{tenantID}

		if f.TemplateID != "" {
			args = append(args, f.TemplateID)
			query += fmt.Sprintf(" AND template_id = $%d", len(args))
		}
		if f.Status != "" {
			args = append(args, f.Status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}

		query += " ORDER BY requested_at DESC"
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
			var a domain.TemplateApproval
			if err := scanTemplateApproval(rows, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) DecideTemplateApproval(ctx context.Context, approvalID, approverID, status, reason string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE template_approvals
			SET status = $1, approved_by = $2, reason = $3, decided_at = $4
			WHERE approval_id = $5 AND tenant_id = $6 AND status = 'pending'
		`, status, approverID, reason, now, approvalID, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrApprovalNotFound
		}
		return nil
	})
}

// ── NCD-01: Template Render ───────────────────────────────────────────────────

const templateRenderColumns = `
	render_id, template_id, tenant_id, variables, rendered_subject, rendered_body, error, created_by, created_at`

func scanTemplateRender(s scannable, r *domain.TemplateRender) error {
	var varsJSON []byte
	err := s.Scan(
		&r.RenderID, &r.TemplateID, &r.TenantID, &varsJSON,
		&r.RenderedSubject, &r.RenderedBody, &r.Error,
		&r.CreatedBy, &r.CreatedAt,
	)
	if err != nil {
		return err
	}
	if varsJSON != nil {
		_ = json.Unmarshal(varsJSON, &r.Variables)
	}
	return nil
}

func (s *PgStore) CreateTemplateRender(ctx context.Context, r *domain.TemplateRender) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if r.RenderID == "" {
		r.RenderID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	varsJSON, _ := json.Marshal(r.Variables)
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO template_renders (render_id, template_id, tenant_id, variables, rendered_subject, rendered_body, error, created_by, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, r.RenderID, r.TemplateID, tenantID, varsJSON, r.RenderedSubject, r.RenderedBody, r.Error, r.CreatedBy, r.CreatedAt)
		return err
	})
}

func (s *PgStore) GetTemplateRender(ctx context.Context, id string) (*domain.TemplateRender, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var r domain.TemplateRender
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTemplateRender(tx.QueryRow(ctx, `
			SELECT `+templateRenderColumns+`
			FROM template_renders
			WHERE render_id = $1 AND tenant_id = $2
		`, id, tenantID), &r)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateNotFound // reusing error
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &r, nil
}

func (s *PgStore) ListTemplateRenders(ctx context.Context, templateID string, limit, offset int) ([]domain.TemplateRender, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.TemplateRender
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+templateRenderColumns+`
			FROM template_renders
			WHERE template_id = $1 AND tenant_id = $2
			ORDER BY created_at DESC
			LIMIT $3 OFFSET $4
		`, templateID, tenantID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var r domain.TemplateRender
			if err := scanTemplateRender(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── NCD-05: Regulated Notice ──────────────────────────────────────────────────

const regulatedNoticeColumns = `
	regulated_notice_id, tenant_id, legal_entity_id, intent_id, template_id,
	recipient_principal_id, recipient_address, subject, body, variables,
	channel, status, priority, expires_at, acknowledged_at, acknowledged_by,
	acknowledgment_method, acknowledgment_chain, created_by, created_at, updated_at`

func scanRegulatedNotice(s scannable, n *domain.RegulatedNotice) error {
	var varsJSON, chainJSON []byte
	err := s.Scan(
		&n.RegulatedNoticeID, &n.TenantID, &n.LegalEntityID, &n.IntentID, &n.TemplateID,
		&n.RecipientPrincipalID, &n.RecipientAddress, &n.Subject, &n.Body, &varsJSON,
		&n.Channel, &n.Status, &n.Priority, &n.ExpiresAt, &n.AcknowledgedAt, &n.AcknowledgedBy,
		&n.AcknowledgmentMethod, &chainJSON, &n.CreatedBy, &n.CreatedAt, &n.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if varsJSON != nil {
		_ = json.Unmarshal(varsJSON, &n.Variables)
	}
	if chainJSON != nil {
		_ = json.Unmarshal(chainJSON, &n.AcknowledgmentChain)
	}
	return nil
}

func (s *PgStore) CreateRegulatedNotice(ctx context.Context, n *domain.RegulatedNotice) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if n.RegulatedNoticeID == "" {
		n.RegulatedNoticeID = uuid.NewString()
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now().UTC()
	}
	if n.UpdatedAt.IsZero() {
		n.UpdatedAt = time.Now().UTC()
	}
	varsJSON, _ := json.Marshal(n.Variables)
	chainJSON, _ := json.Marshal(n.AcknowledgmentChain)
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO regulated_notices (regulated_notice_id, tenant_id, legal_entity_id, intent_id, template_id,
				recipient_principal_id, recipient_address, subject, body, variables,
				channel, status, priority, expires_at, acknowledged_at, acknowledged_by,
				acknowledgment_method, acknowledgment_chain, created_by, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		`, n.RegulatedNoticeID, tenantID, n.LegalEntityID, n.IntentID, n.TemplateID,
			n.RecipientPrincipalID, n.RecipientAddress, n.Subject, n.Body, varsJSON,
			n.Channel, n.Status, n.Priority, n.ExpiresAt, n.AcknowledgedAt, n.AcknowledgedBy,
			n.AcknowledgmentMethod, chainJSON, n.CreatedBy, n.CreatedAt, n.UpdatedAt)
		return err
	})
}

func (s *PgStore) GetRegulatedNotice(ctx context.Context, id string) (*domain.RegulatedNotice, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var n domain.RegulatedNotice
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanRegulatedNotice(tx.QueryRow(ctx, `
			SELECT `+regulatedNoticeColumns+`
			FROM regulated_notices
			WHERE regulated_notice_id = $1 AND tenant_id = $2
		`, id, tenantID), &n)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRegulatedNoticeNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &n, nil
}

func (s *PgStore) ListRegulatedNotices(ctx context.Context, f domain.RegulatedNoticeFilter) ([]domain.RegulatedNotice, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.RegulatedNotice
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + regulatedNoticeColumns + ` FROM regulated_notices WHERE tenant_id = $1`
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
		if f.Priority != "" {
			args = append(args, f.Priority)
			query += fmt.Sprintf(" AND priority = $%d", len(args))
		}

		query += " ORDER BY created_at DESC"
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
			var n domain.RegulatedNotice
			if err := scanRegulatedNotice(rows, &n); err != nil {
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

func (s *PgStore) UpdateRegulatedNotice(ctx context.Context, n *domain.RegulatedNotice) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	n.UpdatedAt = time.Now().UTC()
	varsJSON, _ := json.Marshal(n.Variables)
	chainJSON, _ := json.Marshal(n.AcknowledgmentChain)
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE regulated_notices
			SET subject = $1, body = $2, variables = $3, status = $4, priority = $5,
			    expires_at = $6, acknowledged_at = $7, acknowledged_by = $8,
			    acknowledgment_method = $9, acknowledgment_chain = $10, updated_at = $11
			WHERE regulated_notice_id = $12 AND tenant_id = $13
		`, n.Subject, n.Body, varsJSON, n.Status, n.Priority,
			n.ExpiresAt, n.AcknowledgedAt, n.AcknowledgedBy,
			n.AcknowledgmentMethod, chainJSON, n.UpdatedAt, n.RegulatedNoticeID, tenantID)
		return err
	})
}

// ── NCD-05: Acknowledgment Chain ──────────────────────────────────────────────

const acknowledgmentChainColumns = `
	chain_id, regulated_notice_id, tenant_id, step_number, action, actor, method, evidence, metadata, created_at`

func scanAcknowledgmentChainStep(s scannable, c *domain.AcknowledgmentChainStep) error {
	var evidenceJSON, metadataJSON []byte
	err := s.Scan(
		&c.ChainID, &c.RegulatedNoticeID, &c.TenantID, &c.StepNumber, &c.Action,
		&c.Actor, &c.Method, &evidenceJSON, &metadataJSON, &c.CreatedAt,
	)
	if err != nil {
		return err
	}
	if evidenceJSON != nil {
		_ = json.Unmarshal(evidenceJSON, &c.Evidence)
	}
	if metadataJSON != nil {
		_ = json.Unmarshal(metadataJSON, &c.Metadata)
	}
	return nil
}

func (s *PgStore) CreateAcknowledgmentChainStep(ctx context.Context, c *domain.AcknowledgmentChainStep) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if c.ChainID == "" {
		c.ChainID = uuid.NewString()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	evidenceJSON, _ := json.Marshal(c.Evidence)
	metadataJSON, _ := json.Marshal(c.Metadata)
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO acknowledgment_chain (chain_id, regulated_notice_id, tenant_id, step_number, action, actor, method, evidence, metadata, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, c.ChainID, c.RegulatedNoticeID, tenantID, c.StepNumber, c.Action, c.Actor, c.Method, evidenceJSON, metadataJSON, c.CreatedAt)
		return err
	})
}

func (s *PgStore) GetAcknowledgmentChain(ctx context.Context, regulatedNoticeID string) ([]domain.AcknowledgmentChainStep, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.AcknowledgmentChainStep
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+acknowledgmentChainColumns+`
			FROM acknowledgment_chain
			WHERE regulated_notice_id = $1 AND tenant_id = $2
			ORDER BY step_number
		`, regulatedNoticeID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.AcknowledgmentChainStep
			if err := scanAcknowledgmentChainStep(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) AcknowledgeRegulatedNotice(ctx context.Context, req *domain.AcknowledgeRequest, actorID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Get current notice
		var n domain.RegulatedNotice
		err := scanRegulatedNotice(tx.QueryRow(ctx, `
			SELECT `+regulatedNoticeColumns+`
			FROM regulated_notices
			WHERE regulated_notice_id = $1 AND tenant_id = $2
			FOR UPDATE
		`, req.RegulatedNoticeID, tenantID), &n)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRegulatedNoticeNotFound
		}
		if err != nil {
			return err
		}

		if n.Status == domain.RegulatedNoticeStatusAcknowledged {
			return domain.ErrAlreadyAcknowledged
		}
		if n.ExpiresAt != nil && time.Now().UTC().After(*n.ExpiresAt) {
			return domain.ErrNoticeExpired
		}

		now := time.Now().UTC()
		n.Status = domain.RegulatedNoticeStatusAcknowledged
		n.AcknowledgedAt = &now
		n.AcknowledgedBy = actorID
		n.AcknowledgmentMethod = req.Method
		n.UpdatedAt = now

		evidence := req.Evidence
		if evidence == nil {
			evidence = make(map[string]any)
		}
		if req.WitnessPrincipalID != "" {
			evidence["witness"] = req.WitnessPrincipalID
		}
		if req.DigitalSignature != "" {
			evidence["digital_signature"] = req.DigitalSignature
		}

		chain := n.AcknowledgmentChain
		if chain == nil {
			chain = make(map[string]any)
		}
		chain[fmt.Sprintf("step_%d", len(chain)+1)] = map[string]any{
			"action":      "acknowledged",
			"actor":       actorID,
			"method":      req.Method,
			"evidence":    evidence,
			"timestamp":   now,
		}
		n.AcknowledgmentChain = chain

		chainJSON, _ := json.Marshal(n.AcknowledgmentChain)
		evidenceJSON, _ := json.Marshal(evidence)

		_, err = tx.Exec(ctx, `
			UPDATE regulated_notices
			SET status = $1, acknowledged_at = $2, acknowledged_by = $3,
			    acknowledgment_method = $4, acknowledgment_chain = $5, updated_at = $6
			WHERE regulated_notice_id = $7 AND tenant_id = $8
		`, n.Status, n.AcknowledgedAt, n.AcknowledgedBy, n.AcknowledgmentMethod, chainJSON, n.UpdatedAt, n.RegulatedNoticeID, tenantID)
		if err != nil {
			return err
		}

		// Add chain step
		stepNumber := len(chain) + 1
		_, err = tx.Exec(ctx, `
			INSERT INTO acknowledgment_chain (chain_id, regulated_notice_id, tenant_id, step_number, action, actor, method, evidence, metadata, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, uuid.NewString(), n.RegulatedNoticeID, tenantID, stepNumber, "acknowledged", actorID, req.Method,
			evidenceJSON, nil, now)
		return err
	})
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
