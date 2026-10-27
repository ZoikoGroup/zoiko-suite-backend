package ar03

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

const headerCols = `order_id::text, tenant_id::text, legal_entity_id::text, order_number, customer_party_id::text, currency, state, hold_active,
	COALESCE(hold_kind,''), COALESCE(hold_reason,''), COALESCE(hold_evidence_id,''), COALESCE(held_by,''), held_at, customer_po_ref,
	COALESCE(source_quote_ref,''), COALESCE(source_ref,''), net_total::text, discount_percent::text, current_version_no, version, created_by,
	COALESCE(submitted_by,''), COALESCE(approved_version_no,0), COALESCE(approved_content_hash,''), COALESCE(approved_by,''), approved_at,
	COALESCE(credit_exception_id::text,''), COALESCE(confirmed_by,''), confirmed_at, COALESCE(cancel_reason,''), COALESCE(cancelled_by,''),
	cancelled_at, COALESCE(close_reason,''), COALESCE(closed_by,''), closed_at, created_at, updated_at`

func scanHeader(row pgx.Row) (*Header, error) {
	var h Header
	var state string
	if err := row.Scan(&h.OrderID, &h.TenantID, &h.LegalEntityID, &h.OrderNumber, &h.CustomerPartyID, &h.Currency, &state, &h.HoldActive,
		&h.HoldKind, &h.HoldReason, &h.HoldEvidenceID, &h.HeldBy, &h.HeldAt, &h.CustomerPORef,
		&h.SourceQuoteRef, &h.SourceRef, &h.NetTotal, &h.DiscountPercent, &h.CurrentVersionNo, &h.Version, &h.CreatedBy,
		&h.SubmittedBy, &h.ApprovedVersionNo, &h.ApprovedHash, &h.ApprovedBy, &h.ApprovedAt,
		&h.CreditExceptionID, &h.ConfirmedBy, &h.ConfirmedAt, &h.CancelReason, &h.CancelledBy,
		&h.CancelledAt, &h.CloseReason, &h.ClosedBy, &h.ClosedAt, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	h.State = State(state)
	return &h, nil
}

func getHeader(ctx context.Context, tx pgx.Tx, tenantID, orderID string, forUpdate bool) (*Header, error) {
	q := `SELECT ` + headerCols + ` FROM ar03_sales_orders WHERE tenant_id = $1 AND order_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	h, err := scanHeader(tx.QueryRow(ctx, q, tenantID, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("sales order not found")
	}
	return h, kit.MapPgErr(err)
}

func getHeaderByNumber(ctx context.Context, tx pgx.Tx, tenantID, entityID, number string) (*Header, error) {
	h, err := scanHeader(tx.QueryRow(ctx, `SELECT `+headerCols+` FROM ar03_sales_orders
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND order_number = $3`, tenantID, entityID, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("sales order not found")
	}
	return h, kit.MapPgErr(err)
}

func getHeaderBySourceRef(ctx context.Context, tx pgx.Tx, tenantID, ref string) (*Header, error) {
	h, err := scanHeader(tx.QueryRow(ctx, `SELECT `+headerCols+` FROM ar03_sales_orders WHERE tenant_id = $1 AND source_ref = $2`, tenantID, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return h, kit.MapPgErr(err)
}

const versionCols = `version_no, currency, customer_po_ref, terms, price_source_ref, catalog_version_ref, discount_percent::text,
	subtotal::text, discount_amount::text, net_total::text, content_hash, lines, change_kind, COALESCE(reason_code,''), COALESCE(evidence_id,''),
	created_by, created_at`

func getVersion(ctx context.Context, tx pgx.Tx, tenantID, orderID string, versionNo int) (*Version, error) {
	var v Version
	var lines []byte
	err := tx.QueryRow(ctx, `SELECT `+versionCols+` FROM ar03_order_versions WHERE tenant_id = $1 AND order_id = $2 AND version_no = $3`,
		tenantID, orderID, versionNo).Scan(&v.VersionNo, &v.Currency, &v.CustomerPORef, &v.Terms, &v.PriceSourceRef, &v.CatalogVersionRef,
		&v.DiscountPercent, &v.Subtotal, &v.DiscountAmount, &v.NetTotal, &v.Hash, &lines, &v.ChangeKind, &v.ReasonCode, &v.EvidenceID,
		&v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("order version not found")
	}
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	if err := json.Unmarshal(lines, &v.Lines); err != nil {
		return nil, err
	}
	return &v, nil
}

const lineCols = `line_no, description, product_ref, ordered_qty::text, unit_price::text, list_net::text, discount_amount::text, line_net::text,
	invoiced_qty::text, invoiced_amount::text, remaining_qty::text, remaining_amount::text`

func getLines(ctx context.Context, tx pgx.Tx, tenantID, orderID string, forUpdate bool) ([]Line, error) {
	q := `SELECT ` + lineCols + ` FROM ar03_order_lines WHERE tenant_id = $1 AND order_id = $2 ORDER BY line_no`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	rows, err := tx.Query(ctx, q, tenantID, orderID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	var out []Line
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.LineNo, &l.Description, &l.ProductRef, &l.Quantity, &l.UnitPrice, &l.ListNet, &l.DiscountAmount, &l.LineNet,
			&l.InvoicedQty, &l.InvoicedAmount, &l.RemainingQty, &l.RemainingAmount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

const milestoneCols = `milestone_id::text, order_id::text, external_ref, line_no, description, scheduled_qty::text, scheduled_amount::text, state,
	fulfillment_source, fulfillment_evidence_ref, recorded_by, recorded_at, COALESCE(invoice_id,''), COALESCE(invoiced_net,0)::text, invoiced_at,
	COALESCE(void_reason,''), voided_at`

func scanMilestone(row pgx.Row) (*Milestone, error) {
	var m Milestone
	if err := row.Scan(&m.MilestoneID, &m.OrderID, &m.ExternalRef, &m.LineNo, &m.Description, &m.ScheduledQty, &m.ScheduledAmount, &m.State,
		&m.FulfillmentSource, &m.FulfillmentEvidenceRef, &m.RecordedBy, &m.RecordedAt, &m.InvoiceID, &m.InvoicedNet, &m.InvoicedAt,
		&m.VoidReason, &m.VoidedAt); err != nil {
		return nil, err
	}
	return &m, nil
}

func getMilestones(ctx context.Context, tx pgx.Tx, tenantID, orderID string) ([]Milestone, error) {
	rows, err := tx.Query(ctx, `SELECT `+milestoneCols+` FROM ar03_billing_milestones WHERE tenant_id = $1 AND order_id = $2
		ORDER BY recorded_at, milestone_id`, tenantID, orderID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []Milestone{}
	for rows.Next() {
		m, err := scanMilestone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// getMilestone loads one milestone of an order; nil when unknown to that order.
func getMilestone(ctx context.Context, tx pgx.Tx, tenantID, orderID, milestoneID string, forUpdate bool) (*Milestone, error) {
	if _, err := uuid.Parse(milestoneID); err != nil {
		return nil, nil
	}
	q := `SELECT ` + milestoneCols + ` FROM ar03_billing_milestones WHERE tenant_id = $1 AND order_id = $2 AND milestone_id = $3`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	m, err := scanMilestone(tx.QueryRow(ctx, q, tenantID, orderID, milestoneID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return m, kit.MapPgErr(err)
}

func loadOrder(ctx context.Context, tx pgx.Tx, tenantID string, h *Header, versionNo int) (*Order, error) {
	if versionNo == 0 {
		versionNo = h.CurrentVersionNo
	}
	v, err := getVersion(ctx, tx, tenantID, h.OrderID, versionNo)
	if err != nil {
		return nil, err
	}
	lines, err := getLines(ctx, tx, tenantID, h.OrderID, false)
	if err != nil {
		return nil, err
	}
	ms, err := getMilestones(ctx, tx, tenantID, h.OrderID)
	if err != nil {
		return nil, err
	}
	return &Order{Header: *h, Current: v, Lines: lines, Milestones: ms, Billing: Summarize(lines)}, nil
}

// ── writes ───────────────────────────────────────────────────────────────────

func jsonb(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// nextNumber allocates the next order number for (tenant, legal entity) inside the
// command transaction, so a rolled-back create leaves no gap.
func nextNumber(ctx context.Context, tx pgx.Tx, tenantID, entityID string) (string, error) {
	var n int
	if err := tx.QueryRow(ctx, `INSERT INTO ar03_order_counters (tenant_id, legal_entity_id, last_no) VALUES ($1,$2,1)
		ON CONFLICT (tenant_id, legal_entity_id) DO UPDATE SET last_no = ar03_order_counters.last_no + 1 RETURNING last_no`,
		tenantID, entityID).Scan(&n); err != nil {
		return "", kit.MapPgErr(err)
	}
	return fmt.Sprintf("SO-%06d", n), nil
}

// allowLineWrites authorises the line triggers for this transaction and this order only:
// order lines are created, replaced or amended solely by a governed command.
func allowLineWrites(ctx context.Context, tx pgx.Tx, orderID string) error {
	_, err := tx.Exec(ctx, `SELECT set_config('ar03.amending', $1, true)`, orderID)
	return err
}

func insertHeader(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar03_sales_orders (order_id, tenant_id, legal_entity_id, order_number, customer_party_id, currency, state,
			customer_po_ref, source_quote_ref, source_ref, net_total, discount_percent, current_version_no, version, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),$11::numeric,$12::numeric,1,1,$13,$14,$14)`,
		h.OrderID, c.TenantID, h.LegalEntityID, h.OrderNumber, h.CustomerPartyID, h.Currency, string(h.State), h.CustomerPORef,
		h.SourceQuoteRef, h.SourceRef, h.NetTotal.String(), h.DiscountPercent.String(), c.PrincipalID, at.UTC())
	return kit.MapPgErr(err)
}

func insertVersion(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, versionNo int, kind, reason, evidence string, v Content, t Totals, hash string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar03_order_versions (order_id, version_no, tenant_id, legal_entity_id, currency, customer_po_ref, terms,
			price_source_ref, catalog_version_ref, discount_percent, subtotal, discount_amount, net_total, content_hash, lines, change_kind,
			reason_code, evidence_id, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::numeric,$11::numeric,$12::numeric,$13::numeric,$14,$15::jsonb,$16,NULLIF($17,''),NULLIF($18,''),$19,$20)`,
		h.OrderID, versionNo, c.TenantID, h.LegalEntityID, v.Currency, v.CustomerPORef, v.Terms, v.PriceSourceRef, v.CatalogVersionRef,
		v.DiscountPercent.String(), t.Subtotal.String(), t.DiscountAmount.String(), t.NetTotal.String(), hash, jsonb(v.Lines), kind,
		reason, evidence, c.PrincipalID, at.UTC())
	return kit.MapPgErr(err)
}

func insertLines(ctx context.Context, tx pgx.Tx, c kit.Caller, orderID string, lines []ContentLine) error {
	if err := allowLineWrites(ctx, tx, orderID); err != nil {
		return err
	}
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `INSERT INTO ar03_order_lines (order_id, line_no, tenant_id, description, product_ref, ordered_qty, unit_price,
				list_net, discount_amount, line_net)
			VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10::numeric)`,
			orderID, l.LineNo, c.TenantID, l.Description, l.ProductRef, l.Quantity.String(), l.UnitPrice.String(), l.ListNet.String(),
			l.DiscountAmount.String(), l.LineNet.String()); err != nil {
			return kit.MapPgErr(err)
		}
	}
	return nil
}

// replaceLines swaps every line of a never-billed order (pre-confirmation amendment).
func replaceLines(ctx context.Context, tx pgx.Tx, c kit.Caller, orderID string, lines []ContentLine) error {
	if err := allowLineWrites(ctx, tx, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM ar03_order_lines WHERE tenant_id = $1 AND order_id = $2`, c.TenantID, orderID); err != nil {
		return kit.MapPgErr(err)
	}
	return insertLines(ctx, tx, c, orderID, lines)
}

// updateLines amends the commercial fields of existing lines in place (post-confirmation
// reduction); the line triggers and CHECKs refuse anything that would go below invoiced.
func updateLines(ctx context.Context, tx pgx.Tx, c kit.Caller, orderID string, lines []ContentLine) error {
	if err := allowLineWrites(ctx, tx, orderID); err != nil {
		return err
	}
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `UPDATE ar03_order_lines SET description = $4, product_ref = $5, ordered_qty = $6::numeric, unit_price = $7::numeric,
				list_net = $8::numeric, discount_amount = $9::numeric, line_net = $10::numeric
			WHERE tenant_id = $1 AND order_id = $2 AND line_no = $3`,
			c.TenantID, orderID, l.LineNo, l.Description, l.ProductRef, l.Quantity.String(), l.UnitPrice.String(), l.ListNet.String(),
			l.DiscountAmount.String(), l.LineNet.String()); err != nil {
			return mapBilledErr(err)
		}
	}
	return nil
}

// mapBilledErr turns the CHECK that protects billed lines into the stable immutability class.
func mapBilledErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		switch pgErr.ConstraintName {
		case "ar03_invoiced_within_ordered", "ar03_remaining_non_negative":
			return kit.Conflict(kit.CodeImmutable, "a billed line cannot be reduced below what was already invoiced")
		}
	}
	return kit.MapPgErr(err)
}

func insertApproval(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, v *Version, sodRequired bool, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar03_order_approvals (approval_id, tenant_id, legal_entity_id, order_id, version_no, content_hash, net_total,
			sod_required, created_by, submitted_by, approved_by, approved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,$11,$12)`,
		uuid.NewString(), c.TenantID, h.LegalEntityID, h.OrderID, v.VersionNo, v.Hash, v.NetTotal.String(), sodRequired, h.CreatedBy, h.SubmittedBy,
		c.PrincipalID, at.UTC())
	return kit.MapPgErr(err)
}

// CreditExceptionInput is a recorded, approved credit exception.
type CreditExceptionInput struct {
	ReasonCode string
	EvidenceID string
	Check      CreditResult
}

func insertCreditException(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header, v *Version, in CreditExceptionInput, at time.Time) (string, error) {
	id := uuid.NewString()
	_, err := tx.Exec(ctx, `INSERT INTO ar03_credit_exceptions (exception_id, tenant_id, legal_entity_id, order_id, version_no, content_hash, order_net,
			reason_code, evidence_id, created_by, submitted_by, approved_by, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,NULLIF($11,''),$12,$13)`,
		id, c.TenantID, h.LegalEntityID, h.OrderID, v.VersionNo, v.Hash, v.NetTotal.String(), in.ReasonCode, in.EvidenceID, h.CreatedBy,
		h.SubmittedBy, c.PrincipalID, at.UTC())
	return id, kit.MapPgErr(err)
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
	_, err := tx.Exec(ctx, `INSERT INTO ar03_order_history (history_id, tenant_id, legal_entity_id, order_id, order_version, version_no, command,
			state_after, actor_id, approver_id, reason_code, evidence_id, content_hash, prior_values, new_values, correlation_id, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),$14::jsonb,$15::jsonb,$16,$17)`,
		uuid.NewString(), c.TenantID, h.LegalEntityID, h.OrderID, h.Version, versionNo, hs.Command, string(state), c.PrincipalID,
		hs.ApproverID, hs.Reason, hs.Evidence, hs.ContentHash, jsonb(hs.Prior), jsonb(hs.New), c.CorrelationID, hs.At.UTC())
	return kit.MapPgErr(err)
}

func auditDetail(h *Header, hs hist) map[string]any {
	d := map[string]any{"order_number": h.OrderNumber, "order_version": h.Version, "version_no": h.CurrentVersionNo,
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
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: h.OrderID, Command: hs.Command,
		ReasonCode: hs.Reason, EvidenceID: hs.Evidence, Before: hs.Prior, After: hs.New, Detail: auditDetail(h, hs)}); err != nil {
		return err
	}
	return kit.Emit(ctx, tx, c, events(h)...)
}

// commit persists nh over cur (optimistic concurrency by version, already held by the row
// lock), appends the history row and the kit audit record and emits the events -- all in
// the caller's transaction.
func commit(ctx context.Context, tx pgx.Tx, c kit.Caller, cur, nh *Header, hs hist, events func(*Header) []kit.Event) (*Header, error) {
	out := *nh
	if err := tx.QueryRow(ctx, `UPDATE ar03_sales_orders SET state = $3, hold_active = $4, hold_kind = NULLIF($5,''), hold_reason = NULLIF($6,''),
			hold_evidence_id = NULLIF($7,''), held_by = NULLIF($8,''), held_at = $9, customer_po_ref = $10, net_total = $11::numeric,
			discount_percent = $12::numeric, current_version_no = $13, submitted_by = NULLIF($14,''), approved_version_no = NULLIF($15,0),
			approved_content_hash = NULLIF($16,''), approved_by = NULLIF($17,''), approved_at = $18, credit_exception_id = NULLIF($19,'')::uuid,
			confirmed_by = NULLIF($20,''), confirmed_at = $21, cancel_reason = NULLIF($22,''), cancelled_by = NULLIF($23,''), cancelled_at = $24,
			close_reason = NULLIF($25,''), closed_by = NULLIF($26,''), closed_at = $27, version = version + 1, updated_at = $28
		WHERE tenant_id = $1 AND order_id = $2 AND version = $29 RETURNING version, updated_at`,
		c.TenantID, cur.OrderID, string(nh.State), nh.HoldActive, nh.HoldKind, nh.HoldReason, nh.HoldEvidenceID, nh.HeldBy, nh.HeldAt,
		nh.CustomerPORef, nh.NetTotal.String(), nh.DiscountPercent.String(), nh.CurrentVersionNo, nh.SubmittedBy, nh.ApprovedVersionNo,
		nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt, nh.CreditExceptionID, nh.ConfirmedBy, nh.ConfirmedAt, nh.CancelReason, nh.CancelledBy,
		nh.CancelledAt, nh.CloseReason, nh.ClosedBy, nh.ClosedAt, hs.At.UTC(), cur.Version).Scan(&out.Version, &out.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, kit.ErrStaleVersion
		}
		return nil, kit.MapPgErr(err)
	}
	if err := writeHistory(ctx, tx, c, &out, out.CurrentVersionNo, out.State, hs); err != nil {
		return nil, err
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: cur.OrderID, Command: hs.Command,
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

// voidOpenMilestones voids every RECORDED (eligible but not yet billed) milestone of an
// order that is being cancelled or short-closed, so no eligibility survives it.
func voidOpenMilestones(ctx context.Context, tx pgx.Tx, tenantID, orderID, reason string, at time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE ar03_billing_milestones SET state = 'VOIDED', void_reason = $3, voided_at = $4
		WHERE tenant_id = $1 AND order_id = $2 AND state = 'RECORDED'`, tenantID, orderID, reason, at.UTC())
	return tag.RowsAffected(), kit.MapPgErr(err)
}
