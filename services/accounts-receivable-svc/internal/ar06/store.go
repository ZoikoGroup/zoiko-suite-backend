package ar06

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

const headerCols = `correction_id::text, tenant_id::text, legal_entity_id::text, customer_party_id::text, kind, source_invoice_id::text, source_invoice_number,
	currency, COALESCE(client_reference,''), state, application_state, einvoice_state, accounting_state, COALESCE(accounting_journal_id,''),
	COALESCE(accounting_error,''), reason_code, reason_text, COALESCE(evidence_id,''), to_char(supply_date,'YYYY-MM-DD'),
	tax_jurisdiction_id, tax_supply_jurisdiction_id, tax_supply_type, tax_buyer_registration_id,
	net_total, tax_total, gross_total, content_hash, COALESCE(approved_content_hash,''), COALESCE(approved_by,''), approved_at,
	created_by, COALESCE(submitted_by,''), submitted_at, COALESCE(cancel_reason,''), COALESCE(cancelled_by,''), cancelled_at,
	COALESCE(document_number,''), COALESCE(numbering_series,''), COALESCE(sequence_no,0), COALESCE(to_char(issue_date,'YYYY-MM-DD'),''),
	COALESCE(to_char(due_date,'YYYY-MM-DD'),''), COALESCE(issued_by,''), issued_at, COALESCE(issued_canonical,''), COALESCE(issued_content_hash,''),
	COALESCE(open_item_id,''), version, created_at, updated_at`

func scanHeader(row pgx.Row) (*Header, error) {
	var h Header
	var kind, state string
	if err := row.Scan(&h.CorrectionID, &h.TenantID, &h.LegalEntityID, &h.CustomerPartyID, &kind, &h.SourceInvoiceID, &h.SourceInvoiceNumber,
		&h.Currency, &h.Content.ClientReference, &state, &h.ApplicationState, &h.EInvoiceState, &h.AccountingState, &h.AccountingJournalID,
		&h.AccountingError, &h.Content.ReasonCode, &h.Content.ReasonText, &h.Content.EvidenceID, &h.Content.SupplyDate,
		&h.Content.Tax.JurisdictionID, &h.Content.Tax.SupplyJurisdictionID, &h.Content.Tax.SupplyType, &h.Content.Tax.BuyerTaxRegistrationID,
		&h.NetTotal, &h.TaxTotal, &h.GrossTotal, &h.ContentHash, &h.ApprovedHash, &h.ApprovedBy, &h.ApprovedAt,
		&h.CreatedBy, &h.SubmittedBy, &h.SubmittedAt, &h.CancelReason, &h.CancelledBy, &h.CancelledAt,
		&h.DocumentNumber, &h.NumberingSeries, &h.SequenceNo, &h.IssueDate, &h.DueDate, &h.IssuedBy, &h.IssuedAt, &h.issuedCanonical, &h.IssuedContentHash,
		&h.OpenItemID, &h.Version, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	h.Kind, h.State = Kind(kind), State(state)
	return &h, nil
}

func getHeader(ctx context.Context, tx pgx.Tx, tenantID, correctionID string, forUpdate bool) (*Header, error) {
	if err := mustUUID("correction_id", correctionID); err != nil {
		return nil, err
	}
	q := `SELECT ` + headerCols + ` FROM ar06_corrections WHERE tenant_id = $1 AND correction_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	h, err := scanHeader(tx.QueryRow(ctx, q, tenantID, correctionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("correction not found")
	}
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	return h, nil
}

func getLines(ctx context.Context, tx pgx.Tx, tenantID, correctionID string) ([]Line, error) {
	rows, err := tx.Query(ctx, `SELECT line_no, COALESCE(source_line_no,0), description, quantity, unit_price, net_amount, tax_amount, tax_category,
			product_classification, supply_kind FROM ar06_correction_lines WHERE tenant_id = $1 AND correction_id = $2 ORDER BY line_no`, tenantID, correctionID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []Line{}
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.LineNo, &l.SourceLineNo, &l.Description, &l.Quantity, &l.UnitPrice, &l.Net, &l.Tax, &l.TaxCategory, &l.ProductClassification, &l.SupplyKind); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func getTaxGroups(ctx context.Context, tx pgx.Tx, tenantID, correctionID string) ([]ar04.TaxGroup, error) {
	rows, err := tx.Query(ctx, `SELECT group_no, tax_category, product_classification, supply_kind, determination_id, provenance, determination_status,
			jurisdiction_id, tax_rate, net_amount, taxable_amount, tax_amount, recorded_by, recorded_at
		FROM ar06_correction_tax_determinations WHERE tenant_id = $1 AND correction_id = $2 ORDER BY group_no`, tenantID, correctionID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []ar04.TaxGroup{}
	for rows.Next() {
		var g ar04.TaxGroup
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

func loadCorrection(ctx context.Context, tx pgx.Tx, tenantID string, h *Header) (*Correction, error) {
	lines, err := getLines(ctx, tx, tenantID, h.CorrectionID)
	if err != nil {
		return nil, err
	}
	groups, err := getTaxGroups(ctx, tx, tenantID, h.CorrectionID)
	if err != nil {
		return nil, err
	}
	return &Correction{Header: *h, Lines: lines, TaxGroups: groups}, nil
}

func jsonb(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func insertHeader(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, at time.Time) error {
	ct := h.Content
	_, err := tx.Exec(ctx, `INSERT INTO ar06_corrections (correction_id, tenant_id, legal_entity_id, customer_party_id, kind, source_invoice_id, source_invoice_number,
			currency, client_reference, state, reason_code, reason_text, evidence_id, supply_date, tax_jurisdiction_id, tax_supply_jurisdiction_id, tax_supply_type,
			tax_buyer_registration_id, net_total, tax_total, gross_total, content_hash, created_by, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),'DRAFT',$10,$11,NULLIF($12,''),$13::date,$14,$15,$16,$17,$18::numeric,$19::numeric,$20::numeric,$21,$22,1,$23,$23)`,
		h.CorrectionID, c.TenantID, h.LegalEntityID, h.CustomerPartyID, string(h.Kind), h.SourceInvoiceID, h.SourceInvoiceNumber, h.Currency, ct.ClientReference,
		ct.ReasonCode, ct.ReasonText, ct.EvidenceID, ct.SupplyDate, ct.Tax.JurisdictionID, ct.Tax.SupplyJurisdictionID, ct.Tax.SupplyType, ct.Tax.BuyerTaxRegistrationID,
		h.NetTotal.String(), h.TaxTotal.String(), h.GrossTotal.String(), h.ContentHash, c.PrincipalID, at.UTC())
	switch {
	case kit.IsUniqueViolation(err, "uq_ar06_client_reference"):
		return kit.Conflict(CodeCorrectionDuplicate, "a live correction with this client_reference already exists for the source invoice")
	case err != nil:
		return kit.MapPgErr(err)
	}
	return nil
}

func insertLines(ctx context.Context, tx pgx.Tx, c kit.Caller, correctionID string, lines []Line) error {
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `INSERT INTO ar06_correction_lines (correction_id, line_no, tenant_id, source_line_no, description, quantity, unit_price, net_amount,
				tax_amount, tax_category, product_classification, supply_kind)
			VALUES ($1,$2,$3,NULLIF($4,0),$5,$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10,$11,$12)`,
			correctionID, l.LineNo, c.TenantID, l.SourceLineNo, l.Description, l.Quantity.String(), l.UnitPrice.String(), l.Net.String(), l.Tax.String(),
			l.TaxCategory, l.ProductClassification, l.SupplyKind); err != nil {
			return kit.MapPgErr(err)
		}
	}
	return nil
}

func replaceLines(ctx context.Context, tx pgx.Tx, c kit.Caller, correctionID string, lines []Line) error {
	if _, err := tx.Exec(ctx, `DELETE FROM ar06_correction_lines WHERE tenant_id = $1 AND correction_id = $2`, c.TenantID, correctionID); err != nil {
		return kit.MapPgErr(err)
	}
	return insertLines(ctx, tx, c, correctionID, lines)
}

func replaceTax(ctx context.Context, tx pgx.Tx, c kit.Caller, correctionID string, groups []ar04.TaxGroup) error {
	if _, err := tx.Exec(ctx, `DELETE FROM ar06_correction_tax_determinations WHERE tenant_id = $1 AND correction_id = $2`, c.TenantID, correctionID); err != nil {
		return kit.MapPgErr(err)
	}
	for _, g := range groups {
		at := time.Now().UTC()
		if g.RecordedAt != nil {
			at = g.RecordedAt.UTC()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ar06_correction_tax_determinations (correction_id, group_no, tenant_id, tax_category, product_classification, supply_kind,
				determination_id, provenance, determination_status, jurisdiction_id, tax_rate, net_amount, taxable_amount, tax_amount, recorded_by, recorded_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::numeric,$13::numeric,$14::numeric,$15,$16)`,
			correctionID, g.GroupNo, c.TenantID, g.TaxCategory, g.ProductClassification, g.SupplyKind, g.DeterminationID, g.Provenance, g.Status, g.JurisdictionID,
			g.TaxRate, g.Net.String(), g.Taxable.String(), g.Tax.String(), g.RecordedBy, at); err != nil {
			return kit.MapPgErr(err)
		}
	}
	return nil
}

// ── evidence ─────────────────────────────────────────────────────────────────

type hist struct {
	At          time.Time
	Command     string
	Reason      string
	Evidence    string
	ApproverID  string
	ContentHash string
	Prior       map[string]any
	New         map[string]any
}

func writeHistory(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, hs hist) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar06_correction_history (history_id, tenant_id, legal_entity_id, correction_id, correction_version, command,
			state_after, actor_id, approver_id, reason_code, evidence_id, content_hash, prior_values, new_values, correlation_id, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),$13::jsonb,$14::jsonb,$15,$16)`,
		uuid.NewString(), c.TenantID, h.LegalEntityID, h.CorrectionID, h.Version, hs.Command, string(h.State), c.PrincipalID,
		hs.ApproverID, hs.Reason, hs.Evidence, hs.ContentHash, jsonb(hs.Prior), jsonb(hs.New), c.CorrelationID, hs.At.UTC())
	return kit.MapPgErr(err)
}

func auditDetail(h *Header, hs hist) map[string]any {
	d := map[string]any{"correction_version": h.Version, "customer_party_id": h.CustomerPartyID, "source_invoice_id": h.SourceInvoiceID, "kind": h.Kind,
		"content_hash": hs.ContentHash, "lifecycle_state": h.State}
	if h.DocumentNumber != "" {
		d["document_number"] = h.DocumentNumber
	}
	if hs.ApproverID != "" {
		d["approver_id"] = hs.ApproverID
	}
	return d
}

// record appends the history row and the kit audit record and emits the events, all in the caller's transaction. h carries the
// version and state AFTER the change.
func record(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, hs hist, events func(*Header) []kit.Event) error {
	if err := writeHistory(ctx, tx, c, h, hs); err != nil {
		return err
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: h.CorrectionID, Command: hs.Command,
		ReasonCode: hs.Reason, EvidenceID: hs.Evidence, Before: hs.Prior, After: hs.New, Detail: auditDetail(h, hs)}); err != nil {
		return err
	}
	if events != nil {
		return kit.Emit(ctx, tx, c, events(h)...)
	}
	return nil
}

// commit persists the pre-issue lifecycle / totals / approval facts of nh over cur, then records history, audit and events.
func commit(ctx context.Context, tx pgx.Tx, c kit.Caller, cur, nh *Header, hs hist, events func(*Header) []kit.Event) (*Header, error) {
	out := *nh
	if err := tx.QueryRow(ctx, `UPDATE ar06_corrections SET state = $3, net_total = $4::numeric, tax_total = $5::numeric, gross_total = $6::numeric, content_hash = $7,
			approved_content_hash = NULLIF($8,''), approved_by = NULLIF($9,''), approved_at = $10, submitted_by = NULLIF($11,''), submitted_at = $12,
			cancel_reason = NULLIF($13,''), cancelled_by = NULLIF($14,''), cancelled_at = $15, version = version + 1, updated_at = $16
		WHERE tenant_id = $1 AND correction_id = $2 AND version = $17 RETURNING version, updated_at`,
		c.TenantID, cur.CorrectionID, string(nh.State), nh.NetTotal.String(), nh.TaxTotal.String(), nh.GrossTotal.String(), nh.ContentHash,
		nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt, nh.SubmittedBy, nh.SubmittedAt, nh.CancelReason, nh.CancelledBy, nh.CancelledAt,
		hs.At.UTC(), cur.Version).Scan(&out.Version, &out.UpdatedAt); err != nil {
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

func bump(ctx context.Context, tx pgx.Tx, q string, args ...any) (version int, err error) {
	if err := tx.QueryRow(ctx, q, args...).Scan(&version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, kit.ErrStaleVersion
		}
		return 0, kit.MapPgErr(err)
	}
	return version, nil
}

// ── source invoice eligibility ───────────────────────────────────────────────

// lockSource serialises every eligibility check and credit issue against one source invoice: it takes a row lock on the
// invoice's ar06_source_locks row (created on first use). Held until the caller's transaction ends.
func lockSource(ctx context.Context, tx pgx.Tx, tenantID, sourceInvoiceID string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ar06_source_locks (tenant_id, source_invoice_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, tenantID, sourceInvoiceID); err != nil {
		return kit.MapPgErr(err)
	}
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM ar06_source_locks WHERE tenant_id = $1 AND source_invoice_id = $2 FOR UPDATE`, tenantID, sourceInvoiceID).Scan(&one); err != nil {
		return kit.MapPgErr(err)
	}
	return nil
}

// creditedTotals sums the ISSUED credit notes against a source invoice (excluding one correction), exactly, from this
// module's own tables. Drafts, pending, approved and cancelled notes consume nothing.
func creditedTotals(ctx context.Context, tx pgx.Tx, tenantID, sourceInvoiceID, excludeID string) (net, gross kit.Amount, err error) {
	err = tx.QueryRow(ctx, `SELECT COALESCE(sum(net_total),0), COALESCE(sum(gross_total),0) FROM ar06_corrections
		WHERE tenant_id = $1 AND source_invoice_id = $2 AND kind = 'CREDIT_NOTE' AND state IN ('ISSUED','APPLIED','SETTLED') AND correction_id::text <> $3`,
		tenantID, sourceInvoiceID, excludeID).Scan(&net, &gross)
	return net, gross, kit.MapPgErr(err)
}

// ── numbering ────────────────────────────────────────────────────────────────

func numberingProfile(ctx context.Context, tx pgx.Tx, tenantID, entityID string, k Kind) (NumberingProfile, error) {
	p := DefaultNumbering(k)
	err := tx.QueryRow(ctx, `SELECT series, prefix, number_width FROM ar06_numbering_profiles
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND series = $3`, tenantID, entityID, k.Series()).Scan(&p.Series, &p.Prefix, &p.Width)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultNumbering(k), nil
	}
	if err != nil {
		return NumberingProfile{}, kit.MapPgErr(err)
	}
	return p, p.Validate()
}

// nextSequence allocates the next gapless number of (tenant, legal entity, series) inside the issue transaction: a rolled-back
// issue returns the number, so no gap and no duplicate is possible. The conflicting UPDATE row-locks the counter.
func nextSequence(ctx context.Context, tx pgx.Tx, tenantID, entityID, series string) (int64, error) {
	var n int64
	if err := tx.QueryRow(ctx, `INSERT INTO ar06_number_counters (tenant_id, legal_entity_id, series, next_number) VALUES ($1,$2,$3,2)
		ON CONFLICT (tenant_id, legal_entity_id, series) DO UPDATE SET next_number = ar06_number_counters.next_number + 1
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
	IssuedBy         string
	IssuedAt         time.Time
	Canonical        string
	Hash             string
	OpenItemID       string
	EInvoiceRequired bool
}

// writeIssued moves the note to ISSUED with its number, dates, sealed representation, receivable link, APPLIED application
// state and QUEUED accounting state in one statement.
func writeIssued(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, w issueWrite) (*Header, error) {
	einv := EInvNotRequired
	if w.EInvoiceRequired {
		einv = EInvRequired
	}
	out := *cur
	if err := tx.QueryRow(ctx, `UPDATE ar06_corrections SET state = 'ISSUED', document_number = $3, numbering_series = $4, sequence_no = $5,
			issue_date = $6::date, due_date = NULLIF($7,'')::date, issued_by = $8, issued_at = $9, issued_canonical = $10, issued_content_hash = $11, open_item_id = $12,
			einvoice_state = $13, application_state = 'APPLIED', accounting_state = 'QUEUED', accounting_updated_at = $9, version = version + 1, updated_at = $9
		WHERE tenant_id = $1 AND correction_id = $2 AND version = $14 RETURNING version, updated_at`,
		c.TenantID, cur.CorrectionID, w.Number, w.Series, w.Sequence, w.IssueDate, w.DueDate, w.IssuedBy, w.IssuedAt.UTC(), w.Canonical, w.Hash, w.OpenItemID,
		einv, cur.Version).Scan(&out.Version, &out.UpdatedAt); err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, kit.ErrStaleVersion
		case kit.IsUniqueViolation(err, "uq_ar06_document_number"), kit.IsUniqueViolation(err, "uq_ar06_document_sequence"):
			return nil, kit.Conflict(kit.CodeInvoiceNumberCollision, "the legal note number "+w.Number+" is already in use: nothing was issued")
		}
		return nil, kit.MapPgErr(err)
	}
	t := w.IssuedAt
	out.State, out.DocumentNumber, out.NumberingSeries, out.SequenceNo = StateIssued, w.Number, w.Series, w.Sequence
	out.IssueDate, out.DueDate, out.IssuedBy, out.IssuedAt, out.issuedCanonical, out.IssuedContentHash, out.OpenItemID = w.IssueDate, w.DueDate, w.IssuedBy, &t, w.Canonical, w.Hash, w.OpenItemID
	out.EInvoiceState, out.AccountingState, out.ApplicationState = einv, "QUEUED", AppApplied
	return &out, nil
}

// mirrorAccounting records the ACC-04 outcome on the accounting fields only. A POSTED note is never regressed by a later,
// contradictory outcome. It does not bump the version.
func mirrorAccounting(ctx context.Context, tx pgx.Tx, tenantID, correctionID, status, journalID, errText string, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE ar06_corrections SET accounting_state = $3::text, accounting_journal_id = COALESCE(NULLIF($4,''), accounting_journal_id),
			accounting_error = NULLIF($5,''), accounting_updated_at = $6, updated_at = $6
		WHERE tenant_id = $1 AND correction_id::text = $2 AND state IN ('ISSUED','APPLIED','SETTLED')
		  AND (accounting_state <> 'POSTED' OR $3::text = 'POSTED')`, tenantID, correctionID, status, journalID, errText, at.UTC())
	if err != nil {
		return false, kit.MapPgErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// ── replacement links ────────────────────────────────────────────────────────

// ReplacementLink is one replacement invoice linked to an issued correction.
type ReplacementLink struct {
	ReplacementInvoiceID string    `json:"replacement_invoice_id"`
	ReplacementNumber    string    `json:"replacement_number,omitempty"`
	ReasonCode           string    `json:"reason_code,omitempty"`
	LinkedBy             string    `json:"linked_by"`
	LinkedAt             time.Time `json:"linked_at"`
}

func listReplacements(ctx context.Context, tx pgx.Tx, tenantID, correctionID string) ([]ReplacementLink, error) {
	rows, err := tx.Query(ctx, `SELECT replacement_invoice_id::text, replacement_number, COALESCE(reason_code,''), linked_by, linked_at
		FROM ar06_replacement_links WHERE tenant_id = $1 AND correction_id = $2 ORDER BY linked_at, link_id`, tenantID, correctionID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []ReplacementLink{}
	for rows.Next() {
		var l ReplacementLink
		if err := rows.Scan(&l.ReplacementInvoiceID, &l.ReplacementNumber, &l.ReasonCode, &l.LinkedBy, &l.LinkedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
