// Package domain defines the authoritative domain types for
// payable-open-item-svc — AP-08 of the Procurement, Expenses & Accounts
// Payable baseline. Its job per spec: "Maintain authoritative
// supplier/expense open-item liabilities, residual amounts, due dates,
// holds/disputes, credits and settlement applications as the basis for
// payment selection and AP-to-GL reconciliation."
//
// THE GAP THIS SERVICE CLOSES — read before anything else. Two services
// built earlier this session each documented, in their own package docs,
// that they had no real AP-08 to hand off to:
//   - expense-claim-svc (AP-07): ApproveExpenseClaim only emitted an
//     unconsumed ExpenseClaimPayableRequested event and could never reach
//     CLOSED, only REIMBURSABLE — because accounts-payable-svc's entire
//     model is vendor-invoice-shaped (vendor_id, CreateInvoice/
//     ApproveInvoice) with no claimant/reimbursement concept, and routing a
//     claim through it as a fabricated "vendor invoice" would have been
//     exactly the kind of invented integration this codebase's discipline
//     forbids.
//   - payment-proposal-svc (AP-09): pulls its eligible-payable population
//     directly from accounts-payable-svc and expense-claim-svc rather than
//     through a proper payables ledger, because no such ledger existed.
//
// This service is that ledger. Confirmed directly against the actual spec
// (docs/original_doc/ZoikoSuite_Procurement_Expenses_Accounts_Payable...
// section 11, "AP-08 — Payables") before writing a line of code.
//
// What IS wired in THIS build, real, not fabricated:
//   - expense-claim-svc's ApproveExpenseClaim now calls this service's real
//     CreatePayableFromApprovedSource (see expense-claim-svc's own updated
//     package doc) instead of emitting an unconsumed event — the first real
//     consumer AP-07 has ever had for its approved claims.
//
// What is a documented, honest gap, left for a natural next step rather
// than fabricated:
//   - accounts-payable-svc (vendor invoices, AP-05/06) is NOT yet wired as
//     a second source — only expense-claim-svc is, in this build. Wiring
//     accounts-payable-svc's own ApproveInvoice to also call
//     CreatePayableFromApprovedSource would give this service its second
//     real source and is scoped but not started here.
//   - payment-proposal-svc (AP-09) still sources payables directly from
//     accounts-payable-svc/expense-claim-svc, not from this service yet —
//     switching AP-09 over to source from here (ListOpenPayables/
//     GetPaymentEligibility) is the natural next wiring step, mirroring
//     exactly how AP-11 was wired to BNK-06/BNK-07 in a later turn after
//     those services were first built.
//   - ApplyConfirmedPayment has no real caller yet (would come from AP-11/
//     BNK-07's settlement chain) — it is a caller-attested command for now,
//     the same "record a real external fact a human observed" doctrine
//     used throughout this session.
//   - ApplyRecovery names AP-12 (Supplier Refund/Recovery) as its source —
//     AP-12 does not exist anywhere in this codebase, so its
//     RecoveryReference is treated as an opaque caller-supplied reference,
//     the same pattern as AP-01's PayeeReference/AP-09's
//     PayingBankAccountRef.
//
// Two scope decisions of this service's own design, not gaps in a peer:
//   - The spec's state model names "Recognized → Open → PartiallySettled →
//     Settled" but the command list has no separate command to move
//     Recognized -> Open. This service collapses that into
//     CreatePayableFromApprovedSource landing directly in OPEN — the same
//     kind of honest consolidation AP-07 itself used for its own
//     Submitted/PendingApproval gap.
//   - ClosePayable is in the spec's command list but there is no
//     corresponding state in its own state model and no PayableClosed
//     event in its own events list — a real inconsistency in the spec
//     text itself. This service treats ClosePayable as the explicit,
//     operator-driven act of marking an already-fully-SETTLED,
//     non-disputed, non-held payable as archived/finalized (setting
//     ClosedAt), not as a new state — the smallest reading consistent with
//     both the command existing and no new state/event being defined for
//     it, documented here rather than silently invented.
package domain

import "time"

type PayableStatus string

const (
	StatusOpen             PayableStatus = "OPEN"
	StatusPartiallySettled PayableStatus = "PARTIALLY_SETTLED"
	StatusSettled          PayableStatus = "SETTLED"
)

type SourceType string

const (
	SourceExpenseClaim     SourceType = "EXPENSE_CLAIM"
	SourceSupplierInvoice  SourceType = "SUPPLIER_INVOICE"
	SourceAuthorizedAdjust SourceType = "AUTHORIZED_ADJUSTMENT"
)

// CanApplySettlement reports whether a payable in status s may receive a
// further settlement application (payment or credit).
func CanApplySettlement(s PayableStatus) bool { return s == StatusOpen || s == StatusPartiallySettled }

// CanClose reports whether a payable may be marked closed/archived — only
// once fully settled, never held, never disputed. See package doc for why
// ClosePayable is modeled this way.
func CanClose(status PayableStatus, isHeld, isDisputed bool) bool {
	return status == StatusSettled && !isHeld && !isDisputed
}

// IsEligibleForPayment is the literal enforcement of negative-path #2
// ("disputed payable included as eligible payment") plus its held
// counterpart: a payable is only eligible for payment selection while it
// has real residual left, isn't fully settled, isn't held, and isn't
// disputed.
func IsEligibleForPayment(status PayableStatus, isHeld, isDisputed bool, residual float64) bool {
	return (status == StatusOpen || status == StatusPartiallySettled) && !isHeld && !isDisputed && residual > 0
}

type PayableOpenItem struct {
	PayableID       string
	TenantID        *string
	LegalEntityID   string
	SourceType      SourceType
	SourceReference string // unique per (SourceType, SourceReference) — the direct fix for negative-path #4
	PayeeRef        string

	OriginalAmount float64
	ResidualAmount float64
	Currency       string
	DueDate        time.Time

	Status PayableStatus

	IsHeld     bool
	HoldReason string

	IsDisputed      bool
	DisputeReason   string
	DisputeOpenedAt *time.Time

	ClosedAt *time.Time

	CreatedByPrincipalID string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// SettlementApplication is an append-only record of every residual-reducing
// (or, for a supplier credit, residual-increasing-toward-zero) event
// applied to a payable — payment, credit, or recovery — each carrying its
// own idempotency reference. This is what GetPayableHistory and negative-
// path #3 ("confirmed payment applied twice") are built on.
type SettlementApplication struct {
	ApplicationID    string
	TenantID         *string
	PayableID        string
	ApplicationType  string // PAYMENT | SUPPLIER_CREDIT | RECOVERY
	Amount           float64
	IdempotencyRef   string // e.g. BNK-07 payment_id for PAYMENT; credit/recovery reference otherwise
	Detail           string
	ActorPrincipalID string
	CreatedAt        time.Time
}

const (
	EventPayableCreated          = "PAYABLE_CREATED"
	EventPayableHeld             = "PAYABLE_HELD"
	EventPayableHoldReleased     = "PAYABLE_HOLD_RELEASED"
	EventPayableDisputeOpened    = "PAYABLE_DISPUTE_OPENED"
	EventPayableDisputeResolved  = "PAYABLE_DISPUTE_RESOLVED"
	EventPayablePartiallySettled = "PAYABLE_PARTIALLY_SETTLED"
	EventPayableSettled          = "PAYABLE_SETTLED"
	EventPayableCorrected        = "PAYABLE_CORRECTED"
	EventPayableRecoveryApplied  = "PAYABLE_RECOVERY_APPLIED"
	EventPayableClosed           = "PAYABLE_CLOSED"
)

// ── request DTOs ────────────────────────────────────────────────────────────

type CreatePayableRequest struct {
	LegalEntityID   string
	SourceType      SourceType
	SourceReference string
	PayeeRef        string
	OriginalAmount  float64
	Currency        string
	DueDate         time.Time
}

type ApplySupplierCreditRequest struct {
	Amount    float64
	CreditRef string
	Reason    string
}

type PlaceHoldRequest struct {
	Reason string
}

type OpenDisputeRequest struct {
	Reason string
}

type ResolveDisputeRequest struct {
	Resolution string
}

type ApplyConfirmedPaymentRequest struct {
	Amount             float64
	ProviderPaymentRef string // idempotency reference — e.g. BNK-07's payment_id
}

type ApplyRecoveryRequest struct {
	Amount      float64
	RecoveryRef string
	Reason      string
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrPayableNotFound          = sentinel("payable open item not found")
	ErrInvalidTransition        = sentinel("invalid payable state transition")
	ErrDuplicateSource          = sentinel("a payable already exists for this source_type/source_reference")
	ErrResidualWouldGoNegative  = sentinel("this application would take the residual negative; only a supplier credit may do that")
	ErrSettlementAlreadyApplied = sentinel("this settlement reference has already been applied")
	ErrPayableHeldOrDisputed    = sentinel("payable is held or disputed and cannot be closed")
	ErrPayableNotFullySettled   = sentinel("payable must be fully settled before it can be closed")
	ErrStoreUnavailable         = sentinel("store unavailable")
)
