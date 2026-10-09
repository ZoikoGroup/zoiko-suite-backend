package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/purchase-order-svc/internal/domain"
)

// Command is a lifecycle command that moves a PO between states.
type Command string

const (
	CmdSubmit  Command = "SUBMIT"
	CmdApprove Command = "APPROVE"
	CmdIssue   Command = "ISSUE"
	CmdHold    Command = "HOLD"
	CmdRelease Command = "RELEASE"
	CmdCancel  Command = "CANCEL"
	CmdClose   Command = "CLOSE"
)

// TransitionInput is one lifecycle command against one order.
type TransitionInput struct {
	TenantID string
	OrderID  string
	Actor    string
	Command  Command
	// ExpectedVersion, when set, must equal the order's current version or the
	// command is refused as STALE_VERSION.
	ExpectedVersion *int
	Reason          string
	// SupplierExceptionRef/By record an approved exception under which a PO is
	// issued to a supplier that is on hold (verified by the handler).
	SupplierExceptionRef string
	SupplierExceptionBy  string
}

type commandSpec struct {
	from  []domain.OrderStatus
	event string
}

var commandSpecs = map[Command]commandSpec{
	CmdSubmit:  {[]domain.OrderStatus{domain.OrderStatusDraft}, EventSubmitted},
	CmdApprove: {[]domain.OrderStatus{domain.OrderStatusPendingApproval}, EventApproved},
	CmdIssue:   {[]domain.OrderStatus{domain.OrderStatusApproved}, EventIssued},
	CmdHold:    {[]domain.OrderStatus{domain.OrderStatusApproved, domain.OrderStatusIssued}, EventHeld},
	CmdRelease: {[]domain.OrderStatus{domain.OrderStatusOnHold}, EventHoldReleased},
	CmdCancel: {[]domain.OrderStatus{domain.OrderStatusDraft, domain.OrderStatusPendingApproval, domain.OrderStatusApproved,
		domain.OrderStatusIssued, domain.OrderStatusOnHold}, EventCancelled},
	CmdClose: {[]domain.OrderStatus{domain.OrderStatusIssued}, EventClosed},
}

func (c commandSpec) allows(s domain.OrderStatus) bool {
	for _, f := range c.from {
		if f == s {
			return true
		}
	}
	return false
}

// Transition applies one lifecycle command. The order row is locked, the
// state-machine, version and maker-checker rules are checked, the change is
// written, and its history row and outbox events commit in the same
// transaction. The database triggers (migration 000007) repeat the structural
// rules, so a code path that forgot one still cannot commit.
func (s *PgStore) Transition(ctx context.Context, in TransitionInput) (*domain.OrderDetail, error) {
	spec, ok := commandSpecs[in.Command]
	if !ok {
		return nil, fmt.Errorf("unknown command %q", in.Command)
	}
	var out *domain.OrderDetail
	err := s.withRLS(ctx, in.TenantID, func(tx pgx.Tx) error {
		cur, err := loadOrderDetail(ctx, tx, in.TenantID, in.OrderID, true)
		if err != nil {
			return err
		}
		if !spec.allows(cur.Status) {
			return domain.ErrInvalidTransition
		}
		if in.ExpectedVersion != nil && cur.Version != *in.ExpectedVersion {
			return domain.ErrStaleVersion
		}

		now := time.Now().UTC()
		var tag string
		switch in.Command {
		case CmdSubmit:
			// A governed PO needs lines; a legacy header-only order needs a total.
			if len(cur.Lines) == 0 && cur.TotalAmount <= 0 {
				return domain.ErrNoLines
			}
			tag, err = exec(ctx, tx, `
				UPDATE purchase_orders SET po_status = 'PENDING_APPROVAL', submitted_by_principal_id = $3, submitted_at = $4,
					version = version + 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`, in.OrderID, in.TenantID, in.Actor, now)

		case CmdApprove:
			// Maker-checker. The trigger repeats it; checking here gives the
			// caller a typed error instead of a raised exception.
			if in.Actor == cur.PreparedByPrincipalID || (cur.SubmittedByPrincipalID != nil && in.Actor == *cur.SubmittedByPrincipalID) {
				return domain.ErrSoDConflict
			}
			tag, err = exec(ctx, tx, `
				UPDATE purchase_orders SET po_status = 'APPROVED', approved_by_principal_id = $3, approved_at = $4,
					approval_basis = $5, approval_ref = '', version = version + 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`, in.OrderID, in.TenantID, in.Actor, now, domain.ApprovalBasisWorkflow)

		case CmdIssue:
			tag, err = exec(ctx, tx, `
				UPDATE purchase_orders SET po_status = 'ISSUED', issued_by_principal_id = $3, issued_at = $4,
					supplier_exception_ref = COALESCE(NULLIF($5, ''), supplier_exception_ref),
					supplier_exception_by = COALESCE(NULLIF($6, ''), supplier_exception_by), version = version + 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`, in.OrderID, in.TenantID, in.Actor, now, in.SupplierExceptionRef, in.SupplierExceptionBy)

		case CmdHold:
			tag, err = exec(ctx, tx, `
				UPDATE purchase_orders SET po_status = 'ON_HOLD', held_from_status = po_status, hold_reason = $3,
					held_by_principal_id = $4, held_at = $5, version = version + 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`, in.OrderID, in.TenantID, in.Reason, in.Actor, now)

		case CmdRelease:
			back := cur.HeldFromStatus
			if back != string(domain.OrderStatusApproved) && back != string(domain.OrderStatusIssued) {
				return fmt.Errorf("order %s is on hold with no valid held_from_status %q", in.OrderID, back)
			}
			tag, err = exec(ctx, tx, `
				UPDATE purchase_orders SET po_status = $3, held_from_status = NULL, hold_reason = NULL,
					held_by_principal_id = NULL, held_at = NULL, version = version + 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`, in.OrderID, in.TenantID, back)

		case CmdCancel:
			// Receipts or invoices already exist: the commitment is partly
			// performed and cannot simply be cancelled.
			var progress float64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(received_quantity + invoiced_quantity), 0)
				FROM purchase_order_lines WHERE purchase_order_id = $1 AND tenant_id = $2`, in.OrderID, in.TenantID).Scan(&progress); err != nil {
				return err
			}
			if progress > 0 {
				return domain.ErrHasProgress
			}
			tag, err = exec(ctx, tx, `
				UPDATE purchase_orders SET po_status = 'CANCELLED', cancellation_reason = $3,
					cancelled_by_principal_id = $4, cancelled_at = $5, version = version + 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`, in.OrderID, in.TenantID, in.Reason, in.Actor, now)

		case CmdClose:
			tag, err = exec(ctx, tx, `
				UPDATE purchase_orders SET po_status = 'CLOSED', closed_by_principal_id = $3, closed_at = $4,
					version = version + 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`, in.OrderID, in.TenantID, in.Actor, now)
		}
		if err != nil {
			return err
		}
		if tag == "" {
			return domain.ErrInvalidTransition
		}

		out, err = loadOrderDetail(ctx, tx, in.TenantID, in.OrderID, false)
		if err != nil {
			return err
		}
		return s.record(ctx, tx, spec.event, cur.Status, out, in.Actor, in.Reason)
	})
	if err != nil {
		return nil, mapTransitionErr(err)
	}
	return out, nil
}

// exec runs an UPDATE and returns a non-empty tag when it touched a row.
func exec(ctx context.Context, tx pgx.Tx, sql string, args ...any) (string, error) {
	t, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	if t.RowsAffected() == 0 {
		return "", nil
	}
	return "ok", nil
}

// mapTransitionErr leaves typed domain errors alone and turns a trigger's
// raised exception (P0001) into an invalid transition: the database refused a
// change the state machine does not allow.
func mapTransitionErr(err error) error {
	for _, known := range []error{domain.ErrInvalidTransition, domain.ErrStaleVersion, domain.ErrSoDConflict,
		domain.ErrNoLines, domain.ErrHasProgress, domain.ErrOrderNotFound, domain.ErrAmendmentBelowProgress,
		domain.ErrInvalidLine, domain.ErrInvalidIdentifier} {
		if errors.Is(err, known) {
			return err
		}
	}
	err = mapPgError(err)
	if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return err
	}
	return err
}

// ── amend ───────────────────────────────────────────────────────────────────

// AmendResult is the outcome of an amendment.
type AmendResult struct {
	Order domain.OrderDetail
	// Material: a field needing re-approval changed. RequiresReapproval: the
	// order has been taken back to DRAFT and must be submitted and approved again.
	Material           bool
	RequiresReapproval bool
	// NewRevision is true when the superseded revision was snapshotted.
	NewRevision bool
}

// ErrNoChange is returned when an amendment changes nothing.
var ErrNoChange = errors.New("the amendment changes nothing")

// AmendOrder applies an amendment under the spec's revision rules:
//
//   - DRAFT: edited in place (it has never been approved).
//   - PENDING_APPROVAL: edited in place and sent back to DRAFT — the approval
//     under way was for different content, so it is withdrawn.
//   - APPROVED / ISSUED: never overwritten. The current revision is snapshotted
//     immutably, the order moves to revision+1, and a MATERIAL change (supplier,
//     currency, payment terms, price, quantity, UOM, lines added/removed) also
//     returns it to DRAFT for re-approval, clearing the approval. A non-material
//     change (descriptions, delivery details) keeps the status.
//
// An amendment may not reduce a line below what was already received or
// invoiced, nor remove a line that has progress.
func (s *PgStore) AmendOrder(ctx context.Context, tenantID, orderID, actor string, req domain.AmendOrderRequest) (*AmendResult, error) {
	var result *AmendResult
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := loadOrderDetail(ctx, tx, tenantID, orderID, true)
		if err != nil {
			return err
		}
		switch cur.Status {
		case domain.OrderStatusDraft, domain.OrderStatusPendingApproval, domain.OrderStatusApproved, domain.OrderStatusIssued:
		default:
			return domain.ErrInvalidTransition
		}
		if req.ExpectedVersion != nil && cur.Version != *req.ExpectedVersion {
			return domain.ErrStaleVersion
		}

		next, err := buildAmended(*cur, req)
		if err != nil {
			return err
		}
		if err := s.checkProgress(ctx, tx, tenantID, orderID, cur.Lines, next.Lines); err != nil {
			return err
		}
		material := domain.IsMaterialChange(*cur, next)
		if !material && !anyChange(*cur, next) {
			return ErrNoChange
		}

		now := time.Now().UTC()
		fromStatus := cur.Status
		newRevision := false
		reapproval := false

		switch cur.Status {
		case domain.OrderStatusDraft, domain.OrderStatusPendingApproval:
			// In place; a pending approval is withdrawn.
			if _, err := tx.Exec(ctx, `
				UPDATE purchase_orders SET po_status = 'DRAFT', total_amount = $3, currency_code = $4, supplier_ref = $5,
					delivery_terms = $6, payment_terms = $7, prepared_by_principal_id = $8,
					submitted_by_principal_id = NULL, submitted_at = NULL, version = version + 1
				WHERE purchase_order_id = $1 AND tenant_id = $2`,
				orderID, tenantID, next.TotalAmount, next.CurrencyCode, next.SupplierRef,
				next.DeliveryTerms, next.PaymentTerms, actor); err != nil {
				return err
			}
			reapproval = cur.Status == domain.OrderStatusPendingApproval

		default: // APPROVED, ISSUED: a new revision, never an overwrite
			snapshot, err := json.Marshal(cur)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO purchase_order_revisions (revision_id, tenant_id, purchase_order_id, revision, status_at_snapshot,
					snapshot, approved_by_principal_id, approved_at, reason, created_by_principal_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
				uuid.NewString(), tenantID, orderID, cur.Revision, string(cur.Status), snapshot,
				cur.ApprovedByPrincipalID, cur.ApprovedAt, req.Reason, actor); err != nil {
				return err
			}
			newRevision = true
			if material {
				// Back to DRAFT: the approval no longer covers this content.
				reapproval = true
				if _, err := tx.Exec(ctx, `
					UPDATE purchase_orders SET po_status = 'DRAFT', revision = revision + 1, total_amount = $3, currency_code = $4,
						supplier_ref = $5, delivery_terms = $6, payment_terms = $7, prepared_by_principal_id = $8,
						submitted_by_principal_id = NULL, submitted_at = NULL,
						approved_by_principal_id = NULL, approved_at = NULL, approval_basis = NULL, approval_ref = NULL,
						version = version + 1
					WHERE purchase_order_id = $1 AND tenant_id = $2`,
					orderID, tenantID, next.TotalAmount, next.CurrencyCode, next.SupplierRef,
					next.DeliveryTerms, next.PaymentTerms, actor); err != nil {
					return err
				}
			} else {
				if _, err := tx.Exec(ctx, `
					UPDATE purchase_orders SET revision = revision + 1, delivery_terms = $3, version = version + 1
					WHERE purchase_order_id = $1 AND tenant_id = $2`,
					orderID, tenantID, next.DeliveryTerms); err != nil {
					return err
				}
			}
		}

		if err := applyLines(ctx, tx, tenantID, orderID, cur.Lines, next.Lines); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO purchase_order_amendments (amendment_id, purchase_order_id, tenant_id, from_version, to_version,
				previous_total_amount, new_total_amount, reason, amended_by_principal_id, amended_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			uuid.NewString(), orderID, tenantID, cur.Version, cur.Version+1, cur.TotalAmount, next.TotalAmount,
			req.Reason, actor, now); err != nil {
			return err
		}

		updated, err := loadOrderDetail(ctx, tx, tenantID, orderID, false)
		if err != nil {
			return err
		}
		detail := req.Reason
		if material {
			detail += " [material change"
			if reapproval {
				detail += "; re-approval required"
			}
			detail += "]"
		}
		if err := s.record(ctx, tx, EventAmended, fromStatus, updated, actor, detail); err != nil {
			return err
		}
		result = &AmendResult{Order: *updated, Material: material, RequiresReapproval: reapproval, NewRevision: newRevision}
		return nil
	})
	if err != nil {
		return nil, mapTransitionErr(err)
	}
	return result, nil
}

// buildAmended returns the PO as it would be after req. Lines, when supplied,
// are the COMPLETE new set keyed by line_number; a line without a number is
// appended.
func buildAmended(cur domain.OrderDetail, req domain.AmendOrderRequest) (domain.OrderDetail, error) {
	next := cur
	next.Lines = append([]domain.Line(nil), cur.Lines...)
	if req.SupplierRef != nil {
		v := strings.TrimSpace(*req.SupplierRef)
		next.SupplierRef = &v
	}
	if req.CurrencyCode != nil {
		next.CurrencyCode = strings.ToUpper(strings.TrimSpace(*req.CurrencyCode))
	}
	if req.DeliveryTerms != nil {
		next.DeliveryTerms = *req.DeliveryTerms
	}
	if req.PaymentTerms != nil {
		next.PaymentTerms = *req.PaymentTerms
	}

	switch {
	case req.Lines != nil:
		maxNumber := 0
		for _, l := range cur.Lines {
			if l.LineNumber > maxNumber {
				maxNumber = l.LineNumber
			}
		}
		byNumber := map[int]domain.Line{}
		for _, l := range cur.Lines {
			byNumber[l.LineNumber] = l
		}
		lines := make([]domain.Line, 0, len(req.Lines))
		seen := map[int]bool{}
		var total float64
		for _, in := range req.Lines {
			n := in.LineNumber
			if n == 0 {
				maxNumber++
				n = maxNumber
			}
			if seen[n] {
				return next, fmt.Errorf("%w: line_number %d appears twice", domain.ErrInvalidLine, n)
			}
			seen[n] = true
			l := domain.Line{
				LineID: byNumber[n].LineID, LineNumber: n, ItemRef: in.ItemRef, Description: in.Description,
				Quantity: in.Quantity, UnitPrice: in.UnitPrice, UOM: in.UOM, LineAmount: in.Amount(),
				DeliveryDate: in.DeliveryDate, DeliveryLocation: in.DeliveryLocation,
			}
			total += l.LineAmount
			lines = append(lines, l)
		}
		if len(lines) == 0 {
			return next, fmt.Errorf("%w: an amendment cannot remove every line", domain.ErrInvalidLine)
		}
		next.Lines = lines
		next.TotalAmount = domain.RoundMoney(total)

	case len(cur.Lines) == 0 && req.NewTotalAmount > 0:
		// Legacy header-only order: the total is the commitment.
		next.TotalAmount = domain.RoundMoney(req.NewTotalAmount)

	case len(cur.Lines) > 0 && req.NewTotalAmount > 0:
		return next, fmt.Errorf("%w: new_total_amount cannot be set on an order with lines; amend the lines", domain.ErrInvalidLine)
	}
	return next, nil
}

// anyChange reports whether next differs from cur in any stored field, material
// or not.
func anyChange(cur, next domain.OrderDetail) bool {
	if derefS(cur.SupplierRef) != derefS(next.SupplierRef) || cur.CurrencyCode != next.CurrencyCode ||
		cur.DeliveryTerms != next.DeliveryTerms || cur.PaymentTerms != next.PaymentTerms ||
		cur.TotalAmount != next.TotalAmount || len(cur.Lines) != len(next.Lines) {
		return true
	}
	for i := range cur.Lines {
		a, b := cur.Lines[i], next.Lines[i]
		if a.LineNumber != b.LineNumber || a.ItemRef != b.ItemRef || a.Description != b.Description || a.Quantity != b.Quantity ||
			a.UnitPrice != b.UnitPrice || a.UOM != b.UOM || a.LineAmount != b.LineAmount ||
			a.DeliveryLocation != b.DeliveryLocation || !sameTime(a.DeliveryDate, b.DeliveryDate) {
			return true
		}
	}
	return false
}

func derefS(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// checkProgress refuses an amendment that would drop a line below what was
// already received or invoiced, or remove a line that has progress.
func (s *PgStore) checkProgress(ctx context.Context, tx pgx.Tx, tenantID, orderID string, cur, next []domain.Line) error {
	rows, err := tx.Query(ctx, `SELECT line_number, received_quantity, invoiced_quantity FROM purchase_order_lines
		WHERE purchase_order_id = $1 AND tenant_id = $2`, orderID, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type prog struct{ recv, inv float64 }
	progress := map[int]prog{}
	for rows.Next() {
		var n int
		var p prog
		if err := rows.Scan(&n, &p.recv, &p.inv); err != nil {
			return err
		}
		progress[n] = p
	}
	if err := rows.Err(); err != nil {
		return err
	}
	nextByNumber := map[int]domain.Line{}
	for _, l := range next {
		nextByNumber[l.LineNumber] = l
	}
	for n, p := range progress {
		if p.recv == 0 && p.inv == 0 {
			continue
		}
		l, kept := nextByNumber[n]
		if !kept || l.Quantity+1e-9 < p.recv || l.Quantity+1e-9 < p.inv {
			return domain.ErrAmendmentBelowProgress
		}
	}
	return nil
}

// applyLines makes the stored lines equal next: updates by line number,
// inserts new numbers, deletes the ones dropped. The line trigger allows the
// commercial columns to change only while the order is DRAFT, which is exactly
// the material-amendment path; a non-material amendment only touches
// descriptive columns, which are always editable.
func applyLines(ctx context.Context, tx pgx.Tx, tenantID, orderID string, cur, next []domain.Line) error {
	if len(next) == 0 && len(cur) == 0 {
		return nil
	}
	keep := map[int]bool{}
	existing := map[int]domain.Line{}
	for _, l := range cur {
		existing[l.LineNumber] = l
	}
	for _, l := range next {
		keep[l.LineNumber] = true
		if _, ok := existing[l.LineNumber]; ok {
			if _, err := tx.Exec(ctx, `
				UPDATE purchase_order_lines SET item_ref = $3, description = $4, quantity = $5, unit_price = $6, uom = $7,
					line_amount = $8, delivery_date = $9, delivery_location = $10, updated_at = NOW()
				WHERE purchase_order_id = $1 AND tenant_id = $2 AND line_number = $11`,
				orderID, tenantID, l.ItemRef, l.Description, l.Quantity, l.UnitPrice, l.UOM, l.LineAmount,
				l.DeliveryDate, l.DeliveryLocation, l.LineNumber); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO purchase_order_lines (line_id, tenant_id, purchase_order_id, line_number, item_ref, description,
				quantity, unit_price, uom, line_amount, delivery_date, delivery_location)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			uuid.NewString(), tenantID, orderID, l.LineNumber, l.ItemRef, l.Description, l.Quantity, l.UnitPrice,
			l.UOM, l.LineAmount, l.DeliveryDate, l.DeliveryLocation); err != nil {
			return err
		}
	}
	for n := range existing {
		if keep[n] {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM purchase_order_lines WHERE purchase_order_id = $1 AND tenant_id = $2 AND line_number = $3`,
			orderID, tenantID, n); err != nil {
			return err
		}
	}
	return nil
}
