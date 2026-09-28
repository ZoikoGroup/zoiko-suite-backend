// COM-05 Platform Commercial Billing, part 5c (ZS-SVC-Q-001 §4.5). Owns
// CreditNote, RefundRequest, write-offs and the OutstandingBalance query
// surface.
//
// An issued invoice is never edited to reflect any of these (§4.5 failure
// semantics: "Closed invoice immutable; correction through
// credit/debit/adjustment") — each is its own immutable document, and
// OutstandingBalance (GetBalance) is always computed from the invoice plus
// every document issued against it, never a second running total that could
// drift from them.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const (
	PrefixCreditNote    = "ccrn_"
	PrefixWriteOff      = "cwof_"
	PrefixRefundRequest = "crfd_"
)

// CreditNote reduces what is owed on an issued invoice. It never reduces
// what has already been collected — that is a RefundRequest's job.
type CreditNote struct {
	CreditNoteID        string    `json:"credit_note_id"`
	OrganizationID      string    `json:"organization_id"`
	InvoiceID           string    `json:"invoice_id"`
	Amount              string    `json:"amount"`
	CurrencyCode        string    `json:"currency_code"`
	Reason              string    `json:"reason"`
	IssuedAt            time.Time `json:"issued_at"`
	IssuedByPrincipalID string    `json:"issued_by_principal_id"`
}

// WriteOff also reduces what is owed, recorded distinctly from a CreditNote
// so the two reasons ("we were wrong to charge this" vs. "we accept this
// will never be collected") are never conflated in evidence.
type WriteOff struct {
	WriteOffID           string    `json:"write_off_id"`
	OrganizationID       string    `json:"organization_id"`
	InvoiceID            string    `json:"invoice_id"`
	Amount               string    `json:"amount"`
	CurrencyCode         string    `json:"currency_code"`
	Reason               string    `json:"reason"`
	AppliedAt            time.Time `json:"applied_at"`
	AppliedByPrincipalID string    `json:"applied_by_principal_id"`
}

type RefundStatus string

const (
	RefundRequested RefundStatus = "REQUESTED"
	RefundSettled   RefundStatus = "SETTLED"
	RefundFailed    RefundStatus = "FAILED"
)

// RefundRequest returns money already collected via a specific SUCCEEDED
// PaymentAttemptRef. DestinationFingerprint is the COM-CTRL-028 control:
// SettleRefund must present the same destination that was requested, or the
// settlement is refused rather than paid out to a changed destination
// (negative path #31).
type RefundRequest struct {
	RefundID               string       `json:"refund_id"`
	OrganizationID         string       `json:"organization_id"`
	InvoiceID              string       `json:"invoice_id"`
	PaymentAttemptID       string       `json:"payment_attempt_id"`
	Amount                 string       `json:"amount"`
	CurrencyCode           string       `json:"currency_code"`
	DestinationRef         string       `json:"destination_ref"`
	DestinationFingerprint string       `json:"destination_fingerprint"`
	Reason                 string       `json:"reason"`
	Status                 RefundStatus `json:"status"`
	RequestedAt            time.Time    `json:"requested_at"`
	RequestedByPrincipalID string       `json:"requested_by_principal_id"`
	SettlementRef          *string      `json:"settlement_ref,omitempty"`
	FailureReason          *string      `json:"failure_reason,omitempty"`
	ResolvedAt             *time.Time   `json:"resolved_at,omitempty"`
	ResolvedByPrincipalID  *string      `json:"resolved_by_principal_id,omitempty"`
}

// DestinationFingerprint is the one place a refund destination is hashed —
// RequestRefund and SettleRefund must always compute it the same way, or a
// legitimate settlement would be refused as a mismatch.
func DestinationFingerprint(destinationRef string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(destinationRef)))
	return hex.EncodeToString(sum[:])
}

// SettleRefundRequest is what a provider adapter or operator reports back
// about a refund's outcome. DestinationRef must match the fingerprint the
// refund was requested with.
type SettleRefundRequest struct {
	RefundID         string
	Outcome          RefundStatus // SETTLED | FAILED
	DestinationRef   string
	SettlementRef    *string
	FailureReason    *string
	OccurredAt       time.Time
	ActorPrincipalID string
}

func (r *SettleRefundRequest) Validate() error {
	switch r.Outcome {
	case RefundSettled, RefundFailed:
	default:
		return invalid("outcome", "must be SETTLED or FAILED")
	}
	if strEmpty(r.DestinationRef) {
		return invalid("destination_ref", "is required to settle a refund (COM-CTRL-028)")
	}
	if r.Outcome == RefundSettled && (r.SettlementRef == nil || strEmpty(*r.SettlementRef)) {
		return invalid("settlement_ref", "is required when outcome is SETTLED")
	}
	if r.Outcome == RefundFailed && (r.FailureReason == nil || strEmpty(*r.FailureReason)) {
		return invalid("failure_reason", "is required when outcome is FAILED")
	}
	return nil
}

// OutstandingBalance is GetBalance's answer: a derived commercial balance,
// shown with the components it was computed from rather than as a bare
// number, so it is evidence, not merely an assertion.
type OutstandingBalance struct {
	OrganizationID  string `json:"organization_id"`
	CurrencyCode    string `json:"currency_code"`
	TotalInvoiced   string `json:"total_invoiced"`
	TotalCredited   string `json:"total_credited"`
	TotalWrittenOff string `json:"total_written_off"`
	TotalCollected  string `json:"total_collected"`
	TotalRefunded   string `json:"total_refunded"`
	Balance         string `json:"balance"`
}

var (
	ErrCreditExceedsInvoice       = errorString("credit note amount would exceed the invoice's remaining owed balance")
	ErrWriteOffExceedsInvoice     = errorString("write-off amount would exceed the invoice's remaining owed balance")
	ErrRefundExceedsCollected     = errorString("refund amount would exceed what remains collected on this payment attempt")
	ErrPaymentAttemptNotSucceeded = errorString("a refund can only be requested against a succeeded payment attempt")
	ErrRefundRequestNotFound      = errorString("refund request not found")
	ErrRefundInvalidState         = errorString("refund request is not in a state that allows this action")
	ErrRefundDestinationMismatch  = errorString("the settlement destination does not match the destination this refund was approved for; re-request the refund against the new destination")
)
