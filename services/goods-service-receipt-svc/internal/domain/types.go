// Package domain defines the authoritative domain types for
// goods-service-receipt-svc — AP-04 of the Procurement, Expenses & Accounts
// Payable baseline. Its job: record independent evidence that ordered goods,
// services or milestones were actually received/accepted, providing the
// authoritative receipt basis for matching (AP-06) and GRNI accounting
// events. It sits between purchase-order-svc (AP-03) and the invoice/matching
// side of accounts-payable-svc (AP-05/06).
//
// How the service relates to its neighbours:
//
//  1. PO lines and quantities (AP-03). A receipt may be line-level: it names a
//     po_line_id of the PO and a quantity. At create and again at confirm the
//     PO must be ISSUED (anything else — draft, held, cancelled, closed — is
//     refused PO_NOT_OPEN), the line must belong to the PO, and the quantity
//     must fit the line's open_receipt_quantity (as reported by AP-03, less
//     this service's own confirmed-but-not-yet-delivered pushes) plus the
//     configured OVER_RECEIPT_TOLERANCE_PCT. Past that ceiling only a
//     caller-supplied tolerance_exception_ref plus the authz-svc action
//     GOODS_SERVICE_RECEIPT_TOLERANCE_OVERRIDE lets it through. Legacy
//     header-only receipts (no po_line_id) are still accepted and are checked
//     against the PO's total_amount, net of confirmed receipts.
//
//     Received quantity is owned by AP-03: every confirmation and reversal of
//     a line receipt writes a po_progress_pushes row in the same transaction,
//     and a background worker delivers it to AP-03's progress endpoint
//     (idempotent on source_ref). A transient failure is retried with backoff
//     and never lost; a PROGRESS_EXCEEDS_ORDER refusal marks the push FAILED,
//     visible as progress_push_status on the receipt.
//
//  2. GRNI accounting is never written to the ledger as a raw journal from
//     here (invariant #20). Confirming a receipt inserts, in the same
//     transaction, an accounting_posting_requests row (unique per tenant on
//     source_event_id = receipt id; a reversal uses receipt id + ":reversal:" +
//     reversal id, so a replay can never queue the consequence twice). A
//     dispatcher posts it through general-ledger-svc's ACC-04 endpoint
//     (POST /v1/postings/events — idempotent on source_event_id, period-gated),
//     retrying transient failures and surfacing refusals as FAILED/QUARANTINED.
//     GetReceiptAccountingStatus reports the true state of that row; the
//     legacy POSTED/EXCEPTION rows of the retired direct-journal path stay
//     readable as history.
//
// Confirmed receipts are never deleted or overwritten (DB triggers); the only
// correction is a linked reversal. All state changes carry a version
// (expected_version → STALE_VERSION) and emit their domain events through the
// transactional outbox.
package domain

import (
	"time"
)

// ReceiptType distinguishes a goods delivery from a service milestone —
// RecordServiceAcceptance only applies to the latter.
type ReceiptType string

const (
	ReceiptTypeGoods   ReceiptType = "GOODS"
	ReceiptTypeService ReceiptType = "SERVICE"
)

func ValidReceiptType(t ReceiptType) bool {
	return t == ReceiptTypeGoods || t == ReceiptTypeService
}

// ReceiptStatus follows the spec's own state model: Draft ->
// PendingConfirmation -> Confirmed -> PartiallyReversed/FullyReversed, with
// a Rejected branch. The spec names no explicit command for Draft ->
// PendingConfirmation; this service reaches PendingConfirmation when
// RecordServiceAcceptance is recorded against a Draft receipt (acceptance
// recorded = ready to confirm), and allows ConfirmReceipt directly from Draft
// for a goods receipt that needed no separate acceptance step.
type ReceiptStatus string

const (
	StatusDraft               ReceiptStatus = "DRAFT"
	StatusPendingConfirmation ReceiptStatus = "PENDING_CONFIRMATION"
	StatusConfirmed           ReceiptStatus = "CONFIRMED"
	StatusRejected            ReceiptStatus = "REJECTED"
	StatusPartiallyReversed   ReceiptStatus = "PARTIALLY_REVERSED"
	StatusFullyReversed       ReceiptStatus = "FULLY_REVERSED"
)

var confirmableFrom = map[ReceiptStatus]bool{
	StatusDraft:               true,
	StatusPendingConfirmation: true,
}

// CanConfirm reports whether a receipt in status s may be confirmed.
func CanConfirm(s ReceiptStatus) bool { return confirmableFrom[s] }

var rejectableFrom = map[ReceiptStatus]bool{
	StatusDraft:               true,
	StatusPendingConfirmation: true,
}

// CanReject reports whether a receipt in status s may be rejected.
func CanReject(s ReceiptStatus) bool { return rejectableFrom[s] }

var reversibleFrom = map[ReceiptStatus]bool{
	StatusConfirmed:         true,
	StatusPartiallyReversed: true,
}

// CanReverse reports whether a receipt in status s may accept a further
// reversal.
func CanReverse(s ReceiptStatus) bool { return reversibleFrom[s] }

// CanAmendDraft reports whether a receipt in status s may still be amended
// as a draft.
func CanAmendDraft(s ReceiptStatus) bool { return s == StatusDraft }

// Progress push statuses as reported on a receipt (progress_push_status).
const (
	PushNotApplicable = "NOT_APPLICABLE"
	PushPending       = "PENDING"
	PushDelivered     = "DELIVERED"
	PushFailed        = "FAILED"
)

// GoodsServiceReceipt is the authoritative receipt record.
type GoodsServiceReceipt struct {
	ReceiptID       string  `json:"receipt_id"`
	TenantID        string  `json:"tenant_id"`
	LegalEntityID   string  `json:"legal_entity_id"`
	PurchaseOrderID string  `json:"purchase_order_id"`
	POLineID        *string `json:"po_line_id"`
	// PORevision is the PO revision observed when the receipt was confirmed
	// (evidence/lineage); nil for a draft or a legacy receipt.
	PORevision                    *int        `json:"po_revision"`
	ReceiptType                   ReceiptType `json:"receipt_type"`
	Quantity                      float64     `json:"quantity"`
	UnitOfMeasure                 string      `json:"unit_of_measure"`
	Amount                        float64     `json:"amount"`
	CurrencyCode                  string      `json:"currency_code"`
	ReceiptDate                   time.Time   `json:"receipt_date"`
	Location                      string      `json:"location"`
	InspectionResult              string      `json:"inspection_result"`
	RequiresIndependentAcceptance bool        `json:"requires_independent_acceptance"`
	ToleranceExceptionRef         string      `json:"tolerance_exception_ref"`

	Status           ReceiptStatus `json:"status"`
	RejectionReason  string        `json:"rejection_reason"`
	ReversedAmount   float64       `json:"reversed_amount"`
	ReversedQuantity float64       `json:"reversed_quantity"`

	ReceiverPrincipalID    string     `json:"receiver_principal_id"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	ConfirmedByPrincipalID *string    `json:"confirmed_by_principal_id"`
	ConfirmedAt            *time.Time `json:"confirmed_at"`

	// Version is the optimistic-concurrency counter (expected_version).
	Version int `json:"version"`
	// ProgressPushStatus is the delivery state of this receipt's received-
	// quantity pushes to AP-03: PENDING, DELIVERED, FAILED or NOT_APPLICABLE
	// (header-level or not yet confirmed).
	ProgressPushStatus string `json:"progress_push_status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReceiptEvidence is an append-only evidence attachment against a receipt.
type ReceiptEvidence struct {
	EvidenceID            string    `json:"evidence_id"`
	TenantID              string    `json:"tenant_id"`
	ReceiptID             string    `json:"receipt_id"`
	EvidenceRef           string    `json:"evidence_ref"`
	Description           string    `json:"description"`
	RecordedByPrincipalID string    `json:"recorded_by_principal_id"`
	CreatedAt             time.Time `json:"created_at"`
}

// ReceiptReversal is an append-only record of one reversal against a
// confirmed receipt. Multiple partial reversals may accumulate up to (but
// never past) the original Amount and Quantity.
type ReceiptReversal struct {
	ReversalID            string    `json:"reversal_id"`
	TenantID              string    `json:"tenant_id"`
	ReceiptID             string    `json:"receipt_id"`
	ReversedAmount        float64   `json:"reversed_amount"`
	ReversedQuantity      float64   `json:"reversed_quantity"`
	Reason                string    `json:"reason"`
	ReversedByPrincipalID string    `json:"reversed_by_principal_id"`
	CreatedAt             time.Time `json:"created_at"`
}

// AccountingEventStatus is the state of one accounting consequence of a
// receipt. PENDING, POSTED, FAILED and QUARANTINED are the lifecycle of an
// accounting_posting_requests row; EXCEPTION is only ever seen on historical
// rows written by the retired direct-GL path.
type AccountingEventStatus string

const (
	AccountingPending     AccountingEventStatus = "PENDING"
	AccountingPosted      AccountingEventStatus = "POSTED"
	AccountingFailed      AccountingEventStatus = "FAILED"
	AccountingQuarantined AccountingEventStatus = "QUARANTINED"
	AccountingException   AccountingEventStatus = "EXCEPTION"
)

// Directions of a GRNI posting request.
const (
	DirectionAccrue  = "ACCRUE"
	DirectionReverse = "REVERSE"
)

// ReceiptAccountingEvent is one accounting consequence of a receipt —
// GetReceiptAccountingStatus reports the most recent one.
type ReceiptAccountingEvent struct {
	EventID              string                `json:"event_id"`
	TenantID             string                `json:"tenant_id"`
	ReceiptID            string                `json:"receipt_id"`
	SourceEventID        *string               `json:"source_event_id"`
	Direction            string                `json:"direction,omitempty"`
	Status               AccountingEventStatus `json:"status"`
	PostingExecutionID   *string               `json:"posting_execution_id"`
	JournalID            *string               `json:"journal_id"`
	PostingPolicyVersion string                `json:"posting_policy_version,omitempty"`
	Attempts             int                   `json:"attempts"`
	FailureReason        string                `json:"failure_reason"`
	CreatedAt            time.Time             `json:"created_at"`
}

// Command carries the per-request context every state-changing store method
// needs: who acts, the correlation to stamp on emitted events, and the
// caller's optimistic-concurrency expectation.
type Command struct {
	PrincipalID     string
	CorrelationID   string
	ExpectedVersion *int
}

// ConfirmInput is what ConfirmReceipt may add to the receipt as it moves to
// CONFIRMED (all part of the same UPDATE).
type ConfirmInput struct {
	// POLineID, when set and the receipt has none yet, links it to a PO line.
	POLineID *string
	// PORevision is the PO revision observed during the confirm-time check.
	PORevision *int
	// ToleranceExceptionRef, when non-empty, replaces the stored reference.
	ToleranceExceptionRef string
	// Limits are what the store needs to re-check the tolerance ceiling inside
	// the confirmation transaction, under a lock that serializes concurrent
	// confirmations against the same PO line / PO (the handler's earlier check is
	// advisory; this one is authoritative).
	Limits ConfirmLimits
}

// ConfirmLimits carries purchase-order-svc's figures into the store's
// authoritative tolerance check. TolerancePct is a percentage (5 = 5%), the same
// unit as purchase-order-svc's PO_OVER_TOLERANCE_PERCENT.
type ConfirmLimits struct {
	POTotalAmount           float64
	TolerancePct            float64
	LineOrderedQuantity     float64
	LineOpenReceiptQuantity float64
}

// ProgressPush is one received-quantity delta queued for delivery to AP-03.
type ProgressPush struct {
	PushID          string
	TenantID        string
	LegalEntityID   string
	ReceiptID       string
	PurchaseOrderID string
	POLineID        string
	Quantity        float64
	Amount          float64
	DeltaSign       int
	SourceRef       string
	CorrelationID   string
	Attempts        int
}

// ConfirmResult is a confirmed receipt together with the GRNI posting request
// its confirmation queued (nil when a replay queued nothing new).
type ConfirmResult struct {
	Receipt    *GoodsServiceReceipt
	Accounting *ReceiptAccountingEvent
}

// LineReceived is the net confirmed receipt of one PO line (received-to-date).
type LineReceived struct {
	POLineID         string  `json:"po_line_id"`
	ReceivedQuantity float64 `json:"received_quantity"`
	ReceivedAmount   float64 `json:"received_amount"`
}

// ── request DTOs ────────────────────────────────────────────────────────────

type CreateReceiptRequest struct {
	LegalEntityID                 string      `json:"legal_entity_id"`
	PurchaseOrderID               string      `json:"purchase_order_id"`
	POLineID                      string      `json:"po_line_id,omitempty"`
	ReceiptType                   ReceiptType `json:"receipt_type"`
	Quantity                      float64     `json:"quantity"`
	UnitOfMeasure                 string      `json:"unit_of_measure"`
	Amount                        float64     `json:"amount"`
	CurrencyCode                  string      `json:"currency_code"`
	ReceiptDate                   time.Time   `json:"receipt_date"`
	Location                      string      `json:"location"`
	InspectionResult              string      `json:"inspection_result"`
	RequiresIndependentAcceptance bool        `json:"requires_independent_acceptance"`
	ToleranceExceptionRef         string      `json:"tolerance_exception_ref"`
}

type AmendReceiptDraftRequest struct {
	Quantity         *float64 `json:"quantity"`
	UnitOfMeasure    *string  `json:"unit_of_measure"`
	Amount           *float64 `json:"amount"`
	Location         *string  `json:"location"`
	InspectionResult *string  `json:"inspection_result"`
	Reason           string   `json:"reason"`
	ExpectedVersion  *int     `json:"expected_version,omitempty"`
}

// ConfirmReceiptRequest is the optional body of POST .../confirm. expected_version
// is required (also accepted as the X-Expected-Version header).
type ConfirmReceiptRequest struct {
	ExpectedVersion       *int   `json:"expected_version,omitempty"`
	POLineID              string `json:"po_line_id,omitempty"`
	ToleranceExceptionRef string `json:"tolerance_exception_ref,omitempty"`
}

type RejectReceiptRequest struct {
	Reason          string `json:"reason"`
	ExpectedVersion *int   `json:"expected_version,omitempty"`
}

type ReverseReceiptRequest struct {
	ReversedAmount float64 `json:"reversed_amount"`
	// ReversedQuantity defaults to the quantity proportional to ReversedAmount.
	ReversedQuantity *float64 `json:"reversed_quantity,omitempty"`
	Reason           string   `json:"reason"`
	ExpectedVersion  *int     `json:"expected_version,omitempty"`
}

type RecordServiceAcceptanceRequest struct {
	EvidenceRef     string `json:"evidence_ref"`
	Notes           string `json:"notes"`
	ExpectedVersion *int   `json:"expected_version,omitempty"`
}

type AttachReceiptEvidenceRequest struct {
	EvidenceRef string `json:"evidence_ref"`
	Description string `json:"description"`
}

// ── event type names — the spec's own "Events produced" list, each emitted
// together with the platform's pre-existing SCREAMING_SNAKE alias that other
// consumers may already read (ServiceAcceptanceRecorded had no distinct alias,
// so it is emitted once). There is no named event for RejectReceipt in the
// spec; that transition is recorded in the receipt's own audit trail but
// deliberately not published as a platform event. ───────────────────────────

const (
	EventReceiptCreated            = "GoodsServiceReceiptCreated"
	EventReceiptConfirmed          = "GoodsServiceReceiptConfirmed"
	EventReceiptReversed           = "GoodsServiceReceiptReversed"
	EventServiceAcceptanceRecorded = "ServiceAcceptanceRecorded"

	AliasReceiptCreated   = "GOODS_SERVICE_RECEIPT_CREATED"
	AliasReceiptConfirmed = "GOODS_SERVICE_RECEIPT_CONFIRMED"
	AliasReceiptReversed  = "GOODS_SERVICE_RECEIPT_REVERSED"
)

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrReceiptNotFound                 = sentinel("receipt not found")
	ErrInvalidTransition               = sentinel("invalid receipt state transition")
	ErrStaleVersion                    = sentinel("receipt version does not match expected_version")
	ErrTenantScopeMissing              = sentinel("tenant scope missing")
	ErrPurchaseOrderNotFound           = sentinel("purchase order not found")
	ErrPurchaseOrderMismatch           = sentinel("purchase order does not belong to caller's tenant/legal entity")
	ErrPurchaseOrderNotOpen            = sentinel("purchase order is not open (must be ISSUED)")
	ErrPurchaseOrderLineInvalid        = sentinel("po_line_id is not a line of the purchase order")
	ErrPurchaseOrderServiceUnavailable = sentinel("purchase-order-svc unavailable")
	ErrProgressExceedsOrder            = sentinel("purchase-order-svc refused the progress push: PROGRESS_EXCEEDS_ORDER")
	ErrOverReceiptTolerance            = sentinel("receipt exceeds purchase order tolerance without an approved exception")
	ErrOverReversal                    = sentinel("reversal exceeds remaining unreversed receipt amount or quantity")
	ErrStoreUnavailable                = sentinel("store unavailable")
	ErrCurrencyMismatch                = sentinel("receipt currency does not match the purchase order currency")
	ErrIdentifierInvalid               = sentinel("identifier is not a valid UUID")
)

// PurchaseOrderNotOpenError is ErrPurchaseOrderNotOpen carrying the status AP-03
// reported, so the refusal can name it.
type PurchaseOrderNotOpenError struct{ Status string }

func (e *PurchaseOrderNotOpenError) Error() string {
	return "purchase order is not open (status " + e.Status + ", must be ISSUED)"
}

func (e *PurchaseOrderNotOpenError) Is(target error) bool { return target == ErrPurchaseOrderNotOpen }
