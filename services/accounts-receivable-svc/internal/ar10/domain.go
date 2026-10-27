// Package ar10 is AR-10 Customer Refund / Write-Off (spec ZS-SVC-C-001 section 13): two separate authority paths.
//
// Refund (external money) pays back the UNAPPLIED cash (overpayment) of a registered AR-08 receipt. The cash is earmarked through
// the AR-08 hold port at request time (ar08.Module.ReserveUnapplied) so it cannot be applied to invoices or refunded twice, and is
// consumed only when payment-status-svc reports the payment SETTLED. AR never moves money and never decides settlement:
// payment-initiation-adapter-svc (BNK-06) holds the durable external attempt, payment-status-svc (BNK-07) owns finality.
//
//	PENDING_APPROVAL -> APPROVED -> SUBMITTED -> PENDING | UNKNOWN -> SETTLED | FAILED        CANCELLED only before SUBMITTED
//
// (DRAFT is the transient creation state, recorded in history; CreateRefundRequest lands in PENDING_APPROVAL.) The approval is
// bound to a hash of source + amount + destination (+ its verification fingerprint) + payer account: any change, or a destination
// that no longer verifies, invalidates the approval (REFUND_DESTINATION_CHANGED). A provider timeout or any ambiguous answer is
// UNKNOWN (REFUND_STATE_UNKNOWN), never failed, and nothing is resubmitted until reconciled against payment-status-svc.
//
// Write-off (receivable derecognition) is internal accounting: PENDING_APPROVAL -> APPROVED -> COMMITTED, with a controlled reason,
// evidence, delegated-authority tiers (AuthorityPolicy), an explicit tax treatment (a REVIEW_REQUIRED treatment blocks commit) and
// segregation of duties. Commit adjusts the AR-07 residual and queues the ACC-04 posting in one transaction.
package ar10

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Aggregate types (outbox / audit).
const (
	AggregateRefund   = "ar10_refund"
	AggregateWriteOff = "ar10_writeoff"
)

// Authz actions (authorization-svc action types). Reads are tenant-scoped (the settled AR read posture).
const (
	ActionRefundCreate      = "AR10_REFUND_CREATE"       // create / change destination / cancel an unsubmitted refund
	ActionRefundApprove     = "AR10_REFUND_APPROVE"      // approve a refund (a different principal)
	ActionRefundSubmit      = "AR10_REFUND_SUBMIT"       // release the approved refund for payment (a different principal from the requester)
	ActionRefundReconcile   = "AR10_REFUND_RECONCILE"    // reconcile the refund state with payment-status-svc
	ActionWriteOffCreate    = "AR10_WRITEOFF_CREATE"     // request a write-off
	ActionWriteOffTaxReview = "AR10_WRITEOFF_TAX_REVIEW" // record the tax treatment decision of a write-off
	ActionWriteOffApproveT1 = "AR10_WRITEOFF_APPROVE_T1" // delegated approval authority tiers (see DefaultTiers)
	ActionWriteOffApproveT2 = "AR10_WRITEOFF_APPROVE_T2"
	ActionWriteOffApproveT3 = "AR10_WRITEOFF_APPROVE_T3"
	ActionWriteOffApproveT4 = "AR10_WRITEOFF_APPROVE_T4"
	ActionWriteOffCommit    = "AR10_WRITEOFF_COMMIT" // commit an approved write-off to the receivable
)

// Events (ar.refund.<verb>, ar.write_off.<verb>).
const (
	EvRefundRequested    = "ar.refund.requested"
	EvRefundApproved     = "ar.refund.approved"
	EvRefundInvalidated  = "ar.refund.approval_invalidated"
	EvRefundSubmitted    = "ar.refund.submitted"
	EvRefundStateUnknown = "ar.refund.state_unknown"
	EvRefundSettled      = "ar.refund.settled"
	EvRefundFailed       = "ar.refund.failed"
	EvRefundCancelled    = "ar.refund.cancelled"

	EvWriteOffRequested  = "ar.write_off.requested"
	EvWriteOffTaxDecided = "ar.write_off.tax_treatment_decided"
	EvWriteOffInvalid    = "ar.write_off.approval_invalidated"
	EvWriteOffApproved   = "ar.write_off.approved"
	EvReceivableWritten  = "ar.write_off.receivable_written_off"
)

// GL mapping keys (seeded per tenant by general-ledger-svc, ACC-02).
const (
	KeyUnappliedCash = "AR_UNAPPLIED_CASH"
	KeyCashReceipt   = "AR_CASH_RECEIPT"
	KeyBadDebt       = "AR_BAD_DEBT_EXPENSE"
	KeyTaxOutput     = "AR_TAX_OUTPUT"
	KeyReceivable    = "AR_RECEIVABLE_STANDARD"
)

// Refund states.
const (
	RfDraft           = "DRAFT"
	RfPendingApproval = "PENDING_APPROVAL"
	RfApproved        = "APPROVED"
	RfSubmitted       = "SUBMITTED"
	RfPending         = "PENDING"
	RfUnknown         = "UNKNOWN"
	RfSettled         = "SETTLED"
	RfFailed          = "FAILED"
	RfCancelled       = "CANCELLED"
)

// Write-off states.
const (
	WoDraft           = "DRAFT"
	WoPendingApproval = "PENDING_APPROVAL"
	WoApproved        = "APPROVED"
	WoCommitted       = "COMMITTED"
)

// Tax treatment decisions of a write-off (#34).
const (
	TaxNoneRequired  = "NONE_REQUIRED"
	TaxReliefClaimed = "RELIEF_CLAIMED"
	TaxReviewNeeded  = "REVIEW_REQUIRED"
)

// Controlled write-off reason categories.
var ReasonCategories = map[string]bool{"BAD_DEBT": true, "INSOLVENCY": true, "UNCOLLECTIBLE_SMALL_BALANCE": true, "STATUTE_BARRED": true, "SETTLEMENT_DISCOUNT": true}

// MaterialWriteOff: a write-off above it is material, so the principals who handled the item (dispute opener / resolver, collection
// hold owner) cannot approve it (#29). A package constant by design, like the AR-08 tolerance policy.
var MaterialWriteOff = kit.MustAmount("1000.0000")

// Attempt states reported by payment-initiation-adapter-svc (BNK-06) and execution states of payment-status-svc (BNK-07).
const (
	AttemptPrepared      = "PREPARED"
	AttemptSubmitted     = "SUBMITTED"
	AttemptPendingUnkn   = "PENDING_UNKNOWN"
	AttemptRejectedPre   = "REJECTED_BEFORE_SUBMISSION"
	AttemptCancelled     = "CANCELLED"
	AttemptQuarantined   = "QUARANTINED"
	PayPrepared          = "PREPARED"
	PaySubmitted         = "SUBMITTED"
	PayAccepted          = "ACCEPTED"
	PayPending           = "PENDING"
	PaySettled           = "SETTLED"
	PayRejected          = "REJECTED"
	PayReturned          = "RETURNED"
	PayCancelled         = "CANCELLED"
	postingStatePending  = "PENDING"
	controlNoPosting     = "NO_POSTING"
	controlPostingFailed = "POSTING_FAILED"
	controlConsequence   = "AR_CONSEQUENCE_MISSING"
	controlReturned      = "REFUND_RETURNED_AFTER_SETTLEMENT"
)

// ── error constructors ───────────────────────────────────────────────────────

func errSoD(msg string) *kit.Problem { return kit.Forbidden(kit.CodeSegregationOfDuties, msg) }
func errTransition(msg string) *kit.Problem {
	return kit.Conflict(kit.CodeInvalidTransition, msg)
}
func errDestinationChanged(msg string) *kit.Problem {
	return kit.Conflict(kit.CodeRefundDestinationChanged, msg)
}
func errStateUnknown(msg string) *kit.Problem {
	return kit.Conflict(kit.CodeRefundStateUnknown, msg)
}
func errApprovalRequired(msg string) *kit.Problem {
	return kit.Forbidden(kit.CodeWriteOffApprovalRequired, msg)
}
func errScope(msg string) *kit.Problem  { return kit.Forbidden(kit.CodeTenantScope, msg) }
func notFound(what string) *kit.Problem { return kit.NotFound(what + " not found") }
func reasonRequired() *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeReasonRequired, "reason_code is required")
}
func evidenceRequired(why string) *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeEvidenceRequired, "evidence_id is required "+why)
}

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sum(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

// RefundBinding is the hash an approval is bound to: source receipt + amount + currency + destination (and its verification
// fingerprint) + payer account. Anything that changes it invalidates the approval (#30).
func RefundBinding(refundID, receiptID, currency string, amount kit.Amount, destinationRef, fingerprint, payerAccountRef string) string {
	return sum("refund", refundID, receiptID, currency, amount.String(), destinationRef, fingerprint, payerAccountRef)
}

// WriteOffBinding is the hash a write-off approval is bound to (item, amount, reason category, tax decision).
func WriteOffBinding(writeOffID, openItemID, currency string, amount kit.Amount, category, taxTreatment string, relief kit.Amount) string {
	return sum("writeoff", writeOffID, openItemID, currency, amount.String(), category, taxTreatment, relief.String())
}

// ── aggregates ───────────────────────────────────────────────────────────────

// Refund is one customer refund of unapplied cash.
type Refund struct {
	RefundID                string         `json:"refund_id"`
	LegalEntityID           string         `json:"legal_entity_id"`
	CustomerPartyID         string         `json:"customer_party_id,omitempty"`
	ReceiptID               string         `json:"receipt_id"`
	RelatedOpenItemID       string         `json:"related_open_item_id,omitempty"`
	Currency                string         `json:"currency"`
	Amount                  kit.Amount     `json:"amount"`
	DestinationRef          string         `json:"destination_ref"`
	DestinationFingerprint  string         `json:"destination_fingerprint"`
	DestinationVerification map[string]any `json:"destination_verification"`
	PayerAccountRef         string         `json:"payer_account_ref"`
	ReasonCode              string         `json:"reason_code"`
	EvidenceID              string         `json:"evidence_id"`
	State                   string         `json:"state"`
	BindingHash             string         `json:"binding_hash"`
	ApprovedHash            string         `json:"approved_hash,omitempty"`
	RequestedBy             string         `json:"requested_by"`
	ApprovedBy              string         `json:"approved_by,omitempty"`
	ApprovedAt              *time.Time     `json:"approved_at,omitempty"`
	SubmittedBy             string         `json:"submitted_by,omitempty"`
	SubmittedAt             *time.Time     `json:"submitted_at,omitempty"`
	AttemptIdempotencyKey   string         `json:"attempt_idempotency_key,omitempty"`
	ExternalAttemptID       string         `json:"external_attempt_id,omitempty"`
	ProviderRequestID       string         `json:"provider_request_id,omitempty"`
	PaymentStatusID         string         `json:"payment_status_id,omitempty"`
	ExternalState           string         `json:"external_state,omitempty"`
	PaymentState            string         `json:"payment_state,omitempty"`
	FailureReason           string         `json:"failure_reason,omitempty"`
	SettledAt               *time.Time     `json:"settled_at,omitempty"`
	AccountingState         string         `json:"accounting_state,omitempty"`
	JournalID               string         `json:"journal_id,omitempty"`
	AccountingError         string         `json:"accounting_error,omitempty"`
	ControlException        string         `json:"control_exception,omitempty"`
	CancelReason            string         `json:"cancel_reason,omitempty"`
	Version                 int            `json:"version"`
	CreatedAt               time.Time      `json:"created_at"`
	UpdatedAt               time.Time      `json:"updated_at"`
}

// Final reports whether the refund can no longer change state.
func (r *Refund) Final() bool {
	return r.State == RfSettled || r.State == RfFailed || r.State == RfCancelled
}

// Binding recomputes the binding hash from the refund's current content.
func (r *Refund) Binding() string {
	return RefundBinding(r.RefundID, r.ReceiptID, r.Currency, r.Amount, r.DestinationRef, r.DestinationFingerprint, r.PayerAccountRef)
}

// WriteOff is one write-off of (part of) an open item's residual.
type WriteOff struct {
	WriteOffID        string     `json:"write_off_id"`
	LegalEntityID     string     `json:"legal_entity_id"`
	OpenItemID        string     `json:"open_item_id"`
	CustomerPartyID   string     `json:"customer_party_id"`
	Currency          string     `json:"currency"`
	Amount            kit.Amount `json:"amount"`
	ResidualAtRequest kit.Amount `json:"residual_at_request"`
	ReasonCategory    string     `json:"reason_category"`
	ReasonCode        string     `json:"reason_code"`
	EvidenceID        string     `json:"evidence_id"`
	TaxTreatment      string     `json:"tax_treatment"`
	TaxReliefAmount   kit.Amount `json:"tax_relief_amount"`
	TaxReviewer       string     `json:"tax_reviewer,omitempty"`
	TaxEvidenceID     string     `json:"tax_evidence_id,omitempty"`
	TaxReviewedAt     *time.Time `json:"tax_reviewed_at,omitempty"`
	RequiredTier      int        `json:"required_tier"`
	RequesterTier     int        `json:"requester_tier"`
	State             string     `json:"state"`
	BindingHash       string     `json:"binding_hash"`
	ApprovedHash      string     `json:"approved_hash,omitempty"`
	RequestedBy       string     `json:"requested_by"`
	ApprovedBy        string     `json:"approved_by,omitempty"`
	ApprovedTier      int        `json:"approved_tier,omitempty"`
	ApprovedAt        *time.Time `json:"approved_at,omitempty"`
	CommittedBy       string     `json:"committed_by,omitempty"`
	CommittedAt       *time.Time `json:"committed_at,omitempty"`
	FiscalPeriod      string     `json:"fiscal_period,omitempty"`
	AccountingState   string     `json:"accounting_state,omitempty"`
	JournalID         string     `json:"journal_id,omitempty"`
	AccountingError   string     `json:"accounting_error,omitempty"`
	Version           int        `json:"version"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Binding recomputes the binding hash from the write-off's current content.
func (w *WriteOff) Binding() string {
	return WriteOffBinding(w.WriteOffID, w.OpenItemID, w.Currency, w.Amount, w.ReasonCategory, w.TaxTreatment, w.TaxReliefAmount)
}

// HistoryEntry is one append-only lifecycle record.
type HistoryEntry struct {
	Seq           int64          `json:"seq"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	Command       string         `json:"command"`
	FromState     string         `json:"from_state,omitempty"`
	ToState       string         `json:"to_state"`
	ActorID       string         `json:"actor_id"`
	Detail        map[string]any `json:"detail,omitempty"`
	RecordedAt    time.Time      `json:"recorded_at"`
}

// ControlException is a visible control break (never a silent one).
type ControlException struct {
	Kind        string `json:"kind"`
	AggregateID string `json:"aggregate_id"`
	Detail      string `json:"detail"`
}

// ── state machines (pure) ────────────────────────────────────────────────────

// RefundCanCancel: only before the instruction leaves AR.
func RefundCanCancel(state string) bool {
	return state == RfDraft || state == RfPendingApproval || state == RfApproved
}

// RefundMapPayment maps a payment-status-svc execution state to the refund state it implies, and whether it is final.
func RefundMapPayment(status string) (state string, final bool) {
	switch status {
	case PaySettled:
		return RfSettled, true
	case PayRejected, PayCancelled, PayReturned:
		return RfFailed, true
	default: // PREPARED, SUBMITTED, ACCEPTED, PENDING: in flight
		return RfPending, false
	}
}

// ── posting schemes (documented) ─────────────────────────────────────────────

// BuildRefundPosting (spec section 17, "Refund settled"): the customer's unapplied cash liability is discharged by cash paid out.
//
//	Dr AR_UNAPPLIED_CASH / Cr AR_CASH_RECEIPT   (refunded amount)
//
// AR_CASH_RECEIPT is the AR cash-in-transit mapping the AR-08 receipt posting debits, so the cash leg is the exact mirror of it.
func BuildRefundPosting(amount kit.Amount) []kit.PostingLine {
	return []kit.PostingLine{
		{MappingKey: KeyUnappliedCash, Debit: amount, Description: "Refund of unapplied customer cash"},
		{MappingKey: KeyCashReceipt, Credit: amount, Description: "Customer refund paid"},
	}
}

// BuildWriteOffPosting (spec section 17, "Write-off committed"):
//
//	Dr AR_BAD_DEBT_EXPENSE (amount - relief) / Dr AR_TAX_OUTPUT (relief, when RELIEF_CLAIMED) / Cr AR_RECEIVABLE_STANDARD (amount)
func BuildWriteOffPosting(w *WriteOff) []kit.PostingLine {
	expense := w.Amount.Sub(w.TaxReliefAmount)
	var lines []kit.PostingLine
	if expense.IsPositive() {
		lines = append(lines, kit.PostingLine{MappingKey: KeyBadDebt, Debit: expense, Description: "Bad debt written off (" + w.ReasonCategory + ")"})
	}
	if w.TaxReliefAmount.IsPositive() {
		lines = append(lines, kit.PostingLine{MappingKey: KeyTaxOutput, Debit: w.TaxReliefAmount, Description: "Output tax relief on write-off"})
	}
	return append(lines, kit.PostingLine{MappingKey: KeyReceivable, Credit: w.Amount, Description: "Receivable written off"})
}
