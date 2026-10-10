// Package domain defines the authoritative domain types for purchase-order-svc
// — AP-03 of the Procurement, Expenses & Accounts Payable baseline
// (ZS-SVC-D-001 §6): "Create the authoritative supplier commercial commitment
// from approved procurement basis, with controlled amendments and no direct
// ledger bypass."
//
// # State model
//
//	DRAFT -> PENDING_APPROVAL -> APPROVED -> ISSUED -> CLOSED
//	  \            \                \          \
//	   +------------+----------------+----------+--> CANCELLED
//	ISSUED/APPROVED <-> ON_HOLD (returns to where it was held from)
//	APPROVED/ISSUED -> DRAFT is an AMENDMENT: it creates the next revision and
//	snapshots the superseded one immutably (purchase_order_revisions).
//
// PARTIALLY_RECEIVED / PARTIALLY_INVOICED / FULFILLED exist in the database
// CHECK constraint but are deliberately never stored: receipts and invoices are
// accepted only while po_status = ISSUED (a cross-service contract), so
// receipt/invoice progress is DERIVED from purchase_order_progress instead
// (GetReceiptStatus / GetInvoiceStatus) and the status stays ISSUED.
//
// # What is enforced where
//
// The same rules are enforced twice: here and in the handler for fast, typed
// errors, and in the migration's triggers (000007) because the runtime role is a
// Postgres superuser and triggers are the only enforcement that binds it. A PO
// cannot become ISSUED without approval evidence; the preparer cannot approve;
// an approved/issued PO's commercial terms cannot be edited in place; lines are
// editable only while DRAFT; revisions, progress and events are append-only.
package domain

import (
	"math"
	"strings"
	"time"
)

// OrderStatus is the PO lifecycle state.
type OrderStatus string

const (
	OrderStatusDraft           OrderStatus = "DRAFT"
	OrderStatusPendingApproval OrderStatus = "PENDING_APPROVAL"
	OrderStatusApproved        OrderStatus = "APPROVED"
	OrderStatusIssued          OrderStatus = "ISSUED"
	OrderStatusOnHold          OrderStatus = "ON_HOLD"
	OrderStatusCancelled       OrderStatus = "CANCELLED"
	OrderStatusClosed          OrderStatus = "CLOSED"
)

// allowedTransitions is the state machine. It mirrors po_guard_order() in
// migration 000007.
var allowedTransitions = map[OrderStatus][]OrderStatus{
	OrderStatusDraft:           {OrderStatusPendingApproval, OrderStatusCancelled},
	OrderStatusPendingApproval: {OrderStatusApproved, OrderStatusDraft, OrderStatusCancelled},
	OrderStatusApproved:        {OrderStatusIssued, OrderStatusDraft, OrderStatusOnHold, OrderStatusCancelled},
	OrderStatusIssued:          {OrderStatusOnHold, OrderStatusDraft, OrderStatusClosed, OrderStatusCancelled},
	OrderStatusOnHold:          {OrderStatusApproved, OrderStatusIssued, OrderStatusCancelled},
	OrderStatusClosed:          {},
	OrderStatusCancelled:       {},
}

// CanTransition reports whether the state machine allows from -> to.
func CanTransition(from, to OrderStatus) bool {
	for _, t := range allowedTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether no further change is possible.
func (s OrderStatus) IsTerminal() bool { return s == OrderStatusClosed || s == OrderStatusCancelled }

// ValidOrderStatus reports whether s is a status this service can ever have
// stored. An unrecognised ?status= filter matched no row and returned an empty
// list, so a typo was indistinguishable from a tenant with no orders.
func ValidOrderStatus(s string) bool {
	switch OrderStatus(s) {
	case OrderStatusDraft, OrderStatusPendingApproval, OrderStatusApproved, OrderStatusIssued,
		OrderStatusOnHold, OrderStatusCancelled, OrderStatusClosed:
		return true
	default:
		return false
	}
}

// Approval bases: where the approval that allows a PO to be ISSUED came from.
const (
	ApprovalBasisLegacy          = "LEGACY"           // pre-governance rows
	ApprovalBasisWorkflow        = "WORKFLOW"         // this service's own submit/approve flow
	ApprovalBasisPurchaseRequest = "PURCHASE_REQUEST" // an APPROVED requisition (verified at the source)
	ApprovalBasisProcurementCase = "PROCUREMENT_CASE" // an APPROVED procurement case (verified at the source)
)

// PurchaseOrder is one order. Entity-bound (LegalEntityID), never hard-deleted.
//
// The wire shape is the existing snake_case one with additions; nothing that
// consumers already read was renamed or removed. Lines are served separately
// (OrderDetail) because list responses do not need them.
type PurchaseOrder struct {
	PurchaseOrderID   string      `json:"purchase_order_id"`
	TenantID          string      `json:"tenant_id"`
	LegalEntityID     string      `json:"legal_entity_id"`
	PurchaseRequestID *string     `json:"purchase_request_id,omitempty"`
	VendorProfileID   *string     `json:"vendor_profile_id,omitempty"`
	SupplierRef       *string     `json:"supplier_ref,omitempty"`
	PONumber          string      `json:"po_number"`
	Status            OrderStatus `json:"po_status"`
	TotalAmount       float64     `json:"total_amount"`
	CurrencyCode      string      `json:"currency_code"`
	// Version increments on every change (optimistic concurrency); Revision
	// increments when an approved/issued PO is amended.
	Version  int `json:"version"`
	Revision int `json:"revision"`

	DeliveryTerms string `json:"delivery_terms,omitempty"`
	PaymentTerms  string `json:"payment_terms,omitempty"`

	PreparedByPrincipalID  string     `json:"prepared_by_principal_id,omitempty"`
	SubmittedByPrincipalID *string    `json:"submitted_by_principal_id,omitempty"`
	SubmittedAt            *time.Time `json:"submitted_at,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ApprovalBasis          string     `json:"approval_basis,omitempty"`
	ApprovalRef            string     `json:"approval_ref,omitempty"`

	HeldByPrincipalID  *string    `json:"held_by_principal_id,omitempty"`
	HeldAt             *time.Time `json:"held_at,omitempty"`
	HoldReason         string     `json:"hold_reason,omitempty"`
	HeldFromStatus     string     `json:"held_from_status,omitempty"`
	CancelledBy        *string    `json:"cancelled_by_principal_id,omitempty"`
	CancelledAt        *time.Time `json:"cancelled_at,omitempty"`
	CancellationReason string     `json:"cancellation_reason,omitempty"`

	SupplierExceptionRef string `json:"supplier_exception_ref,omitempty"`
	SupplierExceptionBy  string `json:"supplier_exception_by,omitempty"`

	IssuedByPrincipalID string     `json:"issued_by_principal_id,omitempty"`
	ClosedByPrincipalID *string    `json:"closed_by_principal_id,omitempty"`
	CorrelationID       string     `json:"correlation_id"`
	CreatedAt           time.Time  `json:"created_at"`
	IssuedAt            *time.Time `json:"issued_at,omitempty"`
	ClosedAt            *time.Time `json:"closed_at,omitempty"`
}

// Line is one PO line (cross-service contract: AP-04/AP-05/AP-06 read these).
type Line struct {
	LineID           string     `json:"line_id"`
	LineNumber       int        `json:"line_number"`
	ItemRef          string     `json:"item_ref"`
	Description      string     `json:"description"`
	Quantity         float64    `json:"quantity"`
	UnitPrice        float64    `json:"unit_price"`
	UOM              string     `json:"uom"`
	LineAmount       float64    `json:"line_amount"`
	DeliveryDate     *time.Time `json:"delivery_date,omitempty"`
	DeliveryLocation string     `json:"delivery_location,omitempty"`
}

// OrderDetail is GET /v1/purchase-orders/{id}: the order plus its lines. The
// `lines` key is always present (an empty array for a legacy header-only PO).
type OrderDetail struct {
	PurchaseOrder
	Lines []Line `json:"lines"`
}

// PurchaseOrderAmendment is an append-only record of a single amendment —
// never mutated or deleted, matching doctrine's "no soft-delete, no
// destructive overwrite of material history" rule.
type PurchaseOrderAmendment struct {
	AmendmentID          string    `json:"amendment_id"`
	PurchaseOrderID      string    `json:"purchase_order_id"`
	FromVersion          int       `json:"from_version"`
	ToVersion            int       `json:"to_version"`
	PreviousTotalAmount  float64   `json:"previous_total_amount"`
	NewTotalAmount       float64   `json:"new_total_amount"`
	Reason               string    `json:"reason"`
	AmendedByPrincipalID string    `json:"amended_by_principal_id"`
	AmendedAt            time.Time `json:"amended_at"`
}

// Revision is the immutable snapshot of a superseded revision.
type Revision struct {
	RevisionID            string      `json:"revision_id"`
	PurchaseOrderID       string      `json:"purchase_order_id"`
	Revision              int         `json:"revision"`
	StatusAtSnapshot      OrderStatus `json:"status_at_snapshot"`
	Snapshot              OrderDetail `json:"snapshot"`
	ApprovedByPrincipalID *string     `json:"approved_by_principal_id,omitempty"`
	ApprovedAt            *time.Time  `json:"approved_at,omitempty"`
	Reason                string      `json:"reason"`
	CreatedByPrincipalID  string      `json:"created_by_principal_id"`
	CreatedAt             time.Time   `json:"created_at"`
	SupersededByRevision  int         `json:"superseded_by_revision"`
}

// OrderEvent is one row of the append-only history.
type OrderEvent struct {
	EventID    string      `json:"event_id"`
	EventType  string      `json:"event_type"`
	FromStatus OrderStatus `json:"from_status,omitempty"`
	ToStatus   OrderStatus `json:"to_status,omitempty"`
	Revision   int         `json:"revision"`
	Version    int         `json:"version"`
	Detail     string      `json:"detail,omitempty"`
	ActorID    string      `json:"actor_principal_id"`
	CreatedAt  time.Time   `json:"created_at"`
}

// ── progress ────────────────────────────────────────────────────────────────

// Progress kinds.
const (
	ProgressReceived = "RECEIVED"
	ProgressInvoiced = "INVOICED"
)

// ProgressRequest is POST /v1/purchase-orders/{id}/lines/{line_id}/progress.
// It is idempotent on (tenant, source_ref, kind).
type ProgressRequest struct {
	Kind      string  `json:"kind"`
	Quantity  float64 `json:"quantity"`
	Amount    float64 `json:"amount"`
	SourceRef string  `json:"source_ref"`
	DeltaSign int     `json:"delta_sign"`
}

// ProgressResult is what a progress push answers.
type ProgressResult struct {
	LineID           string  `json:"line_id"`
	Kind             string  `json:"kind"`
	Replayed         bool    `json:"replayed"`
	OrderedQuantity  float64 `json:"ordered_quantity"`
	ReceivedQuantity float64 `json:"received_quantity"`
	InvoicedQuantity float64 `json:"invoiced_quantity"`
}

// LineProgress is one line of GET .../open-quantity (cross-service contract).
type LineProgress struct {
	LineID              string  `json:"line_id"`
	LineNumber          int     `json:"line_number"`
	OrderedQuantity     float64 `json:"ordered_quantity"`
	ReceivedQuantity    float64 `json:"received_quantity"`
	InvoicedQuantity    float64 `json:"invoiced_quantity"`
	OpenReceiptQuantity float64 `json:"open_receipt_quantity"`
	OpenInvoiceQuantity float64 `json:"open_invoice_quantity"`
}

// Fulfilment states derived from progress.
const (
	FulfilmentNotStarted = "NOT_STARTED"
	FulfilmentPartial    = "PARTIAL"
	FulfilmentComplete   = "COMPLETE"
)

// Fulfilment derives NOT_STARTED / PARTIAL / COMPLETE from totals.
func Fulfilment(ordered, done float64) string {
	switch {
	case done <= 0:
		return FulfilmentNotStarted
	case done+1e-9 >= ordered:
		return FulfilmentComplete
	default:
		return FulfilmentPartial
	}
}

// ── wire types ───────────────────────────────────────────────────────────────

// LineInput is one line in a create/amend request.
type LineInput struct {
	LineNumber       int        `json:"line_number,omitempty"`
	ItemRef          string     `json:"item_ref"`
	Description      string     `json:"description"`
	Quantity         float64    `json:"quantity"`
	UnitPrice        float64    `json:"unit_price"`
	UOM              string     `json:"uom"`
	LineAmount       float64    `json:"line_amount"`
	DeliveryDate     *time.Time `json:"delivery_date,omitempty"`
	DeliveryLocation string     `json:"delivery_location,omitempty"`
}

// Amount returns the line amount from quantity x unit price, rounded to cents.
func (l LineInput) Amount() float64 { return RoundMoney(l.Quantity * l.UnitPrice) }

// RoundMoney rounds to 2 decimals.
func RoundMoney(v float64) float64 { return math.Round(v*100) / 100 }

// CreateDraftRequest is POST /v1/purchase-orders/draft.
type CreateDraftRequest struct {
	TenantID             string      `json:"tenant_id,omitempty"`
	LegalEntityID        string      `json:"legal_entity_id"`
	PurchaseRequestID    *string     `json:"purchase_request_id,omitempty"`
	SupplierRef          string      `json:"supplier_ref"`
	CurrencyCode         string      `json:"currency_code"`
	CorrelationID        string      `json:"correlation_id"`
	DeliveryTerms        string      `json:"delivery_terms,omitempty"`
	PaymentTerms         string      `json:"payment_terms,omitempty"`
	SupplierExceptionRef string      `json:"supplier_exception_ref,omitempty"`
	Lines                []LineInput `json:"lines"`
}

// IssueOrderRequest is the legacy POST /v1/purchase-orders (direct issue). The
// order is created and issued in one governed step, only when its approval can
// be verified at the source (an APPROVED requisition or procurement case).
type IssueOrderRequest struct {
	TenantID             string      `json:"tenant_id"`
	LegalEntityID        string      `json:"legal_entity_id"`
	PurchaseRequestID    *string     `json:"purchase_request_id,omitempty"`
	VendorProfileID      *string     `json:"vendor_profile_id,omitempty"`
	SupplierRef          string      `json:"supplier_ref,omitempty"`
	TotalAmount          float64     `json:"total_amount"`
	CurrencyCode         string      `json:"currency_code"`
	CorrelationID        string      `json:"correlation_id"`
	SupplierExceptionRef string      `json:"supplier_exception_ref,omitempty"`
	Lines                []LineInput `json:"lines,omitempty"`
}

// AmendOrderRequest is POST /v1/purchase-orders/{id}/amend. The legacy body
// ({new_total_amount, reason}) still works for a header-only order; a PO with
// lines is amended by supplying the new `lines`.
type AmendOrderRequest struct {
	NewTotalAmount  float64     `json:"new_total_amount"`
	Reason          string      `json:"reason"`
	ExpectedVersion *int        `json:"expected_version,omitempty"`
	SupplierRef     *string     `json:"supplier_ref,omitempty"`
	CurrencyCode    *string     `json:"currency_code,omitempty"`
	DeliveryTerms   *string     `json:"delivery_terms,omitempty"`
	PaymentTerms    *string     `json:"payment_terms,omitempty"`
	Lines           []LineInput `json:"lines,omitempty"`
}

// CommandRequest is the optional body of the lifecycle commands.
type CommandRequest struct {
	ExpectedVersion      *int   `json:"expected_version,omitempty"`
	Reason               string `json:"reason,omitempty"`
	SupplierExceptionRef string `json:"supplier_exception_ref,omitempty"`
}

// ListOrdersFilter holds optional filters for querying purchase orders.
type ListOrdersFilter struct {
	// TenantID is the caller's VERIFIED scope, resolved from X-Tenant-Id by the
	// handler — never a value the request chose for itself.
	TenantID      string
	LegalEntityID string
	Status        string
}

// ── material change ──────────────────────────────────────────────────────────

// IsMaterialChange reports whether moving from the current PO to the proposed
// one changes a field the spec says needs RE-APPROVAL: supplier, currency,
// payment terms, the total, or any line's item, quantity, unit price or UOM
// (including a line being added or removed). Descriptions, delivery dates,
// delivery location and delivery terms are not material.
func IsMaterialChange(cur OrderDetail, next OrderDetail) bool {
	if !strings.EqualFold(derefStr(cur.SupplierRef), derefStr(next.SupplierRef)) {
		return true
	}
	if !strings.EqualFold(cur.CurrencyCode, next.CurrencyCode) {
		return true
	}
	if strings.TrimSpace(cur.PaymentTerms) != strings.TrimSpace(next.PaymentTerms) {
		return true
	}
	if math.Abs(cur.TotalAmount-next.TotalAmount) > 0.004 {
		return true
	}
	if len(cur.Lines) != len(next.Lines) {
		return true
	}
	byNumber := make(map[int]Line, len(cur.Lines))
	for _, l := range cur.Lines {
		byNumber[l.LineNumber] = l
	}
	for _, n := range next.Lines {
		c, ok := byNumber[n.LineNumber]
		if !ok {
			return true
		}
		if c.ItemRef != n.ItemRef || c.UOM != n.UOM ||
			math.Abs(c.Quantity-n.Quantity) > 1e-9 || math.Abs(c.UnitPrice-n.UnitPrice) > 1e-9 ||
			math.Abs(c.LineAmount-n.LineAmount) > 0.004 {
			return true
		}
	}
	return false
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrOrderNotFound     = errorString("purchase order not found")
	ErrInvalidTransition = errorString("invalid purchase order status transition")
	ErrStoreUnavailable  = errorString("purchase order store unavailable")

	// ErrInvalidIdentifier is a non-UUID value compared against a uuid column.
	// It dies inside the pg driver as SQLSTATE 22P02 before any row is
	// examined, and without this it reached the caller as a generic store
	// failure — a 503 store_unavailable, i.e. a typo wearing an outage's
	// clothes. A malformed id cannot name an existing order, so callers treat
	// this as absent rather than as the database being down.
	ErrInvalidIdentifier = errorString("identifier is not a valid UUID")

	ErrAuthorizationDenied             = errorString("authorization denied for this purchase order action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other Phase 3 service.
	ErrIdentityMissing = errorString("caller identity missing")

	// Purchase-request verification errors — fail closed on all of these,
	// same posture as bank-reconciliation-svc's/accounts-receivable-svc's
	// general-ledger-svc verification calls (never trust a caller-supplied
	// purchase_request_id without checking it against the real record).
	ErrPurchaseRequestNotFound           = errorString("referenced purchase request not found")
	ErrPurchaseRequestNotApproved        = errorString("referenced purchase request is not APPROVED")
	ErrPurchaseRequestMismatch           = errorString("referenced purchase request belongs to a different tenant or legal entity")
	ErrPurchaseRequestServiceUnavailable = errorString("purchase-request-svc unavailable")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope (no X-Tenant-Id). Every order is tenant-owned, and a read
	// with no scope has no honest answer — it must not quietly become
	// "whatever tenant the caller named", which is what ListOrders did.
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ErrTenantScopeMismatch is returned when a request names a tenant other
	// than the caller's verified scope — as ?tenant_id= when listing the
	// register, or as tenant_id in an issue body.
	ErrTenantScopeMismatch = errorString("request tenant_id does not match the caller's verified tenant scope")

	// ── AP-03 governance errors ──────────────────────────────────────────────

	// ErrStaleVersion: the caller acted on a version of the order that has since changed.
	ErrStaleVersion = errorString("the purchase order changed since the version the caller acted on")
	// ErrSoDConflict: maker-checker — the preparer (or submitter) cannot approve.
	ErrSoDConflict = errorString("the preparer or submitter of a purchase order cannot approve it")
	// ErrApprovalRequired: no verifiable approval basis exists for issuing.
	ErrApprovalRequired = errorString("a purchase order cannot be issued without a verifiable approval")
	// ErrNoLines: a governed PO needs at least one line before it can be submitted.
	ErrNoLines = errorString("a purchase order needs at least one line (or a positive total on a legacy header-only order) before it can be submitted")
	// ErrHasProgress: receipts or invoices already exist against the order.
	ErrHasProgress = errorString("receipts or invoices already exist against this purchase order")
	// ErrAmendmentBelowProgress: an amendment would drop a line below what was received or invoiced.
	ErrAmendmentBelowProgress = errorString("an amendment cannot reduce a line below the quantity already received or invoiced, nor remove a line that has progress")
	// ErrOrderNotIssued: receipts/invoices are accepted only while the order is ISSUED.
	ErrOrderNotIssued = errorString("the purchase order is not ISSUED")
	// ErrProgressExceedsOrder: the push would take received/invoiced quantity beyond what was ordered.
	ErrProgressExceedsOrder = errorString("progress would exceed the ordered quantity")
	// ErrProgressBelowZero: a reversal larger than what was recorded.
	ErrProgressBelowZero = errorString("progress reversal would take the quantity below zero")
	// ErrProgressRefReused: the source_ref was already recorded for different content.
	ErrProgressRefReused = errorString("this source_ref was already recorded with different content")
	// ErrLineNotFound: the line does not belong to this purchase order.
	ErrLineNotFound = errorString("purchase order line not found")
	// ErrInvalidLine: a line failed validation.
	ErrInvalidLine = errorString("invalid purchase order line")

	// Supplier eligibility (AP-01).
	ErrSupplierNotEligible        = errorString("the supplier is not eligible for new purchase orders (inactive, suspended or on hold)")
	ErrSupplierServiceUnavailable = errorString("supplier-financial-profile-svc unavailable")
	ErrSupplierUnknown            = errorString("no supplier financial profile exists for this supplier")

	// Procurement-case approval basis.
	ErrProcurementCaseNotApproved        = errorString("referenced procurement case is not approved")
	ErrProcurementCaseMismatch           = errorString("referenced procurement case does not match this order")
	ErrProcurementCaseServiceUnavailable = errorString("procurement-workflow-svc unavailable")
)

// IdempotencyRecord is the stored state of an Idempotency-Key.
type IdempotencyRecord struct {
	RequestHash string
	Completed   bool
	StatusCode  int
	Body        []byte
}
