// COM-05 Platform Commercial Billing persistence, part 5c (migration 000014).
package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/money"
	"zoiko.io/commercial-account-svc/internal/outbox"
)

// CreditStore is the COM-05 persistence contract, part 5c.
type CreditStore interface {
	IssueCreditNote(ctx context.Context, creditNoteID, invoiceID, amount, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.CreditNote, error)
	ApplyWriteOff(ctx context.Context, writeOffID, invoiceID, amount, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.WriteOff, error)
	RequestRefund(ctx context.Context, refundID, invoiceID, paymentAttemptID, amount, destinationRef, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.RefundRequest, error)
	SettleRefund(ctx context.Context, req domain.SettleRefundRequest, claim domain.IdempotencyClaim) (*domain.RefundRequest, error)
	GetCreditNotes(ctx context.Context, invoiceID string) ([]domain.CreditNote, error)
	GetWriteOffs(ctx context.Context, invoiceID string) ([]domain.WriteOff, error)
	GetRefundRequest(ctx context.Context, refundID string) (*domain.RefundRequest, error)
	GetRefundRequests(ctx context.Context, invoiceID string) ([]domain.RefundRequest, error)
	GetBalance(ctx context.Context, organizationID string) (*domain.OutstandingBalance, error)
}

var _ CreditStore = (*PgStore)(nil)

var creditImmutableTables = []string{"credit_notes", "write_offs", "refund request"}

func mapCreditErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code == "CP001" {
		for _, t := range creditImmutableTables {
			if strings.Contains(pgErr.Message, t) {
				return fmt.Errorf("%w: %s", domain.ErrRefundInvalidState, pgErr.Message)
			}
		}
	}
	return err
}

func (s *PgStore) creditSellerTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapCreditErr(s.withSellerPlane(ctx, fn))
}

func (s *PgStore) creditTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapCreditErr(s.withTenant(ctx, fn))
}

// lockInvoiceForCredit locks the invoice as the serialization point for
// concurrent credit notes/write-offs against it, and returns its total and
// currency.
func lockInvoiceForCredit(ctx context.Context, tx pgx.Tx, invoiceID string) (organizationID, currencyCode, total string, err error) {
	err = tx.QueryRow(ctx, `SELECT organization_id::text, currency_code, total_amount::text
		FROM platform_commercial_invoices WHERE invoice_id = $1 FOR UPDATE`, invoiceID).Scan(&organizationID, &currencyCode, &total)
	if errors.Is(err, pgx.ErrNoRows) {
		err = domain.ErrInvoiceNotFound
	}
	return
}

// committedAgainstInvoice sums every credit note and write-off already
// issued against an invoice — the shared pool both draw down, since both
// reduce what is owed rather than what was collected.
func committedAgainstInvoice(ctx context.Context, tx pgx.Tx, invoiceID string) (string, error) {
	var credited, writtenOff string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM credit_notes WHERE invoice_id = $1`, invoiceID).Scan(&credited); err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM write_offs WHERE invoice_id = $1`, invoiceID).Scan(&writtenOff); err != nil {
		return "", err
	}
	return decAdd(credited, writtenOff), nil
}

// decAdd/decSub/decCmp do exact decimal arithmetic over stored money
// amounts (already at the currency's minor-unit scale) via money.Decimal's
// exact big.Rat representation — never float64 (ZS-SVC-Q-001 negative path
// #46), and always re-formatted to a plain decimal string a NUMERIC column
// or a later money.Parse can accept.
func decAdd(a, b string) string {
	x, y := money.MustParse(a).Rat(), money.MustParse(b).Rat()
	return money.FormatHalfEven(new(big.Rat).Add(x, y), money.MaxScale)
}

func decSub(a, b string) string {
	x, y := money.MustParse(a).Rat(), money.MustParse(b).Rat()
	return money.FormatHalfEven(new(big.Rat).Sub(x, y), money.MaxScale)
}

func decCmp(a, b string) int {
	return money.MustParse(a).Cmp(money.MustParse(b))
}

// decSubScale subtracts and formats at an explicit scale — GetBalance
// reports every amount at the billing account's actual currency minor
// units, not a blanket internal scale.
func decSubScale(a, b string, scale int) string {
	x, y := money.MustParse(a).Rat(), money.MustParse(b).Rat()
	return money.FormatHalfEven(new(big.Rat).Sub(x, y), scale)
}

// ── Credit notes ─────────────────────────────────────────────────────────────

const creditNoteColumns = `credit_note_id, organization_id::text, invoice_id, amount::text, currency_code, reason,
	issued_at, issued_by_principal_id`

func scanCreditNote(row pgx.Row) (*domain.CreditNote, error) {
	var c domain.CreditNote
	if err := row.Scan(&c.CreditNoteID, &c.OrganizationID, &c.InvoiceID, &c.Amount, &c.CurrencyCode, &c.Reason,
		&c.IssuedAt, &c.IssuedByPrincipalID); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) IssueCreditNote(ctx context.Context, creditNoteID, invoiceID, amount, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.CreditNote, error) {
	var out *domain.CreditNote
	err := s.creditSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		orgID, currency, total, err := lockInvoiceForCredit(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		committed, err := committedAgainstInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if decCmp(decAdd(committed, amount), total) > 0 {
			return domain.ErrCreditExceedsInvoice
		}
		got, err := scanCreditNote(tx.QueryRow(ctx, `
			INSERT INTO credit_notes (credit_note_id, organization_id, invoice_id, amount, currency_code, reason,
				issued_at, issued_by_principal_id)
			VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, $8) RETURNING `+creditNoteColumns,
			creditNoteID, orgID, invoiceID, amount, currency, reason, now, actor))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "credit_note", AggregateID: creditNoteID,
			EventType: "credit_note.issued", TenantID: &orgID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetCreditNotes(ctx context.Context, invoiceID string) ([]domain.CreditNote, error) {
	var out []domain.CreditNote
	err := s.creditTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+creditNoteColumns+` FROM credit_notes WHERE invoice_id = $1 ORDER BY issued_at`, invoiceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCreditNote(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

// ── Write-offs ───────────────────────────────────────────────────────────────

const writeOffColumns = `write_off_id, organization_id::text, invoice_id, amount::text, currency_code, reason,
	applied_at, applied_by_principal_id`

func scanWriteOff(row pgx.Row) (*domain.WriteOff, error) {
	var w domain.WriteOff
	if err := row.Scan(&w.WriteOffID, &w.OrganizationID, &w.InvoiceID, &w.Amount, &w.CurrencyCode, &w.Reason,
		&w.AppliedAt, &w.AppliedByPrincipalID); err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *PgStore) ApplyWriteOff(ctx context.Context, writeOffID, invoiceID, amount, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.WriteOff, error) {
	var out *domain.WriteOff
	err := s.creditSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		orgID, currency, total, err := lockInvoiceForCredit(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		committed, err := committedAgainstInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if decCmp(decAdd(committed, amount), total) > 0 {
			return domain.ErrWriteOffExceedsInvoice
		}
		got, err := scanWriteOff(tx.QueryRow(ctx, `
			INSERT INTO write_offs (write_off_id, organization_id, invoice_id, amount, currency_code, reason,
				applied_at, applied_by_principal_id)
			VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, $8) RETURNING `+writeOffColumns,
			writeOffID, orgID, invoiceID, amount, currency, reason, now, actor))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "write_off", AggregateID: writeOffID,
			EventType: "write_off.applied", TenantID: &orgID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetWriteOffs(ctx context.Context, invoiceID string) ([]domain.WriteOff, error) {
	var out []domain.WriteOff
	err := s.creditTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+writeOffColumns+` FROM write_offs WHERE invoice_id = $1 ORDER BY applied_at`, invoiceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w, err := scanWriteOff(rows)
			if err != nil {
				return err
			}
			out = append(out, *w)
		}
		return rows.Err()
	})
	return out, err
}

// ── Refunds ──────────────────────────────────────────────────────────────────

const refundColumns = `refund_id, organization_id::text, invoice_id, payment_attempt_id, amount::text, currency_code,
	destination_ref, destination_fingerprint, reason, status, requested_at, requested_by_principal_id,
	settlement_ref, failure_reason, resolved_at, resolved_by_principal_id`

func scanRefund(row pgx.Row) (*domain.RefundRequest, error) {
	var r domain.RefundRequest
	if err := row.Scan(&r.RefundID, &r.OrganizationID, &r.InvoiceID, &r.PaymentAttemptID, &r.Amount, &r.CurrencyCode,
		&r.DestinationRef, &r.DestinationFingerprint, &r.Reason, &r.Status, &r.RequestedAt, &r.RequestedByPrincipalID,
		&r.SettlementRef, &r.FailureReason, &r.ResolvedAt, &r.ResolvedByPrincipalID); err != nil {
		return nil, err
	}
	return &r, nil
}

func loadRefund(ctx context.Context, tx pgx.Tx, refundID string, forUpdate bool) (*domain.RefundRequest, error) {
	q := `SELECT ` + refundColumns + ` FROM refund_requests WHERE refund_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	r, err := scanRefund(tx.QueryRow(ctx, q, refundID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRefundRequestNotFound
	}
	return r, err
}

func (s *PgStore) RequestRefund(ctx context.Context, refundID, invoiceID, paymentAttemptID, amount, destinationRef, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.RefundRequest, error) {
	var out *domain.RefundRequest
	err := s.creditSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		attempt, err := loadPaymentAttempt(ctx, tx, paymentAttemptID, true)
		if err != nil {
			return err
		}
		if attempt.InvoiceID != invoiceID {
			return domain.ErrPaymentAttemptNotFound
		}
		if attempt.Status != domain.PaymentSucceeded {
			return domain.ErrPaymentAttemptNotSucceeded
		}
		var alreadyRefunded string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM refund_requests
			WHERE payment_attempt_id = $1 AND status IN ('REQUESTED', 'SETTLED')`, paymentAttemptID).Scan(&alreadyRefunded); err != nil {
			return err
		}
		if decCmp(decAdd(alreadyRefunded, amount), attempt.Amount) > 0 {
			return domain.ErrRefundExceedsCollected
		}
		fingerprint := domain.DestinationFingerprint(destinationRef)
		got, err := scanRefund(tx.QueryRow(ctx, `
			INSERT INTO refund_requests (refund_id, organization_id, invoice_id, payment_attempt_id, amount,
				currency_code, destination_ref, destination_fingerprint, reason, status, requested_at, requested_by_principal_id)
			VALUES ($1, $2, $3, $4, $5::numeric, $6, $7, $8, $9, 'REQUESTED', $10, $11) RETURNING `+refundColumns,
			refundID, attempt.OrganizationID, invoiceID, paymentAttemptID, amount, attempt.CurrencyCode,
			destinationRef, fingerprint, reason, now, actor))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "refund_request", AggregateID: refundID,
			EventType: "refund.requested", TenantID: &attempt.OrganizationID, Payload: got})
	})
	return out, err
}

// SettleRefund applies a refund's provider outcome. COM-CTRL-028: the
// destination presented here must fingerprint-match what the refund was
// requested against, or settlement is refused (negative path #31) — a
// changed destination means a new refund must be requested, never a
// silent redirect of an already-approved one. An exact replay (same
// outcome, same destination) against an already-resolved refund is
// answered idempotently.
func (s *PgStore) SettleRefund(ctx context.Context, req domain.SettleRefundRequest, claim domain.IdempotencyClaim) (*domain.RefundRequest, error) {
	var out *domain.RefundRequest
	err := s.creditSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		r, err := loadRefund(ctx, tx, req.RefundID, true)
		if err != nil {
			return err
		}
		fingerprint := domain.DestinationFingerprint(req.DestinationRef)
		if r.Status != domain.RefundRequested {
			if r.Status == domain.RefundStatus(req.Outcome) && fingerprint == r.DestinationFingerprint {
				out = r
				return nil
			}
			return fmt.Errorf("%w: refund is already resolved as %s", domain.ErrRefundInvalidState, r.Status)
		}
		if fingerprint != r.DestinationFingerprint {
			return domain.ErrRefundDestinationMismatch
		}
		if _, err := tx.Exec(ctx, `UPDATE refund_requests SET status = $2, settlement_ref = $3, failure_reason = $4,
			resolved_at = $5, resolved_by_principal_id = $6 WHERE refund_id = $1`,
			req.RefundID, req.Outcome, req.SettlementRef, req.FailureReason, req.OccurredAt, req.ActorPrincipalID); err != nil {
			return err
		}
		got, err := loadRefund(ctx, tx, req.RefundID, false)
		if err != nil {
			return err
		}
		out = got
		eventType := "refund.failed"
		if got.Status == domain.RefundSettled {
			eventType = "refund.settled"
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "refund_request", AggregateID: req.RefundID,
			EventType: eventType, TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetRefundRequest(ctx context.Context, refundID string) (*domain.RefundRequest, error) {
	var out *domain.RefundRequest
	err := s.creditTx(ctx, func(tx pgx.Tx) error {
		r, err := loadRefund(ctx, tx, refundID, false)
		out = r
		return err
	})
	return out, err
}

func (s *PgStore) GetRefundRequests(ctx context.Context, invoiceID string) ([]domain.RefundRequest, error) {
	var out []domain.RefundRequest
	err := s.creditTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+refundColumns+` FROM refund_requests WHERE invoice_id = $1 ORDER BY requested_at`, invoiceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRefund(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// ── Outstanding balance ──────────────────────────────────────────────────────

func (s *PgStore) GetBalance(ctx context.Context, organizationID string) (*domain.OutstandingBalance, error) {
	var out *domain.OutstandingBalance
	err := s.creditTx(ctx, func(tx pgx.Tx) error {
		ba, err := loadBillingAccountByOrg(ctx, tx, organizationID)
		if err != nil {
			return err
		}
		currency, err := loadCurrency(ctx, tx, ba.BillingCurrencyCode)
		if err != nil {
			return err
		}
		var invoiced, credited, writtenOff, collected, refunded string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(total_amount), 0)::text FROM platform_commercial_invoices
			WHERE organization_id = $1`, organizationID).Scan(&invoiced); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM credit_notes WHERE organization_id = $1`,
			organizationID).Scan(&credited); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM write_offs WHERE organization_id = $1`,
			organizationID).Scan(&writtenOff); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM payment_attempts
			WHERE organization_id = $1 AND status = 'SUCCEEDED'`, organizationID).Scan(&collected); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM refund_requests
			WHERE organization_id = $1 AND status = 'SETTLED'`, organizationID).Scan(&refunded); err != nil {
			return err
		}
		owed := decSubScale(decSubScale(invoiced, credited, currency.MinorUnits), writtenOff, currency.MinorUnits)
		netCollected := decSubScale(collected, refunded, currency.MinorUnits)
		balance := decSubScale(owed, netCollected, currency.MinorUnits)
		out = &domain.OutstandingBalance{
			OrganizationID: organizationID, CurrencyCode: ba.BillingCurrencyCode,
			TotalInvoiced: invoiced, TotalCredited: credited, TotalWrittenOff: writtenOff,
			TotalCollected: collected, TotalRefunded: refunded, Balance: balance,
		}
		return nil
	})
	return out, err
}
