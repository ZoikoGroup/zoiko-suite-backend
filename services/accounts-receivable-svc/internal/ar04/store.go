package ar04

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

const headerCols = `invoice_id::text, tenant_id::text, legal_entity_id::text, customer_party_id::text, currency, lifecycle_state,
	delivery_state, einvoice_state, settlement_state, dispute_state, attention_state,
	accounting_state, COALESCE(accounting_journal_id,''), COALESCE(accounting_error,''),
	COALESCE(invoice_number,''), COALESCE(numbering_series,''), COALESCE(sequence_no,0),
	supply_date::text, COALESCE(requested_due_date::text,''), COALESCE(issue_date::text,''), COALESCE(due_date::text,''),
	COALESCE(payment_terms_code,''), COALESCE(payment_terms_days,0),
	customer_reference, notes, COALESCE(order_id::text,''), COALESCE(milestone_id::text,''),
	tax_jurisdiction_id, tax_supply_jurisdiction_id, tax_supply_type, tax_buyer_registration_id,
	net_total::text, tax_total::text, gross_total::text,
	content_hash, COALESCE(approved_content_hash,''), COALESCE(approved_by,''), approved_at,
	created_by, COALESCE(submitted_by,''), submitted_at,
	COALESCE(cancel_reason,''), COALESCE(cancelled_by,''), cancelled_at,
	COALESCE(issued_by,''), issued_at, COALESCE(issued_content_hash,''), COALESCE(open_item_id,''), COALESCE(issued_canonical,''),
	COALESCE(delivery_reference,''), COALESCE(delivery_detail,''),
	version, created_at, updated_at`

func scanHeader(row pgx.Row) (*Header, error) {
	var h Header
	var state string
	c := &h.Content
	if err := row.Scan(&h.InvoiceID, &h.TenantID, &h.LegalEntityID, &h.CustomerPartyID, &h.Currency, &state,
		&h.Delivery, &h.EInvoice, &h.Settlement, &h.Dispute, &h.Attention,
		&h.AccountingState, &h.AccountingJournalID, &h.AccountingError,
		&h.InvoiceNumber, &h.NumberingSeries, &h.SequenceNo,
		&c.SupplyDate, &c.RequestedDueDate, &h.IssueDate, &h.DueDate,
		&h.PaymentTermsCode, &h.PaymentTermsDays,
		&c.CustomerRef, &c.Notes, &c.OrderID, &c.MilestoneID,
		&c.Tax.JurisdictionID, &c.Tax.SupplyJurisdictionID, &c.Tax.SupplyType, &c.Tax.BuyerTaxRegistrationID,
		&h.NetTotal, &h.TaxTotal, &h.GrossTotal,
		&h.ContentHash, &h.ApprovedHash, &h.ApprovedBy, &h.ApprovedAt,
		&h.CreatedBy, &h.SubmittedBy, &h.SubmittedAt,
		&h.CancelReason, &h.CancelledBy, &h.CancelledAt,
		&h.IssuedBy, &h.IssuedAt, &h.IssuedContentHash, &h.OpenItemID, &h.issuedCanonical,
		&h.DeliveryReference, &h.DeliveryDetail,
		&h.Version, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	h.State = State(state)
	return &h, nil
}

func getHeader(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string, forUpdate bool) (*Header, error) {
	if _, err := uuid.Parse(invoiceID); err != nil {
		return nil, kit.NotFound("invoice not found")
	}
	q := `SELECT ` + headerCols + ` FROM ar04_invoices WHERE tenant_id = $1 AND invoice_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	h, err := scanHeader(tx.QueryRow(ctx, q, tenantID, invoiceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("invoice not found")
	}
	return h, kit.MapPgErr(err)
}

func getHeaderByNumber(ctx context.Context, tx pgx.Tx, tenantID, entityID, series, number string) (*Header, error) {
	if series == "" {
		series = DefaultSeries
	}
	h, err := scanHeader(tx.QueryRow(ctx, `SELECT `+headerCols+` FROM ar04_invoices
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND numbering_series = $3 AND invoice_number = $4`, tenantID, entityID, series, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("invoice not found")
	}
	return h, kit.MapPgErr(err)
}

const lineCols = `line_no, description, product_ref, quantity::text, unit_price::text, net_amount::text, tax_amount::text,
	tax_category, product_classification, supply_kind`

func getLines(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string) ([]Line, error) {
	rows, err := tx.Query(ctx, `SELECT `+lineCols+` FROM ar04_invoice_lines WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY line_no`, tenantID, invoiceID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []Line{}
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.LineNo, &l.Description, &l.ProductRef, &l.Quantity, &l.UnitPrice, &l.Net, &l.Tax, &l.TaxCategory,
			&l.ProductClassification, &l.SupplyKind); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

const taxCols = `group_no, tax_category, product_classification, supply_kind, determination_id, provenance, determination_status,
	jurisdiction_id, tax_rate, net_amount::text, taxable_amount::text, tax_amount::text, recorded_by, recorded_at`

func getTaxGroups(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string) ([]TaxGroup, error) {
	rows, err := tx.Query(ctx, `SELECT `+taxCols+` FROM ar04_invoice_tax_determinations WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY group_no`, tenantID, invoiceID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []TaxGroup{}
	for rows.Next() {
		var g TaxGroup
		var at time.Time
		if err := rows.Scan(&g.GroupNo, &g.TaxCategory, &g.ProductClassification, &g.SupplyKind, &g.DeterminationID, &g.Provenance, &g.Status,
			&g.JurisdictionID, &g.TaxRate, &g.Net, &g.Taxable, &g.Tax, &g.RecordedBy, &at); err != nil {
			return nil, err
		}
		g.RecordedAt = &at
		out = append(out, g)
	}
	return out, rows.Err()
}

func loadInvoice(ctx context.Context, tx pgx.Tx, tenantID string, h *Header) (*Invoice, error) {
	lines, err := getLines(ctx, tx, tenantID, h.InvoiceID)
	if err != nil {
		return nil, err
	}
	groups, err := getTaxGroups(ctx, tx, tenantID, h.InvoiceID)
	if err != nil {
		return nil, err
	}
	return &Invoice{Header: *h, Lines: lines, TaxGroups: groups}, nil
}

// ── writes ───────────────────────────────────────────────────────────────────

func jsonb(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func insertHeader(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, at time.Time) error {
	ct := h.Content
	_, err := tx.Exec(ctx, `INSERT INTO ar04_invoices (invoice_id, tenant_id, legal_entity_id, customer_party_id, currency, lifecycle_state,
			supply_date, requested_due_date, customer_reference, notes, order_id, milestone_id,
			tax_jurisdiction_id, tax_supply_jurisdiction_id, tax_supply_type, tax_buyer_registration_id,
			net_total, tax_total, gross_total, content_hash, created_by, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,'DRAFT',$6::date,NULLIF($7,'')::date,$8,$9,NULLIF($10,'')::uuid,NULLIF($11,'')::uuid,$12,$13,$14,$15,
			$16::numeric,$17::numeric,$18::numeric,$19,$20,1,$21,$21)`,
		h.InvoiceID, c.TenantID, h.LegalEntityID, h.CustomerPartyID, h.Currency, ct.SupplyDate, ct.RequestedDueDate, ct.CustomerRef, ct.Notes, ct.OrderID, ct.MilestoneID,
		ct.Tax.JurisdictionID, ct.Tax.SupplyJurisdictionID, ct.Tax.SupplyType, ct.Tax.BuyerTaxRegistrationID,
		h.NetTotal.String(), h.TaxTotal.String(), h.GrossTotal.String(), h.ContentHash, c.PrincipalID, at.UTC())
	return kit.MapPgErr(err)
}

func insertLines(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID string, lines []Line) error {
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `INSERT INTO ar04_invoice_lines (invoice_id, line_no, tenant_id, description, product_ref, quantity, unit_price,
				net_amount, tax_amount, tax_category, product_classification, supply_kind)
			VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10,$11,$12)`,
			invoiceID, l.LineNo, c.TenantID, l.Description, l.ProductRef, l.Quantity.String(), l.UnitPrice.String(), l.Net.String(), l.Tax.String(),
			l.TaxCategory, l.ProductClassification, l.SupplyKind); err != nil {
			return kit.MapPgErr(err)
		}
	}
	return nil
}

// replaceLines swaps every line (pre-issue only: the table trigger refuses it afterwards).
func replaceLines(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID string, lines []Line) error {
	if _, err := tx.Exec(ctx, `DELETE FROM ar04_invoice_lines WHERE tenant_id = $1 AND invoice_id = $2`, c.TenantID, invoiceID); err != nil {
		return kit.MapPgErr(err)
	}
	return insertLines(ctx, tx, c, invoiceID, lines)
}

// replaceTax swaps the stored tax result (pre-issue only).
func replaceTax(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID string, groups []TaxGroup) error {
	if _, err := tx.Exec(ctx, `DELETE FROM ar04_invoice_tax_determinations WHERE tenant_id = $1 AND invoice_id = $2`, c.TenantID, invoiceID); err != nil {
		return kit.MapPgErr(err)
	}
	for _, g := range groups {
		at := time.Now().UTC()
		if g.RecordedAt != nil {
			at = *g.RecordedAt
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ar04_invoice_tax_determinations (invoice_id, group_no, tenant_id, tax_category, product_classification, supply_kind,
				determination_id, provenance, determination_status, jurisdiction_id, tax_rate, net_amount, taxable_amount, tax_amount, recorded_by, recorded_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::numeric,$13::numeric,$14::numeric,$15,$16)`,
			invoiceID, g.GroupNo, c.TenantID, g.TaxCategory, g.ProductClassification, g.SupplyKind, g.DeterminationID, g.Provenance, g.Status,
			g.JurisdictionID, g.TaxRate, g.Net.String(), g.Taxable.String(), g.Tax.String(), g.RecordedBy, at.UTC()); err != nil {
			return kit.MapPgErr(err)
		}
	}
	return nil
}

func insertApproval(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar04_invoice_approvals (approval_id, tenant_id, legal_entity_id, invoice_id, content_hash, gross_total,
			created_by, submitted_by, approved_by, approved_at)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9,$10)`,
		uuid.NewString(), c.TenantID, h.LegalEntityID, h.InvoiceID, h.ContentHash, h.GrossTotal.String(), h.CreatedBy, h.SubmittedBy, c.PrincipalID, at.UTC())
	return kit.MapPgErr(err)
}

type hist struct {
	At          time.Time // injected clock
	Command     string
	Reason      string
	Evidence    string
	ApproverID  string
	ContentHash string
	Prior       map[string]any
	New         map[string]any
}

func writeHistory(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, hs hist) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar04_invoice_history (history_id, tenant_id, legal_entity_id, invoice_id, invoice_version, command,
			lifecycle_after, actor_id, approver_id, reason_code, evidence_id, content_hash, prior_values, new_values, correlation_id, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),$13::jsonb,$14::jsonb,$15,$16)`,
		uuid.NewString(), c.TenantID, h.LegalEntityID, h.InvoiceID, h.Version, hs.Command, string(h.State), c.PrincipalID,
		hs.ApproverID, hs.Reason, hs.Evidence, hs.ContentHash, jsonb(hs.Prior), jsonb(hs.New), c.CorrelationID, hs.At.UTC())
	return kit.MapPgErr(err)
}

func auditDetail(h *Header, hs hist) map[string]any {
	d := map[string]any{"invoice_version": h.Version, "customer_party_id": h.CustomerPartyID, "content_hash": hs.ContentHash, "lifecycle_state": h.State}
	if h.InvoiceNumber != "" {
		d["invoice_number"] = h.InvoiceNumber
	}
	if hs.ApproverID != "" {
		d["approver_id"] = hs.ApproverID
	}
	return d
}

// record appends the history row and the kit audit record and emits the events, all in the caller's
// transaction. h carries the version and state AFTER the change.
func record(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, hs hist, events func(*Header) []kit.Event) error {
	if err := writeHistory(ctx, tx, c, h, hs); err != nil {
		return err
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: h.InvoiceID, Command: hs.Command,
		ReasonCode: hs.Reason, EvidenceID: hs.Evidence, Before: hs.Prior, After: hs.New, Detail: auditDetail(h, hs)}); err != nil {
		return err
	}
	if events != nil {
		return kit.Emit(ctx, tx, c, events(h)...)
	}
	return nil
}

// commit persists the pre-issue content / lifecycle of nh over cur (optimistic concurrency by
// version, already held by the row lock), then records history, audit and events.
func commit(ctx context.Context, tx pgx.Tx, c kit.Caller, cur, nh *Header, hs hist, events func(*Header) []kit.Event) (*Header, error) {
	out := *nh
	ct := nh.Content
	if err := tx.QueryRow(ctx, `UPDATE ar04_invoices SET lifecycle_state = $3, supply_date = $4::date, requested_due_date = NULLIF($5,'')::date,
			customer_reference = $6, notes = $7, order_id = NULLIF($8,'')::uuid, milestone_id = NULLIF($9,'')::uuid,
			tax_jurisdiction_id = $10, tax_supply_jurisdiction_id = $11, tax_supply_type = $12, tax_buyer_registration_id = $13,
			net_total = $14::numeric, tax_total = $15::numeric, gross_total = $16::numeric, content_hash = $17,
			approved_content_hash = NULLIF($18,''), approved_by = NULLIF($19,''), approved_at = $20,
			submitted_by = NULLIF($21,''), submitted_at = $22,
			cancel_reason = NULLIF($23,''), cancelled_by = NULLIF($24,''), cancelled_at = $25,
			version = version + 1, updated_at = $26
		WHERE tenant_id = $1 AND invoice_id = $2 AND version = $27 RETURNING version, updated_at`,
		c.TenantID, cur.InvoiceID, string(nh.State), ct.SupplyDate, ct.RequestedDueDate, ct.CustomerRef, ct.Notes, ct.OrderID, ct.MilestoneID,
		ct.Tax.JurisdictionID, ct.Tax.SupplyJurisdictionID, ct.Tax.SupplyType, ct.Tax.BuyerTaxRegistrationID,
		nh.NetTotal.String(), nh.TaxTotal.String(), nh.GrossTotal.String(), nh.ContentHash,
		nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt, nh.SubmittedBy, nh.SubmittedAt,
		nh.CancelReason, nh.CancelledBy, nh.CancelledAt, hs.At.UTC(), cur.Version).Scan(&out.Version, &out.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, kit.ErrStaleVersion
		}
		return nil, kit.MapPgErr(err)
	}
	if err := record(ctx, tx, c, &out, hs, events); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── numbering ────────────────────────────────────────────────────────────────

// numberingProfile returns the entity's profile for the default series, or DefaultNumbering.
func numberingProfile(ctx context.Context, tx pgx.Tx, tenantID, entityID string) (NumberingProfile, error) {
	p := DefaultNumbering
	err := tx.QueryRow(ctx, `SELECT series, prefix, number_width FROM ar04_numbering_profiles
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND series = $3`, tenantID, entityID, DefaultSeries).Scan(&p.Series, &p.Prefix, &p.Width)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultNumbering, nil
	}
	if err != nil {
		return NumberingProfile{}, kit.MapPgErr(err)
	}
	return p, p.Validate()
}

// nextSequence allocates the next gapless number of (tenant, legal entity, series) inside the
// issue transaction: a rolled-back issue returns the number, so no gap and no duplicate is possible.
// The conflicting UPDATE row-locks the counter, so concurrent issues serialise here.
func nextSequence(ctx context.Context, tx pgx.Tx, tenantID, entityID, series string) (int64, error) {
	var n int64
	if err := tx.QueryRow(ctx, `INSERT INTO ar04_number_counters (tenant_id, legal_entity_id, series, next_number) VALUES ($1,$2,$3,2)
		ON CONFLICT (tenant_id, legal_entity_id, series) DO UPDATE SET next_number = ar04_number_counters.next_number + 1
		RETURNING next_number - 1`, tenantID, entityID, series).Scan(&n); err != nil {
		return 0, kit.MapPgErr(err)
	}
	return n, nil
}

// ── issue ────────────────────────────────────────────────────────────────────

type issueWrite struct {
	Number           string
	Series           string
	Sequence         int64
	IssueDate        string
	DueDate          string
	TermsCode        string
	TermsDays        int
	IssuedBy         string
	IssuedAt         time.Time
	Canonical        string
	Hash             string
	OpenItemID       string
	EInvoiceRequired bool
}

// writeIssued moves the invoice to ISSUED with its number, dates, sealed representation, receivable
// link and QUEUED accounting state in one statement.
func writeIssued(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, w issueWrite) (*Header, error) {
	einv := EInvNotRequired
	if w.EInvoiceRequired {
		einv = EInvRequired
	}
	out := *cur
	if err := tx.QueryRow(ctx, `UPDATE ar04_invoices SET lifecycle_state = 'ISSUED', invoice_number = $3, numbering_series = $4, sequence_no = $5,
			issue_date = $6::date, due_date = $7::date, payment_terms_code = NULLIF($8,''), payment_terms_days = $9,
			issued_by = $10, issued_at = $11, issued_canonical = $12, issued_content_hash = $13, open_item_id = $14,
			einvoice_state = $15, accounting_state = 'QUEUED', accounting_updated_at = $11, version = version + 1, updated_at = $11
		WHERE tenant_id = $1 AND invoice_id = $2 AND version = $16 RETURNING version, updated_at`,
		c.TenantID, cur.InvoiceID, w.Number, w.Series, w.Sequence, w.IssueDate, w.DueDate, w.TermsCode, w.TermsDays, w.IssuedBy, w.IssuedAt.UTC(),
		w.Canonical, w.Hash, w.OpenItemID, einv, cur.Version).Scan(&out.Version, &out.UpdatedAt); err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, kit.ErrStaleVersion
		case kit.IsUniqueViolation(err, "uq_ar04_invoice_number"), kit.IsUniqueViolation(err, "uq_ar04_invoice_sequence"):
			// Defence in depth: the gapless counter makes this unreachable, and the unique index refuses it if it ever happens.
			return nil, kit.Conflict(kit.CodeInvoiceNumberCollision, "the legal invoice number "+w.Number+" is already in use: nothing was issued")
		case kit.IsUniqueViolation(err, "uq_ar04_issued_milestone"):
			return nil, kit.Unprocessable(kit.CodeOrderNotBillable, "the billable milestone is already invoiced by another issued invoice")
		}
		return nil, kit.MapPgErr(err)
	}
	t := w.IssuedAt
	out.State, out.InvoiceNumber, out.NumberingSeries, out.SequenceNo = StateIssued, w.Number, w.Series, w.Sequence
	out.IssueDate, out.DueDate, out.PaymentTermsCode, out.PaymentTermsDays = w.IssueDate, w.DueDate, w.TermsCode, w.TermsDays
	out.IssuedBy, out.IssuedAt, out.issuedCanonical, out.IssuedContentHash, out.OpenItemID = w.IssuedBy, &t, w.Canonical, w.Hash, w.OpenItemID
	out.EInvoice, out.AccountingState = einv, "QUEUED"
	return &out, nil
}

// ── dimension writers: each writes ONLY its own dimension (plus version / updated_at) ──

func bump(ctx context.Context, tx pgx.Tx, q string, args ...any) (version int, err error) {
	if err := tx.QueryRow(ctx, q, args...).Scan(&version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, kit.ErrStaleVersion
		}
		return 0, kit.MapPgErr(err)
	}
	return version, nil
}

func writeDelivery(ctx context.Context, tx pgx.Tx, tenantID string, cur *Header, to, ref, detail string, at time.Time) (int, error) {
	return bump(ctx, tx, `UPDATE ar04_invoices SET delivery_state = $3, delivery_reference = NULLIF($4,''), delivery_detail = NULLIF($5,''),
			delivery_changed_at = $6, version = version + 1, updated_at = $6
		WHERE tenant_id = $1 AND invoice_id = $2 AND version = $7 RETURNING version`, tenantID, cur.InvoiceID, to, ref, detail, at.UTC(), cur.Version)
}

func writeEInvoice(ctx context.Context, tx pgx.Tx, tenantID string, cur *Header, to string, at time.Time) (int, error) {
	return bump(ctx, tx, `UPDATE ar04_invoices SET einvoice_state = $3, version = version + 1, updated_at = $4
		WHERE tenant_id = $1 AND invoice_id = $2 AND version = $5 RETURNING version`, tenantID, cur.InvoiceID, to, at.UTC(), cur.Version)
}

func writeSettlement(ctx context.Context, tx pgx.Tx, tenantID string, cur *Header, to string, at time.Time) (int, error) {
	return bump(ctx, tx, `UPDATE ar04_invoices SET settlement_state = $3, settlement_changed_at = $4, version = version + 1, updated_at = $4
		WHERE tenant_id = $1 AND invoice_id = $2 AND version = $5 RETURNING version`, tenantID, cur.InvoiceID, to, at.UTC(), cur.Version)
}

func writeDispute(ctx context.Context, tx pgx.Tx, tenantID string, cur *Header, to string, at time.Time) (int, error) {
	return bump(ctx, tx, `UPDATE ar04_invoices SET dispute_state = $3, version = version + 1, updated_at = $4
		WHERE tenant_id = $1 AND invoice_id = $2 AND version = $5 RETURNING version`, tenantID, cur.InvoiceID, to, at.UTC(), cur.Version)
}

func writeAttention(ctx context.Context, tx pgx.Tx, tenantID string, cur *Header, to string, at time.Time) (int, error) {
	return bump(ctx, tx, `UPDATE ar04_invoices SET attention_state = $3, version = version + 1, updated_at = $4
		WHERE tenant_id = $1 AND invoice_id = $2 AND version = $5 RETURNING version`, tenantID, cur.InvoiceID, to, at.UTC(), cur.Version)
}

// mirrorAccounting records the ACC-04 outcome on the accounting fields only. A POSTED invoice is
// never regressed by a later, contradictory outcome. It does not bump the version: the accounting
// mirror is not an operator-visible state change.
func mirrorAccounting(ctx context.Context, tx pgx.Tx, tenantID, invoiceID, status, journalID, errText string, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE ar04_invoices SET accounting_state = $3::text, accounting_journal_id = COALESCE(NULLIF($4,''), accounting_journal_id),
			accounting_error = NULLIF($5,''), accounting_updated_at = $6, updated_at = $6
		WHERE tenant_id = $1 AND invoice_id::text = $2 AND lifecycle_state IN ('ISSUED','CORRECTED')
		  AND (accounting_state <> 'POSTED' OR $3::text = 'POSTED')`, tenantID, invoiceID, status, journalID, errText, at.UTC())
	if err != nil {
		return false, kit.MapPgErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// ── cancellation requests and correction links ───────────────────────────────

const cancelCols = `request_id::text, status, reason_code, evidence_id, requested_by, requested_at, COALESCE(decided_by,''), decided_at, COALESCE(decision_reason,'')`

func scanCancel(row pgx.Row) (*CancellationRequest, error) {
	var r CancellationRequest
	if err := row.Scan(&r.RequestID, &r.Status, &r.ReasonCode, &r.EvidenceID, &r.RequestedBy, &r.RequestedAt, &r.DecidedBy, &r.DecidedAt, &r.DecisionReason); err != nil {
		return nil, err
	}
	return &r, nil
}

// openCancellation returns the invoice's open (REQUESTED or APPROVED) request, or nil.
func openCancellation(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string, forUpdate bool) (*CancellationRequest, error) {
	q := `SELECT ` + cancelCols + ` FROM ar04_cancellation_requests WHERE tenant_id = $1 AND invoice_id = $2 AND status IN ('REQUESTED','APPROVED')`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	r, err := scanCancel(tx.QueryRow(ctx, q, tenantID, invoiceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, kit.MapPgErr(err)
}

func listCancellations(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string) ([]CancellationRequest, error) {
	rows, err := tx.Query(ctx, `SELECT `+cancelCols+` FROM ar04_cancellation_requests WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY requested_at`, tenantID, invoiceID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []CancellationRequest{}
	for rows.Next() {
		r, err := scanCancel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func listCorrections(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string) ([]CorrectionLink, error) {
	rows, err := tx.Query(ctx, `SELECT correction_id, kind, COALESCE(reason_code,''), COALESCE(evidence_id,''), linked_by, linked_at
		FROM ar04_correction_links WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY linked_at, correction_id`, tenantID, invoiceID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []CorrectionLink{}
	for rows.Next() {
		var l CorrectionLink
		if err := rows.Scan(&l.CorrectionID, &l.Kind, &l.ReasonCode, &l.EvidenceID, &l.LinkedBy, &l.LinkedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func listApprovals(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string) ([]ApprovalRecord, error) {
	rows, err := tx.Query(ctx, `SELECT content_hash, gross_total::text, created_by, submitted_by, approved_by, approved_at
		FROM ar04_invoice_approvals WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY approved_at`, tenantID, invoiceID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []ApprovalRecord{}
	for rows.Next() {
		var a ApprovalRecord
		if err := rows.Scan(&a.ContentHash, &a.GrossTotal, &a.CreatedBy, &a.SubmittedBy, &a.ApprovedBy, &a.ApprovedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
