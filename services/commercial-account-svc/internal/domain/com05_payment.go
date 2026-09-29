// COM-05 Platform Commercial Billing, part 5b (ZS-SVC-Q-001 §4.5). Owns
// PaymentAttemptRef and the CollectionState query surface.
//
// COM-05 must not own (§4.5) bank/provider settlement truth: a
// PaymentAttemptRef records what a processor reported, never a computed or
// assumed settlement outcome. CollectionState is not a second persisted
// state machine — see migration 000013's header — it is a derived summary
// of an invoice's own payment attempts, computed by CollectionStateOf.
package domain

import "time"

const PrefixPaymentAttempt = "cpat_"

type PaymentAttemptStatus string

const (
	PaymentCreated        PaymentAttemptStatus = "CREATED"
	PaymentSubmitted      PaymentAttemptStatus = "SUBMITTED"
	PaymentPendingUnknown PaymentAttemptStatus = "PENDING_UNKNOWN"
	PaymentSucceeded      PaymentAttemptStatus = "SUCCEEDED"
	PaymentFailed         PaymentAttemptStatus = "FAILED"
	PaymentVoided         PaymentAttemptStatus = "VOIDED"
)

// Resolved reports whether this status is terminal — no further outcome can
// ever be recorded against the attempt (negative path #29).
func (s PaymentAttemptStatus) Resolved() bool {
	return s == PaymentSucceeded || s == PaymentFailed || s == PaymentVoided
}

// PaymentAttemptRef is one durable, provider-facing attempt to collect an
// invoice's total. It exists before any external submission is even
// conceptually made (COM-CTRL-023) — CollectPayment creates it in CREATED,
// never after a call to whatever collects the money.
type PaymentAttemptRef struct {
	AttemptID             string               `json:"attempt_id"`
	OrganizationID        string               `json:"organization_id"`
	InvoiceID             string               `json:"invoice_id"`
	Amount                string               `json:"amount"`
	CurrencyCode          string               `json:"currency_code"`
	Status                PaymentAttemptStatus `json:"status"`
	ProviderAttemptRef    *string              `json:"provider_attempt_ref,omitempty"`
	ProviderEventID       *string              `json:"provider_event_id,omitempty"`
	SettlementRef         *string              `json:"settlement_ref,omitempty"`
	FailureReason         *string              `json:"failure_reason,omitempty"`
	CreatedAt             time.Time            `json:"created_at"`
	CreatedByPrincipalID  string               `json:"created_by_principal_id"`
	SubmittedAt           *time.Time           `json:"submitted_at,omitempty"`
	ResolvedAt            *time.Time           `json:"resolved_at,omitempty"`
	ResolvedByPrincipalID *string              `json:"resolved_by_principal_id,omitempty"`
}

// RecordOutcomeRequest is what a provider adapter or webhook handler reports
// back about an attempt. ProviderEventID, when set, is the dedup key for a
// duplicate callback delivery (negative path #28); it is optional because
// not every outcome arrives as a distinct provider event (e.g. a
// synchronous submit response).
type RecordOutcomeRequest struct {
	AttemptID          string
	Outcome            PaymentAttemptStatus // SUBMITTED | PENDING_UNKNOWN | SUCCEEDED | FAILED | VOIDED
	ProviderAttemptRef *string
	ProviderEventID    *string
	SettlementRef      *string
	FailureReason      *string
	OccurredAt         time.Time
	ActorPrincipalID   string
}

func (r *RecordOutcomeRequest) Validate() error {
	switch r.Outcome {
	case PaymentSubmitted, PaymentPendingUnknown, PaymentSucceeded, PaymentFailed, PaymentVoided:
	default:
		return invalid("outcome", "must be SUBMITTED, PENDING_UNKNOWN, SUCCEEDED, FAILED or VOIDED")
	}
	if r.Outcome == PaymentSucceeded && (r.SettlementRef == nil || strEmpty(*r.SettlementRef)) {
		return invalid("settlement_ref", "is required when outcome is SUCCEEDED (COM-CTRL-027)")
	}
	if r.Outcome == PaymentFailed && (r.FailureReason == nil || strEmpty(*r.FailureReason)) {
		return invalid("failure_reason", "is required when outcome is FAILED")
	}
	return nil
}

// CollectionState is CollectionStateOf's derived summary — never stored on
// its own.
type CollectionState string

const (
	CollectionCurrent        CollectionState = "CURRENT"
	CollectionPaymentPending CollectionState = "PAYMENT_PENDING"
	CollectionPaid           CollectionState = "PAID"
	CollectionFailed         CollectionState = "FAILED"
)

// CollectionStateOf derives an invoice's collection state from its payment
// attempts, most-recent first. PAID wins if any attempt ever succeeded
// (a later failed retry does not un-pay an invoice); otherwise an unresolved
// attempt in flight means PAYMENT_PENDING; otherwise the most recent
// resolved attempt's outcome (FAILED/VOIDED) or, with no attempts at all,
// CURRENT.
func CollectionStateOf(attempts []PaymentAttemptRef) CollectionState {
	for _, a := range attempts {
		if a.Status == PaymentSucceeded {
			return CollectionPaid
		}
	}
	for _, a := range attempts {
		if !a.Status.Resolved() {
			return CollectionPaymentPending
		}
	}
	if len(attempts) > 0 {
		return CollectionFailed
	}
	return CollectionCurrent
}

var (
	ErrInvoiceAlreadyPaid         = errorString("this invoice already has a succeeded payment attempt")
	ErrPaymentAttemptNotFound     = errorString("payment attempt not found")
	ErrPaymentAttemptInvalidState = errorString("payment attempt is not in a state that allows this action")
	ErrDuplicateProviderEvent     = errorString("this provider event id was already recorded")
)
