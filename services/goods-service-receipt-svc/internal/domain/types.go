// Package domain defines the authoritative domain types for
// goods-service-receipt-svc — AP-04 of the Procurement, Expenses & Accounts
// Payable baseline. Its job: record independent evidence that ordered goods,
// services or milestones were actually received/accepted, providing the
// authoritative receipt basis for matching and optional GRNI accounting
// events. It is the missing link between purchase-order-svc (AP-03, already
// built) and invoice-approval-svc (AP-05/06, already built) in a three-way
// match.
//
// Two scope decisions were made deliberately narrower than the spec's own
// contract, both because the upstream data the full contract assumes does
// not exist in this codebase — not fabricated to look complete:
//
//  1. PO-line / quantity matching. purchase-order-svc (AP-03) has no
//     line-level model at all: domain.PurchaseOrder is header-only
//     (total_amount, po_status), with no ordered quantity, no PO lines, and
//     only two statuses (ISSUED, CLOSED — no CANCELLED). AP-04's own spec
//     names "PO open quantity/amount" as server-resolved context and lists
//     "receipt exceeds PO tolerance" and "receipt confirmed against
//     cancelled PO" as required negative-path scenarios. Since no PO-line or
//     received-quantity data exists upstream, this service enforces the
//     nearest real equivalent: an aggregate amount check against the PO's
//     own total_amount (net of this service's own confirmed-and-not-yet-
//     reversed receipts), and treats purchase-order-svc's real CLOSED status
//     as the blocking state (there being no CANCELLED status to check
//     against). A caller-supplied ToleranceExceptionRef is required to
//     confirm past that ceiling — the exception approval itself is out of
//     this service's authority, same "opaque caller-supplied reference"
//     doctrine as PayeeReference (AP-01) and lawful_basis_refs (PRV-01).
//
//  2. GRNI accounting. general-ledger-svc (a real service) exposes a real,
//     usable journal endpoint with genuine idempotency (a repeat POST with
//     the same correlation_id returns the original journal, not a
//     duplicate) — this service uses ReceiptID as that correlation_id, so a
//     replayed ConfirmReceipt can never double-post a GRNI entry. No
//     chart-of-accounts authority exists anywhere in this codebase, so the
//     GRNI debit/credit account codes are operator-configured
//     (GRNI_DEBIT_ACCOUNT_CODE / GRNI_CREDIT_ACCOUNT_CODE), not resolved
//     dynamically. Posting is genuinely attempted, but is best-effort and
//     never blocks or reverses the receipt: any failure (GL unreachable,
//     fiscal period locked, GL-side rejection) is recorded as an EXCEPTION
//     accounting event, matching the spec's own stated failure semantics
//     verbatim ("accounting event failure leaves visible pending/exception
//     state, never silent receipt deletion"). There is no background retry
//     scheduler in this codebase, so an EXCEPTION event requires manual
//     operational follow-up — an honest gap, not a fabricated auto-retry.
package domain

import "time"

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
// AttachReceiptEvidence or RecordServiceAcceptance is recorded against a
// Draft receipt (evidence attached = ready to confirm), and allows
// ConfirmReceipt directly from Draft for a goods receipt that needed no
// separate acceptance step.
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

// GoodsServiceReceipt is the authoritative receipt record.
type GoodsServiceReceipt struct {
	ReceiptID                     string
	TenantID                      *string
	LegalEntityID                 string
	PurchaseOrderID               string
	ReceiptType                   ReceiptType
	Quantity                      float64
	UnitOfMeasure                 string
	Amount                        float64
	CurrencyCode                  string
	ReceiptDate                   time.Time
	Location                      string
	InspectionResult              string
	RequiresIndependentAcceptance bool
	ToleranceExceptionRef         string

	Status          ReceiptStatus
	RejectionReason string
	ReversedAmount  float64

	ReceiverPrincipalID    string
	CreatedByPrincipalID   string
	ConfirmedByPrincipalID *string
	ConfirmedAt            *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReceiptEvidence is an append-only evidence attachment against a receipt.
type ReceiptEvidence struct {
	EvidenceID            string
	TenantID              *string
	ReceiptID             string
	EvidenceRef           string
	Description           string
	RecordedByPrincipalID string
	CreatedAt             time.Time
}

// ReceiptReversal is an append-only record of one reversal against a
// confirmed receipt. Multiple partial reversals may accumulate up to (but
// never past) the original Amount.
type ReceiptReversal struct {
	ReversalID            string
	TenantID              *string
	ReceiptID             string
	ReversedAmount        float64
	Reason                string
	ReversedByPrincipalID string
	CreatedAt             time.Time
}

// AccountingEventStatus is the outcome of one attempt to post a GRNI journal
// entry to general-ledger-svc for a confirmed receipt.
type AccountingEventStatus string

const (
	AccountingPosted    AccountingEventStatus = "POSTED"
	AccountingException AccountingEventStatus = "EXCEPTION"
)

// ReceiptAccountingEvent is an append-only log of GRNI posting attempts —
// GetReceiptAccountingStatus reports the most recent one.
type ReceiptAccountingEvent struct {
	EventID       string
	TenantID      *string
	ReceiptID     string
	Status        AccountingEventStatus
	JournalID     *string
	FailureReason string
	CreatedAt     time.Time
}

// ── request DTOs ────────────────────────────────────────────────────────────

type CreateReceiptRequest struct {
	LegalEntityID                 string
	PurchaseOrderID               string
	ReceiptType                   ReceiptType
	Quantity                      float64
	UnitOfMeasure                 string
	Amount                        float64
	CurrencyCode                  string
	ReceiptDate                   time.Time
	Location                      string
	InspectionResult              string
	RequiresIndependentAcceptance bool
	ToleranceExceptionRef         string
}

type AmendReceiptDraftRequest struct {
	Quantity         *float64
	UnitOfMeasure    *string
	Amount           *float64
	Location         *string
	InspectionResult *string
	Reason           string
}

type RejectReceiptRequest struct {
	Reason string
}

type ReverseReceiptRequest struct {
	ReversedAmount float64
	Reason         string
}

type RecordServiceAcceptanceRequest struct {
	EvidenceRef string
	Notes       string
}

type AttachReceiptEvidenceRequest struct {
	EvidenceRef string
	Description string
}

// ── event type names — literally the spec's own "Events produced" list,
// nothing added beyond it (there is no named event for RejectReceipt in the
// spec; that transition is recorded in the receipt's own audit trail but
// deliberately not published as a platform event). ──────────────────────────

const (
	EventReceiptCreated            = "GOODS_SERVICE_RECEIPT_CREATED"
	EventReceiptConfirmed          = "GOODS_SERVICE_RECEIPT_CONFIRMED"
	EventReceiptReversed           = "GOODS_SERVICE_RECEIPT_REVERSED"
	EventServiceAcceptanceRecorded = "SERVICE_ACCEPTANCE_RECORDED"
)

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrReceiptNotFound                 = sentinel("receipt not found")
	ErrInvalidTransition               = sentinel("invalid receipt state transition")
	ErrPurchaseOrderNotFound           = sentinel("purchase order not found")
	ErrPurchaseOrderMismatch           = sentinel("purchase order does not belong to caller's tenant/legal entity")
	ErrPurchaseOrderNotOpen            = sentinel("purchase order is not open (closed)")
	ErrPurchaseOrderServiceUnavailable = sentinel("purchase-order-svc unavailable")
	ErrOverReceiptTolerance            = sentinel("receipt amount exceeds purchase order tolerance without an approved exception")
	ErrOverReversal                    = sentinel("reversal amount exceeds remaining unreversed receipt amount")
	ErrStoreUnavailable                = sentinel("store unavailable")
)
