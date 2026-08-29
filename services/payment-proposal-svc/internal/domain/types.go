// Package domain defines the authoritative domain types for
// payment-proposal-svc — AP-09 of the Procurement, Expenses & Accounts
// Payable baseline. Its job: select eligible payables into an exact payment
// proposal under due-date/hold/withholding/payment-method policy, while
// remaining non-executing and fully reversible before authorization.
//
// AP-09's own command list (CreatePaymentProposal, AddEligiblePayable,
// RemovePayable, RecalculatePaymentProposal, SubmitProposalForReview,
// FreezePaymentProposal, CancelPaymentProposal) has no Authorize or Reject
// command — those belong to AP-10 ("Payment Authorization"), a distinct,
// unbuilt service. This service therefore only ever reaches FROZEN; the
// spec's own state model names AUTHORIZED/REJECTED as later states but this
// service never writes them (kept in the status CHECK constraint for
// forward compatibility, same as AP-07 kept an unused APPROVED value).
// AP-09's SoD line ("proposal preparer cannot be sole authorizer where
// maker-checker applies") has nothing to bite on with no Authorize command
// here — the closest real analogue this service owns is FreezePaymentProposal
// itself, the last checkpoint before authorization would occur, so Freeze
// requires a principal distinct from the proposal's own preparer via
// authorization-svc's dynamic own-object SoD layer (the fourth reuse of
// that feature this session, after AP-01, AP-04, AP-07).
//
// Real integrations, verified against each peer's actual code before this
// service was written:
//
//   - accounts-payable-svc's real GET /v1/invoices/{id}: an APPROVED
//     invoice is eligible for selection. It has no hold/disputed status of
//     its own and no bank/payee reference field at all.
//   - expense-claim-svc's real GET /ap07/expense-claims/{id}: a
//     REIMBURSABLE claim is eligible. It has no stored total either — the
//     total is summed from its lines client-side, the same way AP-07's own
//     handler computes it; this service does the same rather than inventing
//     a total field that doesn't exist upstream.
//   - supplier-financial-profile-svc's real GET /ap01/supplier-financial-profiles
//     (list, unfiltered — it has no server-side filter by supplier_ref) is
//     the ONLY real source of a hold concept usable here: its ON_HOLD
//     status is negative-path scenario #1's literal enforcement point — an
//     AP_INVOICE item whose supplier profile is ON_HOLD is rejected unless
//     the caller supplies a non-empty ExceptionRef (a controlled,
//     caller-attested override, the same "opaque approval reference"
//     doctrine as AP-01's ToleranceExceptionRef/AP-04's
//     ToleranceExceptionRef). accounts-payable-svc itself has no hold
//     concept, so this is the only place a hold signal can come from.
//   - supplier-financial-profile-svc's `updated_at` is the only available
//     staleness signal for negative-path scenario #4 ("payment uses stale
//     payee identity version") — there is no version/revision column on
//     that table (checked directly). Every AP_INVOICE item captures the
//     profile's updated_at as PayeeSnapshotAt when added; FreezePaymentProposal
//     re-fetches the live profile and blocks (409) if it has changed since,
//     rather than freezing against a payee identity that may already be
//     wrong. EXPENSE_CLAIM items carry no such risk: their payment
//     preference is captured once on the claim itself and is immutable the
//     moment the claim leaves DRAFT/RETURNED (AP-07's own trigger), so there
//     is nothing that can go stale under this service's feet.
//   - tax-determination-svc's real POST /v1/tax-determinations, called once
//     per AP_INVOICE item when the caller declares ApplyWithholding — the
//     same "real call, never inferred" doctrine as AP-07's tax-recovery
//     lines, reused for withholding instead of reclaim.
//
// Two honest gaps, not fabricated:
//
//   - BNK account/cash availability: neither bank-reconciliation-svc nor
//     banking-connector-svc exposes a live balance/availability check
//     (confirmed directly in their code) — PayingBankAccountRef is an
//     opaque, caller-supplied reference, never resolved or balance-checked,
//     the same doctrine as AP-01's PayeeReference.
//   - GetFingerprint computes a real SHA-256 digest over the frozen
//     proposal's own item set and amounts (content-addressed, the same
//     principle tax-determination-svc already uses for its rule snapshot,
//     though this is the first "subject fingerprint" of AP-09's own kind
//     in this codebase — there was no existing pattern named exactly that
//     to follow).
package domain

import "time"

type ProposalStatus string

const (
	StatusDraft      ProposalStatus = "DRAFT"
	StatusCalculated ProposalStatus = "CALCULATED"
	StatusReview     ProposalStatus = "REVIEW"
	StatusFrozen     ProposalStatus = "FROZEN"
	StatusAuthorized ProposalStatus = "AUTHORIZED" // never written by this service — see package doc
	StatusRejected   ProposalStatus = "REJECTED"   // never written by this service — see package doc
	StatusCancelled  ProposalStatus = "CANCELLED"
)

// CanMutateItems reports whether items may be added/removed while the
// proposal is in status s.
func CanMutateItems(s ProposalStatus) bool {
	return s == StatusDraft || s == StatusCalculated || s == StatusReview
}

// CanRecalculate reports whether the proposal may be (re)calculated.
func CanRecalculate(s ProposalStatus) bool {
	return s == StatusDraft || s == StatusCalculated || s == StatusReview
}

// CanSubmitForReview reports whether the proposal may move to REVIEW.
func CanSubmitForReview(s ProposalStatus) bool { return s == StatusCalculated }

// CanFreeze reports whether the proposal may be frozen.
func CanFreeze(s ProposalStatus) bool { return s == StatusReview }

// CanCancel reports whether the proposal may be cancelled.
func CanCancel(s ProposalStatus) bool {
	return s == StatusDraft || s == StatusCalculated || s == StatusReview || s == StatusFrozen
}

type PayableSource string

const (
	SourceAPInvoice    PayableSource = "AP_INVOICE"
	SourceExpenseClaim PayableSource = "EXPENSE_CLAIM"
)

func ValidPayableSource(s PayableSource) bool {
	return s == SourceAPInvoice || s == SourceExpenseClaim
}

type PaymentProposal struct {
	ProposalID           string
	TenantID             *string
	LegalEntityID        string
	PayingBankAccountRef string
	Currency             string
	PaymentDate          time.Time
	PaymentMethod        string

	Status ProposalStatus

	GrossAmount       float64
	WithholdingAmount float64
	NetAmount         float64

	FrozenByPrincipalID *string
	FrozenAt            *time.Time

	CreatedByPrincipalID string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type ProposalItem struct {
	ItemID        string
	TenantID      *string
	ProposalID    string
	PayableSource PayableSource
	PayableID     string
	PayeeRef      string // supplier_ref (AP_INVOICE) or claimant_principal_id (EXPENSE_CLAIM)

	GrossAmount       float64
	WithholdingAmount float64
	NetAmount         float64
	Currency          string
	DueDate           time.Time

	// PayeeSnapshotAt is the supplier profile's updated_at captured at
	// AddEligiblePayable time — nil for EXPENSE_CLAIM items, which carry no
	// staleness risk (see package doc).
	PayeeSnapshotAt *time.Time

	TaxDeterminationID string
	ExceptionRef       string // non-empty means a held item was force-added
	IsActive           bool   // false once the parent proposal is cancelled — frees the payable for reselection

	CreatedAt time.Time
}

type ProposalEvent struct {
	EventID          string
	TenantID         *string
	ProposalID       string
	EventType        string
	Detail           string
	ActorPrincipalID string
	CreatedAt        time.Time
}

const (
	EventProposalCreated    = "PAYMENT_PROPOSAL_CREATED"
	EventProposalCalculated = "PAYMENT_PROPOSAL_CALCULATED"
	EventProposalFrozen     = "PAYMENT_PROPOSAL_FROZEN"
	EventProposalChanged    = "PAYMENT_PROPOSAL_CHANGED"
	EventProposalCancelled  = "PAYMENT_PROPOSAL_CANCELLED"
)

// ── request DTOs ────────────────────────────────────────────────────────────

type CreateProposalRequest struct {
	LegalEntityID        string
	PayingBankAccountRef string
	Currency             string
	PaymentDate          time.Time
	PaymentMethod        string
}

type AddEligiblePayableRequest struct {
	PayableSource    PayableSource
	PayableID        string
	ApplyWithholding bool
	JurisdictionID   string
	TaxCategory      string
	ExceptionRef     string // required to force-add an ON_HOLD supplier's invoice
}

type CancelProposalRequest struct {
	Reason string
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrProposalNotFound          = sentinel("payment proposal not found")
	ErrInvalidTransition         = sentinel("invalid payment proposal state transition")
	ErrItemNotFound              = sentinel("proposal item not found")
	ErrPayableAlreadyInProposal  = sentinel("payable is already an active item on another proposal")
	ErrPayableNotEligible        = sentinel("payable does not exist or is not in an eligible status")
	ErrPayableServiceUnavailable = sentinel("upstream payable service unavailable")
	ErrPayeeNotFound             = sentinel("supplier financial profile not found for this payable's payee reference")
	ErrPayeeOnHold               = sentinel("payee is on hold; an exception reference is required to add this payable")
	ErrPayeeServiceUnavailable   = sentinel("supplier-financial-profile-svc unavailable")
	ErrPayeeIdentityStale        = sentinel("payee identity has changed since this payable was added; recalculate before freezing")
	ErrTaxDeterminationFailed    = sentinel("tax-determination-svc call failed for an item with withholding applied")
	ErrNoItems                   = sentinel("proposal has no items")
	ErrStoreUnavailable          = sentinel("store unavailable")
)
