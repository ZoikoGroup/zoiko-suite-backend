package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// recordAttempt writes one durable attempt row (migration 000011) on the
// caller's open transaction.
//
// Unexported and tx-bound for the same reason as enqueue: an attempt record
// written outside the transaction that records the attempt's effect could
// exist for an effect that was rolled back, or be missing for one that was
// kept. CompleteDelivery, ScheduleRetry and MarkOutcomeUnknown are the only
// callers, and n is the row their UPDATE returned — so attempt_number is the
// committed delivery_attempts, not a caller's count.
func recordAttempt(ctx context.Context, tx pgx.Tx, n domain.Notification, outcome, providerResponse, failureReason string,
	attemptedAt time.Time, meta domain.AttemptMeta) error {
	origin := meta.Origin
	if origin == "" {
		origin = domain.AttemptOriginRequest
	}
	actor := meta.ActorPrincipalID
	if actor == "" {
		actor = n.CreatedByPrincipalID
	}
	attemptID := uuid.NewString()
	// The id a provider callback will quote, recorded only for an accepted
	// attempt: a failed or ambiguous attempt has no message to point at, and
	// inventing one would let a callback attach itself to the wrong row.
	var providerMessageID string
	if outcome == domain.AttemptOutcomeAccepted {
		providerMessageID = domain.ExtractProviderMessageID(providerResponse)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO notification_delivery_attempts (
			attempt_id, tenant_id, notification_id, attempt_number, origin, channel,
			provider_name, outcome, provider_response, failure_reason, retryable,
			resend_reason, actor_principal_id, attempted_at, provider_message_id,
			privacy_decision_id, privacy_result
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	`, attemptID, n.TenantID, n.NotificationID, n.DeliveryAttempts, origin, n.Channel,
		nullIfEmpty(meta.ProviderName), outcome, nullIfEmpty(providerResponse), nullIfEmpty(failureReason),
		meta.Retryable, nullIfEmpty(meta.ResendReason), nullIfEmpty(actor), attemptedAt, nullIfEmpty(providerMessageID),
		nullIfEmpty(meta.PrivacyDecisionID), nullIfEmpty(meta.PrivacyResult))
	if err != nil {
		return fmt.Errorf("record delivery attempt %d: %w", n.DeliveryAttempts, err)
	}

	// §10.2 attempt events, in the same transaction as the row they describe.
	a := domain.DeliveryAttempt{
		AttemptID: attemptID, TenantID: n.TenantID, NotificationID: n.NotificationID,
		AttemptNumber: n.DeliveryAttempts, Origin: origin, Channel: n.Channel, ProviderName: meta.ProviderName,
		Outcome: outcome, ProviderResponse: providerResponse, FailureReason: failureReason,
		Retryable: meta.Retryable, ResendReason: meta.ResendReason, ActorPrincipalID: actor, AttemptedAt: attemptedAt,
		PrivacyDecisionID: meta.PrivacyDecisionID, PrivacyResult: meta.PrivacyResult,
	}
	ev, err := events.AttemptCreated(n.CorrelationID, n, a)
	if err != nil {
		return err
	}
	if err := enqueue(ctx, tx, n.TenantID, ev); err != nil {
		return err
	}
	if meta.BlockCode != "" {
		blocked, err := events.CommunicationBlocked(n.CorrelationID, n, meta.BlockCode, n.DeliveryAttempts, meta.Retryable, meta.PrivacyDecisionID)
		if err != nil {
			return err
		}
		if err := enqueue(ctx, tx, n.TenantID, blocked); err != nil {
			return err
		}
	}
	if outcome == domain.AttemptOutcomeUnknown {
		ev, err := events.AttemptUnknown(n.CorrelationID, n, a, attemptedAt.Add(UnknownResolutionWindow))
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, n.TenantID, ev)
	}
	return nil
}

// UnknownResolutionWindow is how long an ambiguous attempt may stay unresolved
// before it is overdue. Carried as resolution_due_at on delivery.attempt.unknown
// (§10.2) so an operator dashboard or escalation can act on it.
const UnknownResolutionWindow = 24 * time.Hour

// ListAttempts returns every recorded attempt for one notification, oldest
// first — the evidence chain §3.4 requires a resend to preserve.
func (s *PgStore) ListAttempts(ctx context.Context, notificationID string) ([]domain.DeliveryAttempt, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.DeliveryAttempt
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT attempt_id, tenant_id, notification_id, attempt_number, origin, channel,
			       COALESCE(provider_name, ''), outcome, COALESCE(provider_response, ''),
			       COALESCE(failure_reason, ''), retryable, COALESCE(resend_reason, ''),
			       COALESCE(actor_principal_id, ''), attempted_at, recorded_at,
			       COALESCE(privacy_decision_id, ''), COALESCE(privacy_result, '')
			FROM notification_delivery_attempts
			WHERE tenant_id = $1 AND notification_id = $2
			ORDER BY attempt_number
		`, tenantID, notificationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.DeliveryAttempt
			if err := rows.Scan(&a.AttemptID, &a.TenantID, &a.NotificationID, &a.AttemptNumber, &a.Origin, &a.Channel,
				&a.ProviderName, &a.Outcome, &a.ProviderResponse, &a.FailureReason, &a.Retryable,
				&a.ResendReason, &a.ActorPrincipalID, &a.AttemptedAt, &a.RecordedAt,
				&a.PrivacyDecisionID, &a.PrivacyResult); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}
