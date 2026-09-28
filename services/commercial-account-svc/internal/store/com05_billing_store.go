// COM-05 Platform Commercial Billing persistence, part 5a (migration 000012).
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

// BillingStore is the COM-05 persistence contract, part 5a.
type BillingStore interface {
	OpenBillingAccount(ctx context.Context, b *domain.BillingAccount, claim domain.IdempotencyClaim) (*domain.BillingAccount, error)
	GetBillingAccount(ctx context.Context, organizationID string) (*domain.BillingAccount, error)

	GenerateInvoiceCandidate(ctx context.Context, req domain.GenerateInvoiceCandidateRequest, claim domain.IdempotencyClaim) (*domain.InvoiceCandidate, error)
	ApproveInvoiceCandidate(ctx context.Context, candidateID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.InvoiceCandidate, error)
	IssueInvoice(ctx context.Context, candidateID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PlatformCommercialInvoice, error)

	GetInvoiceCandidate(ctx context.Context, candidateID string) (*domain.InvoiceCandidate, error)
	GetInvoice(ctx context.Context, invoiceID string) (*domain.PlatformCommercialInvoice, error)
	GetInvoiceBasis(ctx context.Context, invoiceID string) (*domain.InvoiceCandidate, error)
}

var _ BillingStore = (*PgStore)(nil)

// billingImmutableTables scopes CP001 rewrites to this file's own tables —
// same disambiguation problem catalogTx's mapPriceBookErr documents: CP001
// is shared by every wave's seller-plane transactions.
var billingImmutableTables = []string{"invoice candidate", "invoice_candidates", "invoice_lines", "platform_commercial_invoices", "billing_accounts"}

func mapBillingErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "CP001":
		for _, t := range billingImmutableTables {
			if strings.Contains(pgErr.Message, t) {
				return fmt.Errorf("%w: %s", domain.ErrInvoiceCandidateInvalidState, pgErr.Message)
			}
		}
		return err
	case "23505":
		switch pgErr.ConstraintName {
		case "billing_accounts_organization_id_key":
			return domain.ErrBillingAccountExists
		case "idx_invoice_candidates_one_in_flight":
			return fmt.Errorf("%w: an invoice candidate for this subscription term is already in flight", domain.ErrInvoiceCandidateInvalidState)
		}
	case "23514":
		if pgErr.ConstraintName == "invoice_candidates_approver_independent" {
			return domain.ErrInvoiceCandidateSelfApproval
		}
	}
	return err
}

func (s *PgStore) billingTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapBillingErr(s.withTenant(ctx, fn))
}

// billingSellerTx runs fn on the seller plane, additionally declaring
// organizationID as the tenant scope when set — required for any read that
// crosses into subscriptions/subscription_terms/subscription_items, which
// carry no seller-plane RLS bypass of their own (same reasoning as COM-04's
// declareOrgForSellerPlane).
func (s *PgStore) billingSellerTx(ctx context.Context, organizationID string, fn func(pgx.Tx) error) error {
	return mapBillingErr(s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if organizationID != "" {
			if err := declareOrgForSellerPlane(ctx, tx, organizationID); err != nil {
				return err
			}
		}
		return fn(tx)
	}))
}

// ── Billing accounts ─────────────────────────────────────────────────────────

const billingAccountColumns = `billing_account_id, organization_id::text, selling_entity, billing_currency_code,
	invoice_numbering_profile, payment_provider_ref, status, created_at, created_by_principal_id`

func scanBillingAccount(row pgx.Row) (*domain.BillingAccount, error) {
	var b domain.BillingAccount
	if err := row.Scan(&b.BillingAccountID, &b.OrganizationID, &b.SellingEntity, &b.BillingCurrencyCode,
		&b.InvoiceNumberingProfile, &b.PaymentProviderRef, &b.Status, &b.CreatedAt, &b.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *PgStore) OpenBillingAccount(ctx context.Context, b *domain.BillingAccount, claim domain.IdempotencyClaim) (*domain.BillingAccount, error) {
	var out *domain.BillingAccount
	err := s.billingSellerTx(ctx, "", func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanBillingAccount(tx.QueryRow(ctx, `
			INSERT INTO billing_accounts (billing_account_id, organization_id, selling_entity, billing_currency_code,
				invoice_numbering_profile, payment_provider_ref, status, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, 'ACTIVE', $7, $8) RETURNING `+billingAccountColumns,
			b.BillingAccountID, b.OrganizationID, b.SellingEntity, b.BillingCurrencyCode,
			b.InvoiceNumberingProfile, b.PaymentProviderRef, b.CreatedAt, b.CreatedByPrincipalID))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "billing_account", AggregateID: got.BillingAccountID,
			EventType: "billing_account.opened", TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

func loadBillingAccountByOrg(ctx context.Context, tx pgx.Tx, organizationID string) (*domain.BillingAccount, error) {
	b, err := scanBillingAccount(tx.QueryRow(ctx, `SELECT `+billingAccountColumns+` FROM billing_accounts WHERE organization_id = $1`, organizationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrBillingAccountNotFound
	}
	return b, err
}

func (s *PgStore) GetBillingAccount(ctx context.Context, organizationID string) (*domain.BillingAccount, error) {
	var out *domain.BillingAccount
	err := s.billingTx(ctx, func(tx pgx.Tx) error {
		b, err := loadBillingAccountByOrg(ctx, tx, organizationID)
		out = b
		return err
	})
	return out, err
}

// ── Invoice candidate lines ──────────────────────────────────────────────────

// candidateLineColumns and issuedLineColumns are deliberately separate:
// invoice_candidate_lines carries statement_total_quantity_at_generate (the
// frozen usage-basis evidence IssueInvoice re-checks for drift);
// invoice_lines, the immutable issued copy, does not.
const candidateLineColumns = `line_no, kind, description, price_version_id, component_key, meter_key,
	statement_id, statement_total_quantity_at_generate::text, quantity::text, unit_amount::text, amount::text`

const issuedLineColumns = `line_no, kind, description, price_version_id, component_key, meter_key,
	statement_id, quantity::text, unit_amount::text, amount::text`

func scanCandidateLine(row pgx.Row) (*domain.InvoiceLine, error) {
	var l domain.InvoiceLine
	if err := row.Scan(&l.LineNo, &l.Kind, &l.Description, &l.PriceVersionID, &l.ComponentKey, &l.MeterKey,
		&l.StatementID, &l.StatementTotalQuantityAtGenerate, &l.Quantity, &l.UnitAmount, &l.Amount); err != nil {
		return nil, err
	}
	return &l, nil
}

func scanIssuedLine(row pgx.Row) (*domain.InvoiceLine, error) {
	var l domain.InvoiceLine
	if err := row.Scan(&l.LineNo, &l.Kind, &l.Description, &l.PriceVersionID, &l.ComponentKey, &l.MeterKey,
		&l.StatementID, &l.Quantity, &l.UnitAmount, &l.Amount); err != nil {
		return nil, err
	}
	return &l, nil
}

func loadCandidateLines(ctx context.Context, tx pgx.Tx, candidateID string) ([]domain.InvoiceLine, error) {
	rows, err := tx.Query(ctx, `SELECT `+candidateLineColumns+` FROM invoice_candidate_lines WHERE candidate_id = $1 ORDER BY line_no`, candidateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.InvoiceLine
	for rows.Next() {
		l, err := scanCandidateLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

func loadInvoiceLines(ctx context.Context, tx pgx.Tx, invoiceID string) ([]domain.InvoiceLine, error) {
	rows, err := tx.Query(ctx, `SELECT `+issuedLineColumns+` FROM invoice_lines WHERE invoice_id = $1 ORDER BY line_no`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.InvoiceLine
	for rows.Next() {
		l, err := scanIssuedLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// ── Invoice candidates ───────────────────────────────────────────────────────

const invoiceCandidateColumns = `candidate_id, organization_id::text, billing_account_id, subscription_id,
	subscription_version_id, term_no, currency_code, status, subtotal_amount::text, tax_jurisdiction_code,
	tax_rate_basis_points, tax_amount::text, total_amount::text, created_at, created_by_principal_id,
	approved_at, approved_by_principal_id, issued_invoice_id`

func scanCandidate(row pgx.Row) (*domain.InvoiceCandidate, error) {
	var c domain.InvoiceCandidate
	if err := row.Scan(&c.CandidateID, &c.OrganizationID, &c.BillingAccountID, &c.SubscriptionID, &c.SubscriptionVersionID,
		&c.TermNo, &c.CurrencyCode, &c.Status, &c.SubtotalAmount, &c.TaxJurisdictionCode, &c.TaxRateBasisPoints,
		&c.TaxAmount, &c.TotalAmount, &c.CreatedAt, &c.CreatedByPrincipalID, &c.ApprovedAt, &c.ApprovedByPrincipalID,
		&c.IssuedInvoiceID); err != nil {
		return nil, err
	}
	return &c, nil
}

func loadCandidate(ctx context.Context, tx pgx.Tx, candidateID string, forUpdate bool) (*domain.InvoiceCandidate, error) {
	q := `SELECT ` + invoiceCandidateColumns + ` FROM invoice_candidates WHERE candidate_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	c, err := scanCandidate(tx.QueryRow(ctx, q, candidateID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvoiceCandidateNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Lines, err = loadCandidateLines(ctx, tx, candidateID)
	return c, err
}

// gatherMeterKeys collects every METERED component's meter_key across the
// price versions an effective configuration's items are bound to.
func gatherMeterKeys(items []domain.SubscriptionItem, pvs map[string]*domain.PriceVersion) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		pv, ok := pvs[it.PriceVersionID]
		if !ok {
			continue
		}
		for _, c := range pv.Components {
			if c.ComponentType == domain.ComponentMetered && c.MeterKey != nil && !seen[*c.MeterKey] {
				seen[*c.MeterKey] = true
				out = append(out, *c.MeterKey)
			}
		}
	}
	return out
}

// loadUsageBasis reads the CERTIFIED/ADJUSTED statement for each of the
// given meters at (subscriptionID, termNo). A meter with no such statement
// is simply absent from the returned map; RateInvoiceBasis is what refuses
// to bill a METERED component that needed one and found none.
func loadUsageBasis(ctx context.Context, tx pgx.Tx, subscriptionID string, termNo int, meterKeys []string) (map[string]domain.UsageBasis, error) {
	out := map[string]domain.UsageBasis{}
	for _, mk := range meterKeys {
		var statementID, totalQuantity string
		err := tx.QueryRow(ctx, `SELECT statement_id, total_quantity::text FROM usage_statements
			WHERE subscription_id = $1 AND term_no = $2 AND meter_key = $3 AND status IN ('CERTIFIED', 'ADJUSTED')`,
			subscriptionID, termNo, mk).Scan(&statementID, &totalQuantity)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[mk] = domain.UsageBasis{StatementID: statementID, TotalQuantity: totalQuantity}
	}
	return out, nil
}

func (s *PgStore) GenerateInvoiceCandidate(ctx context.Context, req domain.GenerateInvoiceCandidateRequest, claim domain.IdempotencyClaim) (*domain.InvoiceCandidate, error) {
	var out *domain.InvoiceCandidate
	err := s.billingSellerTx(ctx, req.OrganizationID, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		ba, err := loadBillingAccountByOrg(ctx, tx, req.OrganizationID)
		if err != nil {
			return err
		}
		if ba.Status != domain.BillingAccountActive {
			return domain.ErrBillingAccountNotActive
		}

		var alreadyIssued bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM invoice_candidates
			WHERE subscription_id = $1 AND term_no = $2 AND status = 'ISSUED')`, req.SubscriptionID, req.TermNo).Scan(&alreadyIssued); err != nil {
			return err
		}
		if alreadyIssued {
			return domain.ErrInvoiceAlreadyIssuedForTerm
		}

		var termStartsAt time.Time
		if err := tx.QueryRow(ctx, `SELECT starts_at FROM subscription_terms WHERE subscription_id = $1 AND term_no = $2`,
			req.SubscriptionID, req.TermNo).Scan(&termStartsAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrSubscriptionNotFound
			}
			return err
		}

		agg, err := loadAggregate(ctx, tx, req.SubscriptionID, false)
		if err != nil {
			return err
		}
		version := effectiveAt(agg.versions, termStartsAt)
		if version == nil {
			return ErrNoEffectiveVersion
		}

		meterKeys := gatherMeterKeys(version.Items, agg.pvs)
		usage, err := loadUsageBasis(ctx, tx, req.SubscriptionID, req.TermNo, meterKeys)
		if err != nil {
			return err
		}
		lines, err := domain.RateInvoiceBasis(version.Items, agg.pvs, usage)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return domain.ErrEmptyInvoiceCandidate
		}

		currency, err := loadCurrency(ctx, tx, agg.sub.CurrencyCode)
		if err != nil {
			return err
		}
		// total is computed from subtotal + tax directly, not by re-summing
		// the raw lines with a tax pseudo-line folded in: two independent
		// half-even roundings of overlapping-but-different inputs are not
		// guaranteed additive, and the DB CHECK requires
		// total_amount = subtotal_amount + tax_amount exactly.
		subtotal := domain.SumLines(lines, currency.MinorUnits)
		subtotalDec, err := money.Parse(subtotal)
		if err != nil {
			return err
		}
		taxDec, err := money.Parse(req.TaxAmount)
		if err != nil {
			return err
		}
		total := money.FormatHalfEven(new(big.Rat).Add(subtotalDec.Rat(), taxDec.Rat()), currency.MinorUnits)

		id := req.CandidateID
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_candidates (candidate_id, organization_id, billing_account_id, subscription_id,
				subscription_version_id, term_no, currency_code, status, subtotal_amount, tax_jurisdiction_code,
				tax_rate_basis_points, tax_amount, total_amount, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', $8::numeric, $9, $10, $11::numeric, $12::numeric, $13, $14)`,
			id, req.OrganizationID, ba.BillingAccountID, req.SubscriptionID, version.SubscriptionVersionID, req.TermNo,
			agg.sub.CurrencyCode, subtotal, req.TaxJurisdictionCode, req.TaxRateBasisPoints, req.TaxAmount, total,
			now, req.CreatedByPrincipalID); err != nil {
			return err
		}
		for _, l := range lines {
			if _, err := tx.Exec(ctx, `
				INSERT INTO invoice_candidate_lines (candidate_id, line_no, kind, description, price_version_id,
					component_key, meter_key, statement_id, statement_total_quantity_at_generate, quantity, unit_amount, amount)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10::numeric, $11::numeric, $12::numeric)`,
				id, l.LineNo, l.Kind, l.Description, l.PriceVersionID, l.ComponentKey, l.MeterKey, l.StatementID,
				l.StatementTotalQuantityAtGenerate, l.Quantity, l.UnitAmount, l.Amount); err != nil {
				return err
			}
		}
		got, err := loadCandidate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "invoice_candidate", AggregateID: id,
			EventType: "invoice_candidate.generated", TenantID: &req.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) ApproveInvoiceCandidate(ctx context.Context, candidateID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.InvoiceCandidate, error) {
	var out *domain.InvoiceCandidate
	err := s.billingSellerTx(ctx, "", func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		c, err := loadCandidate(ctx, tx, candidateID, true)
		if err != nil {
			return err
		}
		if c.Status != domain.CandidateDraft {
			return fmt.Errorf("%w: candidate is %s, must be DRAFT to approve", domain.ErrInvoiceCandidateInvalidState, c.Status)
		}
		if actor == c.CreatedByPrincipalID {
			return domain.ErrInvoiceCandidateSelfApproval
		}
		if _, err := tx.Exec(ctx, `UPDATE invoice_candidates SET status = 'APPROVED', approved_at = $2, approved_by_principal_id = $3
			WHERE candidate_id = $1`, candidateID, now, actor); err != nil {
			return err
		}
		got, err := loadCandidate(ctx, tx, candidateID, false)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "invoice_candidate", AggregateID: candidateID,
			EventType: "invoice_candidate.approved", TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) IssueInvoice(ctx context.Context, candidateID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PlatformCommercialInvoice, error) {
	var out *domain.PlatformCommercialInvoice
	err := s.billingSellerTx(ctx, "", func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		c, err := loadCandidate(ctx, tx, candidateID, true)
		if err != nil {
			return err
		}
		if c.Status != domain.CandidateApproved {
			return fmt.Errorf("%w: candidate is %s, must be APPROVED to issue", domain.ErrInvoiceCandidateInvalidState, c.Status)
		}
		// Negative path #42: re-verify every USAGE line's basis is still
		// exactly what it was when the candidate was generated. A statement
		// reopened/superseded/adjusted after generate invalidates the
		// candidate rather than silently issuing a stale total.
		for _, l := range c.Lines {
			if l.Kind != domain.LineUsage {
				continue
			}
			var status, totalQuantity string
			if err := tx.QueryRow(ctx, `SELECT status, total_quantity::text FROM usage_statements WHERE statement_id = $1`,
				*l.StatementID).Scan(&status, &totalQuantity); err != nil {
				return err
			}
			if (status != "CERTIFIED" && status != "ADJUSTED") || totalQuantity != *l.StatementTotalQuantityAtGenerate {
				return domain.ErrInvoiceBasisChanged
			}
		}

		ba, err := loadBillingAccountByOrg(ctx, tx, c.OrganizationID)
		if err != nil {
			return err
		}
		var assigned int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO invoice_number_counters (numbering_profile, next_number) VALUES ($1, 2)
			ON CONFLICT (numbering_profile) DO UPDATE SET next_number = invoice_number_counters.next_number + 1
			RETURNING next_number - 1`, ba.InvoiceNumberingProfile).Scan(&assigned); err != nil {
			return err
		}
		invoiceNumber := fmt.Sprintf("%s-%010d", ba.InvoiceNumberingProfile, assigned)
		invoiceID := domain.NewCommercialID(domain.PrefixInvoice)

		if _, err := tx.Exec(ctx, `
			INSERT INTO platform_commercial_invoices (invoice_id, invoice_number, organization_id, billing_account_id,
				candidate_id, subscription_id, term_no, currency_code, subtotal_amount, tax_jurisdiction_code,
				tax_rate_basis_points, tax_amount, total_amount, issued_at, issued_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11, $12::numeric, $13::numeric, $14, $15)`,
			invoiceID, invoiceNumber, c.OrganizationID, ba.BillingAccountID, candidateID, c.SubscriptionID, c.TermNo,
			c.CurrencyCode, c.SubtotalAmount, c.TaxJurisdictionCode, c.TaxRateBasisPoints, c.TaxAmount, c.TotalAmount,
			now, actor); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_lines (invoice_id, line_no, kind, description, price_version_id, component_key,
				meter_key, statement_id, quantity, unit_amount, amount)
			SELECT $1, line_no, kind, description, price_version_id, component_key, meter_key, statement_id,
				quantity, unit_amount, amount FROM invoice_candidate_lines WHERE candidate_id = $2`,
			invoiceID, candidateID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invoice_candidates SET status = 'ISSUED', issued_invoice_id = $2 WHERE candidate_id = $1`,
			candidateID, invoiceID); err != nil {
			return err
		}
		got, err := loadInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "platform_commercial_invoice", AggregateID: invoiceID,
			EventType: "platform_invoice.issued", TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

const invoiceColumns = `invoice_id, invoice_number, organization_id::text, billing_account_id, candidate_id,
	subscription_id, term_no, currency_code, subtotal_amount::text, tax_jurisdiction_code, tax_rate_basis_points,
	tax_amount::text, total_amount::text, issued_at, issued_by_principal_id`

func scanInvoice(row pgx.Row) (*domain.PlatformCommercialInvoice, error) {
	var inv domain.PlatformCommercialInvoice
	if err := row.Scan(&inv.InvoiceID, &inv.InvoiceNumber, &inv.OrganizationID, &inv.BillingAccountID, &inv.CandidateID,
		&inv.SubscriptionID, &inv.TermNo, &inv.CurrencyCode, &inv.SubtotalAmount, &inv.TaxJurisdictionCode,
		&inv.TaxRateBasisPoints, &inv.TaxAmount, &inv.TotalAmount, &inv.IssuedAt, &inv.IssuedByPrincipalID); err != nil {
		return nil, err
	}
	return &inv, nil
}

func loadInvoice(ctx context.Context, tx pgx.Tx, invoiceID string) (*domain.PlatformCommercialInvoice, error) {
	inv, err := scanInvoice(tx.QueryRow(ctx, `SELECT `+invoiceColumns+` FROM platform_commercial_invoices WHERE invoice_id = $1`, invoiceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvoiceNotFound
	}
	if err != nil {
		return nil, err
	}
	inv.Lines, err = loadInvoiceLines(ctx, tx, invoiceID)
	return inv, err
}

// ── Queries ──────────────────────────────────────────────────────────────────

func (s *PgStore) GetInvoiceCandidate(ctx context.Context, candidateID string) (*domain.InvoiceCandidate, error) {
	var out *domain.InvoiceCandidate
	err := s.billingTx(ctx, func(tx pgx.Tx) error {
		c, err := loadCandidate(ctx, tx, candidateID, false)
		out = c
		return err
	})
	return out, err
}

func (s *PgStore) GetInvoice(ctx context.Context, invoiceID string) (*domain.PlatformCommercialInvoice, error) {
	var out *domain.PlatformCommercialInvoice
	err := s.billingTx(ctx, func(tx pgx.Tx) error {
		inv, err := loadInvoice(ctx, tx, invoiceID)
		out = inv
		return err
	})
	return out, err
}

// GetInvoiceBasis answers "what was this invoice rated from": the candidate
// it was issued from, with its lines and their basis references intact.
func (s *PgStore) GetInvoiceBasis(ctx context.Context, invoiceID string) (*domain.InvoiceCandidate, error) {
	var out *domain.InvoiceCandidate
	err := s.billingTx(ctx, func(tx pgx.Tx) error {
		inv, err := loadInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		c, err := loadCandidate(ctx, tx, inv.CandidateID, false)
		out = c
		return err
	})
	return out, err
}
