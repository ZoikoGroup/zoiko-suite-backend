package ar02

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

const headerCols = `quote_id::text, tenant_id::text, legal_entity_id::text, quote_number, customer_party_id::text, currency, state,
	current_version_no, version, created_by, COALESCE(submitted_by,''), COALESCE(approved_version_no,0), COALESCE(approved_content_hash,''),
	COALESCE(approved_by,''), approved_at, sent_at, COALESCE(sent_version_no,0), accepted_at, COALESCE(accepted_by,''),
	COALESCE(acceptance_channel,''), COALESCE(acceptance_evidence_id,''), COALESCE(accepted_version_no,0), rejected_at,
	COALESCE(rejection_reason,''), expired_at, COALESCE(converted_order_id,''), converted_at, COALESCE(converted_version_no,0),
	created_at, updated_at`

func scanHeader(row pgx.Row) (*Header, error) {
	var h Header
	var state string
	if err := row.Scan(&h.QuoteID, &h.TenantID, &h.LegalEntityID, &h.QuoteNumber, &h.CustomerPartyID, &h.Currency, &state,
		&h.CurrentVersionNo, &h.Version, &h.CreatedBy, &h.SubmittedBy, &h.ApprovedVersionNo, &h.ApprovedContentHash,
		&h.ApprovedBy, &h.ApprovedAt, &h.SentAt, &h.SentVersionNo, &h.AcceptedAt, &h.AcceptedBy,
		&h.AcceptanceChannel, &h.AcceptanceEvidenceID, &h.AcceptedVersionNo, &h.RejectedAt,
		&h.RejectionReason, &h.ExpiredAt, &h.ConvertedOrderID, &h.ConvertedAt, &h.ConvertedVersionNo,
		&h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	h.State = State(state)
	return &h, nil
}

func getHeader(ctx context.Context, tx pgx.Tx, tenantID, quoteID string, forUpdate bool) (*Header, error) {
	q := `SELECT ` + headerCols + ` FROM ar02_quotes WHERE tenant_id = $1 AND quote_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	h, err := scanHeader(tx.QueryRow(ctx, q, tenantID, quoteID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("quote not found")
	}
	return h, kit.MapPgErr(err)
}

func getHeaderByNumber(ctx context.Context, tx pgx.Tx, tenantID, entityID, number string) (*Header, error) {
	h, err := scanHeader(tx.QueryRow(ctx, `SELECT `+headerCols+` FROM ar02_quotes
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND quote_number = $3`, tenantID, entityID, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("quote not found")
	}
	return h, kit.MapPgErr(err)
}

const versionCols = `version_no, currency, validity_date::text, terms, assumptions, discount_percent::text, price_source_ref, catalog_version_ref,
	request_tax_estimate, tax_params, subtotal::text, discount_amount::text, net_total::text, content_hash, change_kind, material_change,
	created_by, created_at`

// getVersion loads one immutable version with its lines and tax estimate.
func getVersion(ctx context.Context, tx pgx.Tx, tenantID, quoteID string, versionNo int) (*Version, error) {
	var v Version
	var tax []byte
	err := tx.QueryRow(ctx, `SELECT `+versionCols+` FROM ar02_quote_versions WHERE tenant_id = $1 AND quote_id = $2 AND version_no = $3`,
		tenantID, quoteID, versionNo).Scan(&v.VersionNo, &v.Currency, &v.ValidityDate, &v.Terms, &v.Assumptions, &v.DiscountPercent,
		&v.PriceSourceRef, &v.CatalogVersionRef, &v.RequestTaxEstimate, &tax, &v.Subtotal, &v.DiscountAmount, &v.NetTotal,
		&v.ContentHash, &v.ChangeKind, &v.MaterialChange, &v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("quote version not found")
	}
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	if len(tax) > 0 && string(tax) != "null" {
		v.TaxParams = &TaxParams{}
		if err := json.Unmarshal(tax, v.TaxParams); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT line_no, description, product_ref, quantity::text, unit_price::text, line_net::text FROM ar02_quote_lines
		WHERE tenant_id = $1 AND quote_id = $2 AND version_no = $3 ORDER BY line_no`, tenantID, quoteID, versionNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.LineNo, &l.Description, &l.ProductRef, &l.Quantity, &l.UnitPrice, &l.LineNet); err != nil {
			return nil, err
		}
		v.Lines = append(v.Lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	v.TaxEstimate, err = getTaxEstimate(ctx, tx, tenantID, quoteID, versionNo)
	return &v, err
}

func getTaxEstimate(ctx context.Context, tx pgx.Tx, tenantID, quoteID string, versionNo int) (*TaxEstimate, error) {
	var t TaxEstimate
	err := tx.QueryRow(ctx, `SELECT determination_id, provenance, status, taxable_amount::text, tax_rate, tax_amount::text, currency,
			jurisdiction_id, is_estimate, recorded_by, recorded_at
		FROM ar02_quote_tax_estimates WHERE tenant_id = $1 AND quote_id = $2 AND version_no = $3`, tenantID, quoteID, versionNo).
		Scan(&t.DeterminationID, &t.Provenance, &t.Status, &t.TaxableAmount, &t.TaxRate, &t.TaxAmount, &t.Currency,
			&t.JurisdictionID, &t.IsEstimate, &t.RecordedBy, &t.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

func loadQuote(ctx context.Context, tx pgx.Tx, tenantID string, h *Header, versionNo int) (*Quote, error) {
	if versionNo == 0 {
		versionNo = h.CurrentVersionNo
	}
	v, err := getVersion(ctx, tx, tenantID, h.QuoteID, versionNo)
	if err != nil {
		return nil, err
	}
	return &Quote{Header: *h, Current: v}, nil
}

// ── writes ───────────────────────────────────────────────────────────────────

func jsonb(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// nextNumber allocates the next quote number for (tenant, legal entity) inside the
// command transaction, so a rolled-back create leaves no gap.
func nextNumber(ctx context.Context, tx pgx.Tx, tenantID, entityID string) (string, error) {
	var n int
	if err := tx.QueryRow(ctx, `INSERT INTO ar02_quote_counters (tenant_id, legal_entity_id, last_no) VALUES ($1,$2,1)
		ON CONFLICT (tenant_id, legal_entity_id) DO UPDATE SET last_no = ar02_quote_counters.last_no + 1 RETURNING last_no`,
		tenantID, entityID).Scan(&n); err != nil {
		return "", kit.MapPgErr(err)
	}
	return fmt.Sprintf("Q-%06d", n), nil
}

func insertHeader(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar02_quotes (quote_id, tenant_id, legal_entity_id, quote_number, customer_party_id, currency, state,
			current_version_no, version, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,1,1,$8,$9,$9)`,
		h.QuoteID, c.TenantID, c.LegalEntityID, h.QuoteNumber, h.CustomerPartyID, h.Currency, string(h.State), c.PrincipalID, at.UTC())
	return kit.MapPgErr(err)
}

// insertVersion appends an immutable version and its lines.
func insertVersion(ctx context.Context, tx pgx.Tx, c kit.Caller, quoteID string, versionNo int, kind string, material bool, v Content, t Totals, hash string, at time.Time) error {
	var tax any
	if v.TaxParams != nil {
		tax = jsonb(v.TaxParams)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ar02_quote_versions (quote_id, version_no, tenant_id, legal_entity_id, currency, validity_date, terms, assumptions,
			discount_percent, price_source_ref, catalog_version_ref, request_tax_estimate, tax_params, subtotal, discount_amount, net_total,
			content_hash, change_kind, material_change, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6::date,$7,$8,$9::numeric,$10,$11,$12,$13::jsonb,$14::numeric,$15::numeric,$16::numeric,$17,$18,$19,$20,$21)`,
		quoteID, versionNo, c.TenantID, c.LegalEntityID, v.Currency, v.ValidityDate, v.Terms, v.Assumptions, v.DiscountPercent.String(),
		v.PriceSourceRef, v.CatalogVersionRef, v.RequestTaxEstimate, tax, t.Subtotal.String(), t.DiscountAmount.String(), t.NetTotal.String(),
		hash, kind, material, c.PrincipalID, at.UTC()); err != nil {
		return kit.MapPgErr(err)
	}
	for _, l := range v.Lines {
		if _, err := tx.Exec(ctx, `INSERT INTO ar02_quote_lines (quote_id, version_no, line_no, tenant_id, description, product_ref, quantity, unit_price, line_net)
			VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric,$9::numeric)`,
			quoteID, versionNo, l.LineNo, c.TenantID, l.Description, l.ProductRef, l.Quantity.String(), l.UnitPrice.String(), l.LineNet.String()); err != nil {
			return kit.MapPgErr(err)
		}
	}
	return nil
}

func insertApproval(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, v *Version, preparedBy string, needsAuthority bool, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar02_quote_approvals (approval_id, tenant_id, legal_entity_id, quote_id, version_no, content_hash, discount_percent,
			requires_discount_authority, prepared_by, approved_by, approved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,$11)`,
		uuid.NewString(), c.TenantID, h.LegalEntityID, h.QuoteID, v.VersionNo, v.ContentHash, v.DiscountPercent.String(), needsAuthority,
		preparedBy, c.PrincipalID, at.UTC())
	return kit.MapPgErr(err)
}

func insertTaxEstimate(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, versionNo int, t *TaxEstimate) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar02_quote_tax_estimates (quote_id, version_no, tenant_id, legal_entity_id, determination_id, provenance, status,
			taxable_amount, tax_rate, tax_amount, currency, jurisdiction_id, recorded_by, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9,$10::numeric,$11,$12,$13,$14) ON CONFLICT (quote_id, version_no) DO NOTHING`,
		h.QuoteID, versionNo, c.TenantID, h.LegalEntityID, t.DeterminationID, t.Provenance, t.Status, t.TaxableAmount.String(), t.TaxRate,
		t.TaxAmount.String(), t.Currency, t.JurisdictionID, t.RecordedBy, t.RecordedAt.UTC())
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

func writeHistory(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, versionNo int, state State, hs hist) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ar02_quote_history (history_id, tenant_id, legal_entity_id, quote_id, quote_version, version_no, command,
			state_after, actor_id, approver_id, reason_code, evidence_id, content_hash, prior_values, new_values, correlation_id, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),$14::jsonb,$15::jsonb,$16,$17)`,
		uuid.NewString(), c.TenantID, h.LegalEntityID, h.QuoteID, h.Version, versionNo, hs.Command, string(state), c.PrincipalID,
		hs.ApproverID, hs.Reason, hs.Evidence, hs.ContentHash, jsonb(hs.Prior), jsonb(hs.New), c.CorrelationID, hs.At.UTC()); err != nil {
		return kit.MapPgErr(err)
	}
	return nil
}

func auditDetail(h *Header, hs hist) map[string]any {
	d := map[string]any{"quote_number": h.QuoteNumber, "quote_version": h.Version, "version_no": h.CurrentVersionNo,
		"customer_party_id": h.CustomerPartyID, "content_hash": hs.ContentHash}
	if hs.ApproverID != "" {
		d["approver_id"] = hs.ApproverID
	}
	return d
}

// commitCreate records the creation (header already inserted at version 1).
func commitCreate(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, hs hist, events func(*Header) []kit.Event) error {
	if err := writeHistory(ctx, tx, c, h, h.CurrentVersionNo, h.State, hs); err != nil {
		return err
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: h.QuoteID, Command: hs.Command,
		ReasonCode: hs.Reason, EvidenceID: hs.Evidence, Before: hs.Prior, After: hs.New, Detail: auditDetail(h, hs)}); err != nil {
		return err
	}
	return kit.Emit(ctx, tx, c, events(h)...)
}

// commit persists nh over cur (optimistic concurrency by version, already held by
// the row lock), appends the history row and the kit audit record, and emits the
// events -- all in the caller's transaction.
func commit(ctx context.Context, tx pgx.Tx, c kit.Caller, cur, nh *Header, hs hist, events func(*Header) []kit.Event) (*Header, error) {
	out := *nh
	if err := tx.QueryRow(ctx, `UPDATE ar02_quotes SET state = $3, currency = $4, current_version_no = $5, submitted_by = NULLIF($6,''),
			approved_version_no = NULLIF($7,0), approved_content_hash = NULLIF($8,''), approved_by = NULLIF($9,''), approved_at = $10,
			sent_at = $11, sent_version_no = NULLIF($12,0), accepted_at = $13, accepted_by = NULLIF($14,''), acceptance_channel = NULLIF($15,''),
			acceptance_evidence_id = NULLIF($16,''), accepted_version_no = NULLIF($17,0), rejected_at = $18, rejection_reason = NULLIF($19,''),
			expired_at = $20, converted_order_id = NULLIF($21,''), converted_at = $22, converted_version_no = NULLIF($23,0),
			version = version + 1, updated_at = $24
		WHERE tenant_id = $1 AND quote_id = $2 AND version = $25 RETURNING version, updated_at`,
		c.TenantID, cur.QuoteID, string(nh.State), nh.Currency, nh.CurrentVersionNo, nh.SubmittedBy, nh.ApprovedVersionNo, nh.ApprovedContentHash,
		nh.ApprovedBy, nh.ApprovedAt, nh.SentAt, nh.SentVersionNo, nh.AcceptedAt, nh.AcceptedBy, nh.AcceptanceChannel, nh.AcceptanceEvidenceID,
		nh.AcceptedVersionNo, nh.RejectedAt, nh.RejectionReason, nh.ExpiredAt, nh.ConvertedOrderID, nh.ConvertedAt, nh.ConvertedVersionNo,
		hs.At.UTC(), cur.Version).Scan(&out.Version, &out.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, kit.ErrStaleVersion
		}
		return nil, kit.MapPgErr(err)
	}
	if err := writeHistory(ctx, tx, c, &out, out.CurrentVersionNo, out.State, hs); err != nil {
		return nil, err
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: cur.QuoteID, Command: hs.Command,
		ReasonCode: hs.Reason, EvidenceID: hs.Evidence, Before: hs.Prior, After: hs.New, Detail: auditDetail(&out, hs)}); err != nil {
		return nil, err
	}
	if events != nil {
		if err := kit.Emit(ctx, tx, c, events(&out)...); err != nil {
			return nil, err
		}
	}
	return &out, nil
}
