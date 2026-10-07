package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
)

// BeginSubmission records, in its own committed transaction, that this
// notification is about to be handed to a provider (migration 000013).
//
// Committed BEFORE the provider call on purpose: if the process then dies, the
// marker survives and the stranded sweep knows the attempt may have reached
// the provider. Cleared by the transition that records the attempt's effect.
//
// Guarded on PENDING with nothing scheduled — the in-flight state a send or a
// claimed retry is in. A row that concluded or was re-scheduled by a racing
// replica matches nothing, and the caller must not submit.
func (s *PgStore) BeginSubmission(ctx context.Context, id, tenantID string, at time.Time) error {
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE notifications SET submitting_since = $1
			WHERE notification_id = $2 AND tenant_id = $3
			  AND status = 'PENDING' AND next_attempt_at IS NULL
		`, at, id, tenantID)
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

// MarkStrandedUnknown moves a stranded notification that WAS handed to a
// provider — submitting_since set, older than staleBefore — to
// PENDING_UNKNOWN, records the lost attempt as UNKNOWN and enqueues
// notification.outcome_unknown, all in one transaction. Returns whether this
// caller did it.
//
// §6.2: "UNKNOWN never authorizes an immediate blind second material send."
// The provider may already have this message; only a person reconciling
// against the provider's records (ResolveDeliveryOutcome) may move it on.
//
// The staleness predicate is repeated in the UPDATE and is the claim, the same
// shape as ReviveStranded: a row that concluded between the find and this
// statement no longer matches.
func (s *PgStore) MarkStrandedUnknown(ctx context.Context, id, tenantID string, staleBefore, at time.Time) (bool, error) {
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	const reason = "outcome lost: the process stopped after handing this notification to a provider " +
		"and before recording what the provider said; it may or may not have been delivered"
	var marked bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var n domain.Notification
		err := scanNotification(tx.QueryRow(ctx, `
			UPDATE notifications
			SET status            = 'PENDING_UNKNOWN',
			    failure_reason    = $1,
			    sent_at           = submitting_since,
			    unknown_at        = $2,
			    delivery_attempts = delivery_attempts + 1,
			    last_attempt_at   = submitting_since,
			    submitting_since  = NULL,
			    next_attempt_at   = NULL
			WHERE notification_id = $3 AND tenant_id = $4
			  AND status = 'PENDING' AND next_attempt_at IS NULL
			  AND submitting_since IS NOT NULL AND submitting_since <= $5
			RETURNING `+notificationColumns,
			reason, at, id, tenantID, staleBefore), &n)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		marked = true
		// Which path made the lost attempt cannot be known; the first attempt
		// of any notification is made inside the send request, later ones by
		// the retry worker.
		origin := domain.AttemptOriginRetry
		if n.DeliveryAttempts == 1 {
			origin = domain.AttemptOriginRequest
		}
		attemptedAt := at
		if n.LastAttemptAt != nil {
			attemptedAt = *n.LastAttemptAt
		}
		if err := recordAttempt(ctx, tx, n, domain.AttemptOutcomeUnknown, "", reason, attemptedAt,
			domain.AttemptMeta{Origin: origin}); err != nil {
			return err
		}
		ev, err := events.OutcomeUnknown(n.CorrelationID, n, reason)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
	})
	if err != nil {
		return false, mapPgError(err)
	}
	return marked, nil
}

// BeginResend reopens a concluded notification for ONE explicit, reasoned
// resend attempt (§3.4, migration 000014), returning the reopened row.
//
// Only SENT or FAILED may be resent. PENDING is still being attempted, and
// PENDING_UNKNOWN must be resolved against the provider first — resending it is
// the blind second material send §6.2 forbids. The guard is in the UPDATE, so
// two concurrent resends cannot both reopen the same notification: the second
// matches nothing (the row is PENDING by then) and is refused.
//
// The reason is required here and again on the attempt row
// (nda_resend_has_reason), where the evidence chain keeps it permanently.
func (s *PgStore) BeginResend(ctx context.Context, id, tenantID, actorPrincipalID, reason string, at time.Time) (*domain.Notification, error) {
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if reason == "" {
		return nil, domain.ErrResendReasonRequired
	}
	var n domain.Notification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanNotification(tx.QueryRow(ctx, `
			UPDATE notifications
			SET status                      = 'PENDING',
			    next_attempt_at             = NULL,
			    resend_count                = resend_count + 1,
			    last_resend_reason          = $1,
			    last_resent_at              = $2,
			    last_resent_by_principal_id = $3
			WHERE notification_id = $4 AND tenant_id = $5 AND status IN ('SENT', 'FAILED')
			  AND message_intent_id IS NULL
			RETURNING `+notificationColumns,
			reason, at, actorPrincipalID, id, tenantID), &n)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Tell a ledger-owned communication apart from one that simply is not in
		// a resendable state, so the caller is told the real reason.
		var ledgerOwned bool
		_ = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT message_intent_id IS NOT NULL FROM notifications
				WHERE notification_id = $1 AND tenant_id = $2 AND status IN ('SENT', 'FAILED')`, id, tenantID).Scan(&ledgerOwned)
		})
		if ledgerOwned {
			return nil, domain.ErrResendLedgerOwned
		}
		return nil, domain.ErrNotResendable
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &n, nil
}
