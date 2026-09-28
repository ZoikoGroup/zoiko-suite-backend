// COM-05 Platform Commercial Billing persistence, part 5d (migration 000015).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/outbox"
)

// DunningStore is the COM-05 persistence contract, part 5d.
type DunningStore interface {
	PublishDunningPolicy(ctx context.Context, p *domain.DunningPolicyVersion, claim domain.IdempotencyClaim) (*domain.DunningPolicyVersion, error)
	GetEffectiveDunningPolicy(ctx context.Context, now time.Time) (*domain.DunningPolicyVersion, error)

	StartDunning(ctx context.Context, caseID, invoiceID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DunningCase, error)
	AdvanceDunning(ctx context.Context, caseID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DunningCase, error)
	StopDunning(ctx context.Context, caseID, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.DunningCase, error)
	GetDunningCase(ctx context.Context, caseID string) (*domain.DunningCase, error)
	GetDunningStateForInvoice(ctx context.Context, invoiceID string) (*domain.DunningCase, error)

	ReconcileCommercialAccount(ctx context.Context, organizationID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.CommercialReconciliation, error)
	GetCommercialReconciliation(ctx context.Context, organizationID string) (*domain.CommercialReconciliation, error)
}

var _ DunningStore = (*PgStore)(nil)

var dunningImmutableTables = []string{"dunning case", "dunning_policy_versions"}

func mapDunningErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "CP001":
		for _, t := range dunningImmutableTables {
			if strings.Contains(pgErr.Message, t) {
				return fmt.Errorf("%w: %s", domain.ErrDunningCaseInvalidState, pgErr.Message)
			}
		}
	case "23505":
		if pgErr.ConstraintName == "idx_dunning_cases_one_open_per_invoice" {
			return domain.ErrDunningCaseAlreadyOpenForInvoice
		}
	}
	return err
}

func (s *PgStore) dunningSellerTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapDunningErr(s.withSellerPlane(ctx, fn))
}

func (s *PgStore) dunningTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapDunningErr(s.withTenant(ctx, fn))
}

// ── Dunning policy ───────────────────────────────────────────────────────────

const dunningPolicyColumns = `policy_version, notice1_after_days, notice2_after_days, restrict_after_days,
	suspend_after_days, effective_from, reason, created_at, created_by_principal_id`

func scanDunningPolicy(row pgx.Row) (*domain.DunningPolicyVersion, error) {
	var p domain.DunningPolicyVersion
	if err := row.Scan(&p.PolicyVersion, &p.Notice1AfterDays, &p.Notice2AfterDays, &p.RestrictAfterDays,
		&p.SuspendAfterDays, &p.EffectiveFrom, &p.Reason, &p.CreatedAt, &p.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PgStore) PublishDunningPolicy(ctx context.Context, p *domain.DunningPolicyVersion, claim domain.IdempotencyClaim) (*domain.DunningPolicyVersion, error) {
	var out *domain.DunningPolicyVersion
	err := s.dunningSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanDunningPolicy(tx.QueryRow(ctx, `
			INSERT INTO dunning_policy_versions (policy_version, notice1_after_days, notice2_after_days,
				restrict_after_days, suspend_after_days, effective_from, reason, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+dunningPolicyColumns,
			p.PolicyVersion, p.Notice1AfterDays, p.Notice2AfterDays, p.RestrictAfterDays, p.SuspendAfterDays,
			p.EffectiveFrom, p.Reason, p.CreatedAt, p.CreatedByPrincipalID))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dunning_policy", AggregateID: fmt.Sprint(got.PolicyVersion),
			EventType: "dunning_policy.published", Payload: got})
	})
	return out, err
}

func loadEffectiveDunningPolicy(ctx context.Context, tx pgx.Tx, now time.Time) (*domain.DunningPolicyVersion, error) {
	p, err := scanDunningPolicy(tx.QueryRow(ctx, `SELECT `+dunningPolicyColumns+` FROM dunning_policy_versions
		WHERE effective_from <= $1 ORDER BY effective_from DESC, policy_version DESC LIMIT 1`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDunningPolicyNotFound
	}
	return p, err
}

func (s *PgStore) GetEffectiveDunningPolicy(ctx context.Context, now time.Time) (*domain.DunningPolicyVersion, error) {
	var out *domain.DunningPolicyVersion
	err := s.dunningTx(ctx, func(tx pgx.Tx) error {
		p, err := loadEffectiveDunningPolicy(ctx, tx, now)
		out = p
		return err
	})
	return out, err
}

// ── Dunning cases ────────────────────────────────────────────────────────────

const dunningCaseColumns = `case_id, organization_id::text, invoice_id, policy_version, status, opened_at,
	opened_by_principal_id, last_advanced_at, closed_at, closed_by_principal_id, close_reason`

func scanDunningCase(row pgx.Row) (*domain.DunningCase, error) {
	var c domain.DunningCase
	if err := row.Scan(&c.CaseID, &c.OrganizationID, &c.InvoiceID, &c.PolicyVersion, &c.Status, &c.OpenedAt,
		&c.OpenedByPrincipalID, &c.LastAdvancedAt, &c.ClosedAt, &c.ClosedByPrincipalID, &c.CloseReason); err != nil {
		return nil, err
	}
	return &c, nil
}

func loadDunningCase(ctx context.Context, tx pgx.Tx, caseID string, forUpdate bool) (*domain.DunningCase, error) {
	q := `SELECT ` + dunningCaseColumns + ` FROM dunning_cases WHERE case_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	c, err := scanDunningCase(tx.QueryRow(ctx, q, caseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDunningCaseNotFound
	}
	return c, err
}

func (s *PgStore) StartDunning(ctx context.Context, caseID, invoiceID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DunningCase, error) {
	var out *domain.DunningCase
	err := s.dunningSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		inv, err := loadInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		policy, err := loadEffectiveDunningPolicy(ctx, tx, now)
		if err != nil {
			return err
		}
		got, err := scanDunningCase(tx.QueryRow(ctx, `
			INSERT INTO dunning_cases (case_id, organization_id, invoice_id, policy_version, status, opened_at, opened_by_principal_id)
			VALUES ($1, $2, $3, $4, 'NOTICE_1', $5, $6) RETURNING `+dunningCaseColumns,
			caseID, inv.OrganizationID, invoiceID, policy.PolicyVersion, now, actor))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dunning_case", AggregateID: caseID,
			EventType: "dunning.started", TenantID: &inv.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) AdvanceDunning(ctx context.Context, caseID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DunningCase, error) {
	var out *domain.DunningCase
	err := s.dunningSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		c, err := loadDunningCase(ctx, tx, caseID, true)
		if err != nil {
			return err
		}
		if c.Status == domain.DunningClosed {
			return fmt.Errorf("%w: case is closed", domain.ErrDunningCaseInvalidState)
		}
		next := domain.NextDunningStage(c.Status)
		if next == "" {
			return domain.ErrDunningCaseFullyEscalated
		}
		if _, err := tx.Exec(ctx, `UPDATE dunning_cases SET status = $2, last_advanced_at = $3 WHERE case_id = $1`,
			caseID, next, now); err != nil {
			return err
		}
		got, err := loadDunningCase(ctx, tx, caseID, false)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dunning_case", AggregateID: caseID,
			EventType: "dunning.advanced", TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) StopDunning(ctx context.Context, caseID, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.DunningCase, error) {
	var out *domain.DunningCase
	err := s.dunningSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		c, err := loadDunningCase(ctx, tx, caseID, true)
		if err != nil {
			return err
		}
		if c.Status == domain.DunningClosed {
			out = c // idempotent: already closed.
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE dunning_cases SET status = 'CLOSED', closed_at = $2, closed_by_principal_id = $3,
			close_reason = $4 WHERE case_id = $1`, caseID, now, actor, reason); err != nil {
			return err
		}
		got, err := loadDunningCase(ctx, tx, caseID, false)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dunning_case", AggregateID: caseID,
			EventType: "dunning.ended", TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetDunningCase(ctx context.Context, caseID string) (*domain.DunningCase, error) {
	var out *domain.DunningCase
	err := s.dunningTx(ctx, func(tx pgx.Tx) error {
		c, err := loadDunningCase(ctx, tx, caseID, false)
		out = c
		return err
	})
	return out, err
}

// GetDunningStateForInvoice returns the invoice's current OPEN case, or nil
// (with no error) if none is open — the invoice being current, or its case
// already closed, are both ordinary states, not failures.
func (s *PgStore) GetDunningStateForInvoice(ctx context.Context, invoiceID string) (*domain.DunningCase, error) {
	var out *domain.DunningCase
	err := s.dunningTx(ctx, func(tx pgx.Tx) error {
		c, err := scanDunningCase(tx.QueryRow(ctx, `SELECT `+dunningCaseColumns+` FROM dunning_cases
			WHERE invoice_id = $1 AND status <> 'CLOSED'`, invoiceID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = c
		return err
	})
	return out, err
}

// ── Commercial reconciliation ────────────────────────────────────────────────

// reconcileInvoice re-verifies, for one invoice, every invariant this
// service already enforces at write time: header/line totals agree, what
// was collected never exceeds what was billed, credits+write-offs never
// exceed the invoice, and no attempt's refunds exceed what it collected.
// Finding nothing here does not mean these checks are redundant — it means
// nothing has bypassed the application layer since they were last enforced.
func reconcileInvoice(ctx context.Context, tx pgx.Tx, inv *domain.PlatformCommercialInvoice) ([]domain.ReconciliationException, error) {
	var exceptions []domain.ReconciliationException
	add := func(check, detail string) {
		exceptions = append(exceptions, domain.ReconciliationException{InvoiceID: inv.InvoiceID, Check: check, Detail: detail})
	}

	currency, err := loadCurrency(ctx, tx, inv.CurrencyCode)
	if err != nil {
		return nil, err
	}
	lines, err := loadInvoiceLines(ctx, tx, inv.InvoiceID)
	if err != nil {
		return nil, err
	}
	computedSubtotal := domain.SumLines(lines, currency.MinorUnits)
	if decCmp(computedSubtotal, inv.SubtotalAmount) != 0 {
		add("line_totals_mismatch", fmt.Sprintf("lines sum to %s, header subtotal is %s", computedSubtotal, inv.SubtotalAmount))
	}
	if decCmp(decAdd(inv.SubtotalAmount, inv.TaxAmount), inv.TotalAmount) != 0 {
		add("header_total_mismatch", fmt.Sprintf("subtotal+tax = %s, header total is %s", decAdd(inv.SubtotalAmount, inv.TaxAmount), inv.TotalAmount))
	}

	var collected string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM payment_attempts
		WHERE invoice_id = $1 AND status = 'SUCCEEDED'`, inv.InvoiceID).Scan(&collected); err != nil {
		return nil, err
	}
	if decCmp(collected, inv.TotalAmount) > 0 {
		add("overcollected", fmt.Sprintf("succeeded payment attempts total %s, invoice total is %s", collected, inv.TotalAmount))
	}

	committed, err := committedAgainstInvoice(ctx, tx, inv.InvoiceID)
	if err != nil {
		return nil, err
	}
	if decCmp(committed, inv.TotalAmount) > 0 {
		add("credits_exceed_invoice", fmt.Sprintf("credit notes + write-offs total %s, invoice total is %s", committed, inv.TotalAmount))
	}

	rows, err := tx.Query(ctx, `SELECT attempt_id, amount::text FROM payment_attempts WHERE invoice_id = $1 AND status = 'SUCCEEDED'`, inv.InvoiceID)
	if err != nil {
		return nil, err
	}
	type attempt struct{ id, amount string }
	var attempts []attempt
	for rows.Next() {
		var a attempt
		if err := rows.Scan(&a.id, &a.amount); err != nil {
			rows.Close()
			return nil, err
		}
		attempts = append(attempts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, a := range attempts {
		var refunded string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM refund_requests
			WHERE payment_attempt_id = $1 AND status IN ('REQUESTED', 'SETTLED')`, a.id).Scan(&refunded); err != nil {
			return nil, err
		}
		if decCmp(refunded, a.amount) > 0 {
			add("refund_exceeds_attempt", fmt.Sprintf("payment attempt %s: refunds total %s, attempt collected %s", a.id, refunded, a.amount))
		}
	}
	return exceptions, nil
}

func (s *PgStore) ReconcileCommercialAccount(ctx context.Context, organizationID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.CommercialReconciliation, error) {
	var out *domain.CommercialReconciliation
	err := s.dunningSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+invoiceColumns+` FROM platform_commercial_invoices WHERE organization_id = $1`, organizationID)
		if err != nil {
			return err
		}
		var invoices []*domain.PlatformCommercialInvoice
		for rows.Next() {
			inv, err := scanInvoice(rows)
			if err != nil {
				rows.Close()
				return err
			}
			invoices = append(invoices, inv)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		var exceptions []domain.ReconciliationException
		for _, inv := range invoices {
			exc, err := reconcileInvoice(ctx, tx, inv)
			if err != nil {
				return err
			}
			exceptions = append(exceptions, exc...)
		}
		if exceptions == nil {
			exceptions = []domain.ReconciliationException{}
		}
		status := domain.ReconciliationClean
		if len(exceptions) > 0 {
			status = domain.ReconciliationExceptionsOpen
		}
		excJSON, err := json.Marshal(exceptions)
		if err != nil {
			return err
		}

		id := domain.NewCommercialID(domain.PrefixReconciliation)
		if _, err := tx.Exec(ctx, `
			INSERT INTO commercial_reconciliations (reconciliation_id, organization_id, status, invoice_count,
				exception_count, exceptions, run_at, run_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, organizationID, status, len(invoices), len(exceptions), excJSON, now, actor); err != nil {
			return err
		}
		out = &domain.CommercialReconciliation{ReconciliationID: id, OrganizationID: organizationID, Status: status,
			InvoiceCount: len(invoices), ExceptionCount: len(exceptions), Exceptions: exceptions, RunAt: now, RunByPrincipalID: actor}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "commercial_reconciliation", AggregateID: id,
			EventType: "commercial_account.reconciled", TenantID: &organizationID, Payload: out})
	})
	return out, err
}

func (s *PgStore) GetCommercialReconciliation(ctx context.Context, organizationID string) (*domain.CommercialReconciliation, error) {
	var out *domain.CommercialReconciliation
	err := s.dunningTx(ctx, func(tx pgx.Tx) error {
		var r domain.CommercialReconciliation
		var excJSON []byte
		err := tx.QueryRow(ctx, `SELECT reconciliation_id, organization_id::text, status, invoice_count, exception_count,
			exceptions, run_at, run_by_principal_id FROM commercial_reconciliations
			WHERE organization_id = $1 ORDER BY run_at DESC LIMIT 1`, organizationID).Scan(
			&r.ReconciliationID, &r.OrganizationID, &r.Status, &r.InvoiceCount, &r.ExceptionCount, &excJSON, &r.RunAt, &r.RunByPrincipalID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrReconciliationNotFound
		}
		if err != nil {
			return err
		}
		if len(excJSON) > 0 {
			if err := json.Unmarshal(excJSON, &r.Exceptions); err != nil {
				return fmt.Errorf("decode exceptions: %w", err)
			}
		}
		out = &r
		return nil
	})
	return out, err
}
