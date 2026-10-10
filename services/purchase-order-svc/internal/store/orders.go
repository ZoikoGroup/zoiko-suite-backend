package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/purchase-order-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
	"zoiko.io/purchase-order-svc/internal/outbox"
)

// orderColumns is every purchase_orders column the API exposes, in scanOrder's
// order. Text columns that are NULL for a draft or a legacy row are COALESCEd
// so a missing value is the empty string, never a scan error.
const orderColumns = `
	purchase_order_id, tenant_id, legal_entity_id, purchase_request_id, vendor_profile_id, supplier_ref,
	po_number, po_status, total_amount, currency_code, version, revision,
	COALESCE(delivery_terms, ''), COALESCE(payment_terms, ''),
	COALESCE(prepared_by_principal_id, ''), submitted_by_principal_id, submitted_at,
	approved_by_principal_id, approved_at, COALESCE(approval_basis, ''), COALESCE(approval_ref, ''),
	held_by_principal_id, held_at, COALESCE(hold_reason, ''), COALESCE(held_from_status, ''),
	cancelled_by_principal_id, cancelled_at, COALESCE(cancellation_reason, ''),
	COALESCE(supplier_exception_ref, ''), COALESCE(supplier_exception_by, ''),
	COALESCE(issued_by_principal_id, ''), closed_by_principal_id, correlation_id,
	created_at, issued_at, closed_at`

func scanOrder(row pgx.Row) (*domain.PurchaseOrder, error) {
	var o domain.PurchaseOrder
	var status string
	err := row.Scan(
		&o.PurchaseOrderID, &o.TenantID, &o.LegalEntityID, &o.PurchaseRequestID, &o.VendorProfileID, &o.SupplierRef,
		&o.PONumber, &status, &o.TotalAmount, &o.CurrencyCode, &o.Version, &o.Revision,
		&o.DeliveryTerms, &o.PaymentTerms,
		&o.PreparedByPrincipalID, &o.SubmittedByPrincipalID, &o.SubmittedAt,
		&o.ApprovedByPrincipalID, &o.ApprovedAt, &o.ApprovalBasis, &o.ApprovalRef,
		&o.HeldByPrincipalID, &o.HeldAt, &o.HoldReason, &o.HeldFromStatus,
		&o.CancelledBy, &o.CancelledAt, &o.CancellationReason,
		&o.SupplierExceptionRef, &o.SupplierExceptionBy,
		&o.IssuedByPrincipalID, &o.ClosedByPrincipalID, &o.CorrelationID,
		&o.CreatedAt, &o.IssuedAt, &o.ClosedAt,
	)
	if err != nil {
		return nil, err
	}
	o.Status = domain.OrderStatus(status)
	return &o, nil
}

const lineColumns = `
	line_id, line_number, COALESCE(item_ref, ''), COALESCE(description, ''), quantity, unit_price,
	COALESCE(uom, ''), line_amount, delivery_date, COALESCE(delivery_location, '')`

func scanLine(row pgx.Row) (domain.Line, error) {
	var l domain.Line
	err := row.Scan(&l.LineID, &l.LineNumber, &l.ItemRef, &l.Description, &l.Quantity, &l.UnitPrice,
		&l.UOM, &l.LineAmount, &l.DeliveryDate, &l.DeliveryLocation)
	return l, err
}

// loadLines returns the order's lines in line-number order. forUpdate locks
// them for the rest of the transaction.
func loadLines(ctx context.Context, tx pgx.Tx, tenantID, orderID string, forUpdate bool) ([]domain.Line, error) {
	q := `SELECT ` + lineColumns + ` FROM purchase_order_lines WHERE purchase_order_id = $1 AND tenant_id = $2 ORDER BY line_number`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	rows, err := tx.Query(ctx, q, orderID, tenantID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()
	out := []domain.Line{}
	for rows.Next() {
		l, err := scanLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// loadOrderDetail returns the order and its lines, locking both when forUpdate.
func loadOrderDetail(ctx context.Context, tx pgx.Tx, tenantID, orderID string, forUpdate bool) (*domain.OrderDetail, error) {
	q := `SELECT ` + orderColumns + ` FROM purchase_orders WHERE purchase_order_id = $1 AND tenant_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	o, err := scanOrder(tx.QueryRow(ctx, q, orderID, tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrOrderNotFound
		}
		// A malformed id names no order: not found, not "found in a wrong state".
		mapped := mapPgError(err)
		if errors.Is(mapped, domain.ErrInvalidIdentifier) {
			return nil, domain.ErrOrderNotFound
		}
		return nil, mapped
	}
	lines, err := loadLines(ctx, tx, tenantID, orderID, forUpdate)
	if err != nil {
		return nil, err
	}
	return &domain.OrderDetail{PurchaseOrder: *o, Lines: lines}, nil
}

// ── outbox + history ────────────────────────────────────────────────────────

// Spec event names (ZS-SVC-D-001 §6 "Events produced") plus the two this
// service adds for the steps the spec names as commands but not as events.
const (
	EventCreated      = "PurchaseOrderCreated"
	EventSubmitted    = "PurchaseOrderSubmitted"
	EventApproved     = "PurchaseOrderApproved"
	EventIssued       = "PurchaseOrderIssued"
	EventAmended      = "PurchaseOrderAmended"
	EventHeld         = "PurchaseOrderHeld"
	EventHoldReleased = "PurchaseOrderHoldReleased"
	EventClosed       = "PurchaseOrderClosed"
	EventCancelled    = "PurchaseOrderCancelled"
)

// legacyAlias maps a spec event to the dotted name this service published
// before the outbox, so existing consumers keep receiving what they subscribed
// to. Both are written, in the same transaction.
var legacyAlias = map[string]string{
	EventIssued:  "purchase.order.issued",
	EventAmended: "purchase.order.amended",
	EventClosed:  "purchase.order.closed",
}

// record writes the order's history row and its outbox event(s) inside tx. The
// payload is the order as it stands AFTER the change (header + lines).
func (s *PgStore) record(ctx context.Context, tx pgx.Tx, eventType string, from domain.OrderStatus, d *domain.OrderDetail, actor, detail string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO purchase_order_events (event_id, tenant_id, purchase_order_id, event_type, from_status, to_status, revision, version, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9, $10)`,
		uuid.NewString(), d.TenantID, d.PurchaseOrderID, eventType, string(from), string(d.Status), d.Revision, d.Version, detail, actor,
	); err != nil {
		return err
	}
	types := []string{eventType}
	if alias, ok := legacyAlias[eventType]; ok {
		types = append(types, alias)
	}
	corr := svcmiddleware.CorrelationIDFromContext(ctx)
	if corr == "" {
		corr = d.CorrelationID
	}
	for _, t := range types {
		id := uuid.NewString()
		env, err := outbox.NewEnvelope(id, t, d.TenantID, d.LegalEntityID, actor, corr, d)
		if err != nil {
			return err
		}
		tenant, actorCopy, corrCopy := d.TenantID, actor, corr
		if err := outbox.Insert(ctx, tx, outbox.Event{
			OutboxEventID: id, AggregateType: "purchase_order", AggregateID: d.PurchaseOrderID, EventType: t,
			TenantID: &tenant, LegalEntityID: d.LegalEntityID, ActorID: &actorCopy, CorrelationID: &corrCopy, Payload: env,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ── create ──────────────────────────────────────────────────────────────────

// CreateDraftInput is everything CreateDraft stores. The handler has already
// validated it, resolved the tenant from the verified header and checked the
// supplier's eligibility.
type CreateDraftInput struct {
	TenantID             string
	LegalEntityID        string
	PurchaseRequestID    *string
	VendorProfileID      *string
	SupplierRef          string
	CurrencyCode         string
	CorrelationID        string
	DeliveryTerms        string
	PaymentTerms         string
	SupplierExceptionRef string
	SupplierExceptionBy  string
	PreparedBy           string
	Lines                []domain.LineInput
	// TotalAmount is used only for a header-only (legacy) order, which has no
	// lines to sum.
	TotalAmount float64
}

// IssuedInput is the legacy direct-issue: the order is created and issued in
// one governed step, with its approval already verified at the source.
type IssuedInput struct {
	CreateDraftInput
	ApprovalBasis string
	ApprovalRef   string
	ApprovedBy    string
}

func (s *PgStore) insertDraft(ctx context.Context, tx pgx.Tx, in CreateDraftInput) (detail *domain.OrderDetail, created bool, err error) {
	var seq int64
	if err := tx.QueryRow(ctx, "SELECT nextval('purchase_order_number_seq')").Scan(&seq); err != nil {
		return nil, false, err
	}
	poNumber := fmt.Sprintf("PO-%06d", seq)
	orderID := uuid.NewString()

	total := in.TotalAmount
	if len(in.Lines) > 0 {
		total = 0
		for _, l := range in.Lines {
			total += l.Amount()
		}
		total = domain.RoundMoney(total)
	}

	var supplier any
	if in.SupplierRef != "" {
		supplier = in.SupplierRef
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO purchase_orders (
			purchase_order_id, tenant_id, legal_entity_id, purchase_request_id, vendor_profile_id, supplier_ref,
			po_number, po_status, total_amount, currency_code, version, revision,
			delivery_terms, payment_terms, prepared_by_principal_id, supplier_exception_ref, supplier_exception_by,
			correlation_id, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'DRAFT',$8,$9,1,1,$10,$11,$12,$13,$14,$15,NOW())
		ON CONFLICT (tenant_id, correlation_id) DO NOTHING`,
		orderID, in.TenantID, in.LegalEntityID, in.PurchaseRequestID, in.VendorProfileID, supplier,
		poNumber, total, in.CurrencyCode,
		in.DeliveryTerms, in.PaymentTerms, in.PreparedBy, in.SupplierExceptionRef, in.SupplierExceptionBy,
		in.CorrelationID)
	if err != nil {
		return nil, false, err
	}
	if tag.RowsAffected() == 0 {
		// A retry: the order for this (tenant, correlation_id) already exists.
		var existingID string
		if err := tx.QueryRow(ctx, `SELECT purchase_order_id::text FROM purchase_orders WHERE tenant_id = $1 AND correlation_id = $2`,
			in.TenantID, in.CorrelationID).Scan(&existingID); err != nil {
			return nil, false, err
		}
		d, err := loadOrderDetail(ctx, tx, in.TenantID, existingID, false)
		return d, false, err
	}

	for i, l := range in.Lines {
		number := l.LineNumber
		if number == 0 {
			number = i + 1
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO purchase_order_lines (line_id, tenant_id, purchase_order_id, line_number, item_ref, description,
				quantity, unit_price, uom, line_amount, delivery_date, delivery_location)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			uuid.NewString(), in.TenantID, orderID, number, l.ItemRef, l.Description,
			l.Quantity, l.UnitPrice, l.UOM, l.Amount(), l.DeliveryDate, l.DeliveryLocation); err != nil {
			return nil, false, err
		}
	}
	d, err := loadOrderDetail(ctx, tx, in.TenantID, orderID, false)
	return d, true, err
}

// CreateDraft creates a PO in DRAFT with its lines. Idempotent on
// (tenant_id, correlation_id): a retry returns the existing order with
// created=false and publishes nothing.
func (s *PgStore) CreateDraft(ctx context.Context, in CreateDraftInput) (*domain.OrderDetail, bool, error) {
	var d *domain.OrderDetail
	var created bool
	err := s.withRLS(ctx, in.TenantID, func(tx pgx.Tx) error {
		var err error
		d, created, err = s.insertDraft(ctx, tx, in)
		if err != nil || !created {
			return err
		}
		return s.record(ctx, tx, EventCreated, "", d, in.PreparedBy, "draft created")
	})
	if err != nil {
		return nil, false, mapPgError(err)
	}
	return d, created, nil
}

// CreateIssued creates the order and takes it DRAFT -> PENDING_APPROVAL ->
// APPROVED -> ISSUED in ONE transaction, recording the verified approval basis.
// Every step is an ordinary state-machine transition, so the database triggers
// judge it like any other: there is no path that skips approval, only one that
// brings its approval from a source this service has verified. The order is
// reported at version 1, as the legacy endpoint always did.
func (s *PgStore) CreateIssued(ctx context.Context, in IssuedInput) (*domain.OrderDetail, bool, error) {
	var d *domain.OrderDetail
	var created bool
	err := s.withRLS(ctx, in.TenantID, func(tx pgx.Tx) error {
		var err error
		d, created, err = s.insertDraft(ctx, tx, in.CreateDraftInput)
		if err != nil || !created {
			return err
		}
		if err := s.record(ctx, tx, EventCreated, "", d, in.PreparedBy, "draft created for direct issue"); err != nil {
			return err
		}

		now := time.Now().UTC()
		steps := []struct {
			sql  string
			args []any
			from domain.OrderStatus
			evt  string
		}{
			{`UPDATE purchase_orders SET po_status = 'PENDING_APPROVAL', submitted_by_principal_id = $3, submitted_at = $4,
				approval_basis = $5, approval_ref = $6 WHERE purchase_order_id = $1 AND tenant_id = $2`,
				[]any{in.PreparedBy, now, in.ApprovalBasis, in.ApprovalRef}, domain.OrderStatusDraft, EventSubmitted},
			{`UPDATE purchase_orders SET po_status = 'APPROVED', approved_by_principal_id = $3, approved_at = $4
				WHERE purchase_order_id = $1 AND tenant_id = $2`,
				[]any{in.ApprovedBy, now}, domain.OrderStatusPendingApproval, EventApproved},
			{`UPDATE purchase_orders SET po_status = 'ISSUED', issued_by_principal_id = $3, issued_at = $4, version = 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`,
				[]any{in.PreparedBy, now}, domain.OrderStatusApproved, EventIssued},
		}
		for _, st := range steps {
			args := append([]any{d.PurchaseOrderID, in.TenantID}, st.args...)
			if _, err := tx.Exec(ctx, st.sql, args...); err != nil {
				return err
			}
			next, err := loadOrderDetail(ctx, tx, in.TenantID, d.PurchaseOrderID, false)
			if err != nil {
				return err
			}
			if err := s.record(ctx, tx, st.evt, st.from, next, in.PreparedBy, "approval basis "+in.ApprovalBasis); err != nil {
				return err
			}
			d = next
		}
		return nil
	})
	if err != nil {
		return nil, false, mapPgError(err)
	}
	return d, created, nil
}

// ── reads ───────────────────────────────────────────────────────────────────

// GetOrderDetail returns the order with its lines, or (nil, nil) when it does
// not exist in the caller's tenant — including a malformed id, which names no
// order. Tenant scope is an explicit filter as well as RLS (see package doc).
func (s *PgStore) GetOrderDetail(ctx context.Context, orderID string) (*domain.OrderDetail, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}
	var d *domain.OrderDetail
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		d, err = loadOrderDetail(ctx, tx, tenantID, orderID, false)
		return err
	})
	if errors.Is(err, domain.ErrOrderNotFound) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ListEvents returns the order's append-only history, oldest first.
func (s *PgStore) ListEvents(ctx context.Context, orderID string) ([]domain.OrderEvent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}
	var out []domain.OrderEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id::text, event_type, COALESCE(from_status, ''), COALESCE(to_status, ''), revision, version, detail, actor_principal_id, created_at
			FROM purchase_order_events WHERE purchase_order_id = $1 AND tenant_id = $2 ORDER BY created_at, event_id`, orderID, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.OrderEvent
			var from, to string
			if err := rows.Scan(&e.EventID, &e.EventType, &from, &to, &e.Revision, &e.Version, &e.Detail, &e.ActorID, &e.CreatedAt); err != nil {
				return err
			}
			e.FromStatus, e.ToStatus = domain.OrderStatus(from), domain.OrderStatus(to)
			out = append(out, e)
		}
		return rows.Err()
	})
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	return out, err
}

// ListRevisions returns the superseded revisions (oldest first). The current
// revision is the order itself.
func (s *PgStore) ListRevisions(ctx context.Context, orderID string) ([]domain.Revision, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}
	var out []domain.Revision
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT revision_id::text, purchase_order_id::text, revision, status_at_snapshot, snapshot,
			       approved_by_principal_id, approved_at, reason, created_by_principal_id, created_at
			FROM purchase_order_revisions WHERE purchase_order_id = $1 AND tenant_id = $2 ORDER BY revision`, orderID, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.Revision
			var status string
			var raw []byte
			if err := rows.Scan(&r.RevisionID, &r.PurchaseOrderID, &r.Revision, &status, &raw,
				&r.ApprovedByPrincipalID, &r.ApprovedAt, &r.Reason, &r.CreatedByPrincipalID, &r.CreatedAt); err != nil {
				return err
			}
			r.StatusAtSnapshot = domain.OrderStatus(status)
			if err := json.Unmarshal(raw, &r.Snapshot); err != nil {
				return fmt.Errorf("decode revision %d snapshot: %w", r.Revision, err)
			}
			r.SupersededByRevision = r.Revision + 1
			out = append(out, r)
		}
		return rows.Err()
	})
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	return out, err
}
