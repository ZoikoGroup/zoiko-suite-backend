// COM-05 Platform Commercial Billing persistence, part 5b (migration 000013).
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/outbox"
)

// PaymentStore is the COM-05 persistence contract, part 5b.
type PaymentStore interface {
	CollectPayment(ctx context.Context, attemptID, invoiceID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PaymentAttemptRef, error)
	RecordProviderOutcome(ctx context.Context, req domain.RecordOutcomeRequest, claim domain.IdempotencyClaim) (*domain.PaymentAttemptRef, error)
	GetPaymentAttempt(ctx context.Context, attemptID string) (*domain.PaymentAttemptRef, error)
	GetPaymentAttempts(ctx context.Context, invoiceID string) ([]domain.PaymentAttemptRef, error)
	GetCollectionState(ctx context.Context, invoiceID string) (domain.CollectionState, error)
}

var _ PaymentStore = (*PgStore)(nil)

var paymentImmutableTables = []string{"payment attempt"}

func mapPaymentErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "CP001":
		for _, t := range paymentImmutableTables {
			if strings.Contains(pgErr.Message, t) {
				return fmt.Errorf("%w: %s", domain.ErrPaymentAttemptInvalidState, pgErr.Message)
			}
		}
		return err
	case "23505":
		if pgErr.ConstraintName == "idx_payment_attempts_provider_event_id" {
			return domain.ErrDuplicateProviderEvent
		}
	}
	return err
}

func (s *PgStore) paymentTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapPaymentErr(s.withTenant(ctx, fn))
}

func (s *PgStore) paymentSellerTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapPaymentErr(s.withSellerPlane(ctx, fn))
}

const paymentAttemptColumns = `attempt_id, organization_id::text, invoice_id, amount::text, currency_code, status,
	provider_attempt_ref, provider_event_id, settlement_ref, failure_reason, created_at, created_by_principal_id,
	submitted_at, resolved_at, resolved_by_principal_id`

func scanPaymentAttempt(row pgx.Row) (*domain.PaymentAttemptRef, error) {
	var a domain.PaymentAttemptRef
	if err := row.Scan(&a.AttemptID, &a.OrganizationID, &a.InvoiceID, &a.Amount, &a.CurrencyCode, &a.Status,
		&a.ProviderAttemptRef, &a.ProviderEventID, &a.SettlementRef, &a.FailureReason, &a.CreatedAt, &a.CreatedByPrincipalID,
		&a.SubmittedAt, &a.ResolvedAt, &a.ResolvedByPrincipalID); err != nil {
		return nil, err
	}
	return &a, nil
}

func loadPaymentAttempt(ctx context.Context, tx pgx.Tx, attemptID string, forUpdate bool) (*domain.PaymentAttemptRef, error) {
	q := `SELECT ` + paymentAttemptColumns + ` FROM payment_attempts WHERE attempt_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	a, err := scanPaymentAttempt(tx.QueryRow(ctx, q, attemptID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPaymentAttemptNotFound
	}
	return a, err
}

// findUnresolvedOrSucceeded returns the invoice's current SUCCEEDED attempt
// (if any) or its current unresolved (CREATED/SUBMITTED/PENDING_UNKNOWN)
// attempt (if any) — the two states that make a second CollectPayment call
// meaningless: either it is already paid, or there is already something to
// wait for (COM-CTRL-026; negative path #27).
func findUnresolvedOrSucceeded(ctx context.Context, tx pgx.Tx, invoiceID string) (*domain.PaymentAttemptRef, error) {
	a, err := scanPaymentAttempt(tx.QueryRow(ctx, `SELECT `+paymentAttemptColumns+` FROM payment_attempts
		WHERE invoice_id = $1 AND status IN ('CREATED', 'SUBMITTED', 'PENDING_UNKNOWN', 'SUCCEEDED')
		ORDER BY (status = 'SUCCEEDED') DESC, created_at DESC LIMIT 1`, invoiceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// CollectPayment creates the durable attempt and marks it SUBMITTED in the
// same transaction (COM-CTRL-023: durable before submit — this service has
// no external processor client of its own to gate on; the row's existence
// is the durability boundary an adapter integration would later dispatch
// from). A second call while an attempt is unresolved, or after one has
// already succeeded, returns the existing attempt rather than creating a
// competing one.
func (s *PgStore) CollectPayment(ctx context.Context, attemptID, invoiceID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PaymentAttemptRef, error) {
	var out *domain.PaymentAttemptRef
	err := s.paymentSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		inv, err := loadInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		existing, err := findUnresolvedOrSucceeded(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Status == domain.PaymentSucceeded {
				return domain.ErrInvoiceAlreadyPaid
			}
			out = existing
			return nil
		}
		got, err := scanPaymentAttempt(tx.QueryRow(ctx, `
			INSERT INTO payment_attempts (attempt_id, organization_id, invoice_id, amount, currency_code, status,
				created_at, created_by_principal_id, submitted_at)
			VALUES ($1, $2, $3, $4::numeric, $5, 'SUBMITTED', $6, $7, $6) RETURNING `+paymentAttemptColumns,
			attemptID, inv.OrganizationID, invoiceID, inv.TotalAmount, inv.CurrencyCode, now, actor))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "payment_attempt", AggregateID: attemptID,
			EventType: "payment_attempt.created", TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

// RecordProviderOutcome applies what a provider adapter or webhook reported.
// A replay of the exact same outcome against an already-resolved attempt
// (same status, and the same or no provider_event_id) is answered
// idempotently rather than rejected — a duplicate callback for a settled
// attempt is expected traffic, not a protocol violation (negative path
// #28). Any other attempt to move a resolved attempt is refused (negative
// path #29).
func (s *PgStore) RecordProviderOutcome(ctx context.Context, req domain.RecordOutcomeRequest, claim domain.IdempotencyClaim) (*domain.PaymentAttemptRef, error) {
	var out *domain.PaymentAttemptRef
	err := s.paymentSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		a, err := loadPaymentAttempt(ctx, tx, req.AttemptID, true)
		if err != nil {
			return err
		}
		if a.Status.Resolved() {
			sameEvent := (req.ProviderEventID == nil && a.ProviderEventID == nil) ||
				(req.ProviderEventID != nil && a.ProviderEventID != nil && *req.ProviderEventID == *a.ProviderEventID)
			if a.Status == req.Outcome && sameEvent {
				out = a
				return nil
			}
			return fmt.Errorf("%w: attempt is already resolved as %s", domain.ErrPaymentAttemptInvalidState, a.Status)
		}

		resolved := req.Outcome.Resolved()
		var resolvedAt *time.Time
		var resolvedBy *string
		if resolved {
			t := req.OccurredAt
			resolvedAt, resolvedBy = &t, &req.ActorPrincipalID
		}
		providerAttemptRef := a.ProviderAttemptRef
		if req.ProviderAttemptRef != nil {
			providerAttemptRef = req.ProviderAttemptRef
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_attempts SET status = $2, provider_attempt_ref = $3, provider_event_id = $4,
				settlement_ref = $5, failure_reason = $6, resolved_at = $7, resolved_by_principal_id = $8
			WHERE attempt_id = $1`,
			req.AttemptID, req.Outcome, providerAttemptRef, req.ProviderEventID, req.SettlementRef, req.FailureReason,
			resolvedAt, resolvedBy); err != nil {
			return err
		}
		got, err := loadPaymentAttempt(ctx, tx, req.AttemptID, false)
		if err != nil {
			return err
		}
		out = got
		eventType := "payment_attempt.outcome_resolved"
		if got.Status == domain.PaymentSucceeded {
			eventType = "platform_payment.settled"
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "payment_attempt", AggregateID: req.AttemptID,
			EventType: eventType, TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetPaymentAttempt(ctx context.Context, attemptID string) (*domain.PaymentAttemptRef, error) {
	var out *domain.PaymentAttemptRef
	err := s.paymentTx(ctx, func(tx pgx.Tx) error {
		a, err := loadPaymentAttempt(ctx, tx, attemptID, false)
		out = a
		return err
	})
	return out, err
}

func (s *PgStore) GetPaymentAttempts(ctx context.Context, invoiceID string) ([]domain.PaymentAttemptRef, error) {
	var out []domain.PaymentAttemptRef
	err := s.paymentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+paymentAttemptColumns+` FROM payment_attempts
			WHERE invoice_id = $1 ORDER BY created_at`, invoiceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanPaymentAttempt(rows)
			if err != nil {
				return err
			}
			out = append(out, *a)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetCollectionState(ctx context.Context, invoiceID string) (domain.CollectionState, error) {
	attempts, err := s.GetPaymentAttempts(ctx, invoiceID)
	if err != nil {
		return "", err
	}
	return domain.CollectionStateOf(attempts), nil
}
