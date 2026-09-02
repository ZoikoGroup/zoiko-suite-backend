// Package domain defines the authoritative domain types for
// expense-claim-svc — AP-07 of the Procurement, Expenses & Accounts Payable
// baseline. Its job: capture employee/authorized-claimant expenses with
// receipt, business-purpose, policy and tax evidence, creating a
// reimbursement/payable basis, without allowing self-approval or direct
// bank execution.
//
// Real integrations, verified against each peer's actual code before this
// service was written, not assumed:
//
//   - Claimant identity: employee-master-svc's real GET /v1/employees/{id}
//     verifies the claimant exists and is ACTIVE (not ONBOARDING/SUSPENDED/
//     TERMINATED) at CreateExpenseClaim time.
//   - Receipt evidence: document-vault-svc's real GET /v1/documents/{id}
//     verifies a receipt reference genuinely exists, belongs to the
//     caller's tenant/legal entity, and isn't PURGE_PENDING. Negative-path
//     scenario #2 ("same receipt used on two claims") is enforced by a
//     genuine partial UNIQUE index on receipt_document_id — a database
//     invariant, not an application check a race could defeat.
//   - Tax: tax-determination-svc's real POST /v1/tax-determinations is
//     called, per line, whenever a line declares ClaimTaxRecovery — this is
//     the literal enforcement of negative-path scenario #4 ("tax reclaim
//     inferred without TAX result" must be blocked): TaxableAmount and
//     CalculatedTaxAmount are NEVER set by this service except from that
//     real call's own response, keyed by the returned DeterminationID, and
//     a failed call blocks SubmitExpenseClaim outright rather than leaving
//     a claim with an invented tax figure.
//   - Policy: policy-svc's real POST /v1/policies/evaluate is called at
//     Submit with policy_type=APPROVAL_THRESHOLD (the only policy type it
//     has real evaluation logic for) against the claim's total amount. A
//     404 "no_applicable_policy" (no threshold policy configured yet) is
//     treated as WITHIN_THRESHOLD — policy-svc's own doc comment says
//     explicitly "this service does not guess fail-open/fail-closed... the
//     caller decides", and an unconfigured threshold is a setup gap, not a
//     security signal, so this service defaults permissive rather than
//     fabricating a threshold. This is a judgment call, documented here
//     rather than hidden.
//   - AP-08 "Payables": accounts-payable-svc exists but its entire model is
//     vendor-invoice-shaped (vendor_id, CreateInvoice/ApproveInvoice) with
//     no claimant/reimbursement concept anywhere in it, so routing an
//     approved claim through it as a fabricated "vendor invoice" was never
//     going to happen. UPDATE: payable-open-item-svc (a real AP-08) now
//     exists specifically to close this gap — ApproveExpenseClaim calls its
//     real CreatePayableFromApprovedSource (internal/payableopenitem
//     client) instead of emitting an unconsumed event. The call is
//     deliberately best-effort, mirroring goods-service-receipt-svc's own
//     GRNI-posting doctrine: a failure emits
//     EXPENSE_CLAIM_PAYABLE_CREATE_FAILED for visibility but never undoes
//     the approval that already succeeded — the claimant should not be
//     penalized for a downstream service being unavailable at this instant.
//     PayeeRef falls back to the claimant's own principal ID when no
//     PaymentPreferenceRef was recorded; DueDate is "now" since nothing in
//     this domain gives a reimbursement a negotiated payment term.
//
// Two scope decisions of this service's own design, not gaps in a peer:
//
//   - The spec's state model names both "Submitted" and "PendingApproval"
//     as distinct states but the command list has only one command
//     (SubmitExpenseClaim) to reach either — there is no second command
//     to move Submitted -> PendingApproval. This service collapses them
//     into one PENDING_APPROVAL status reached directly by
//     SubmitExpenseClaim, the same kind of honest consolidation as AP-01's
//     added Activate command for an analogous gap in its own state model.
//   - "Approved -> Reimbursable -> Closed" per the spec, but Closed would
//     require a real payables consumer to report the reimbursement
//     actually settled — since none exists (see AP-08 above), this service
//     only ever reaches REIMBURSABLE, never CLOSED. Also not in the spec's
//     command list: an explicit CANCELLED terminal state is added for the
//     CancelExpenseClaim command the spec's own commands list does name
//     but the state model diagram omits.
package domain

import "time"

type ClaimStatus string

const (
	StatusDraft           ClaimStatus = "DRAFT"
	StatusPendingApproval ClaimStatus = "PENDING_APPROVAL"
	StatusApproved        ClaimStatus = "APPROVED"
	StatusRejected        ClaimStatus = "REJECTED"
	StatusReturned        ClaimStatus = "RETURNED"
	StatusReimbursable    ClaimStatus = "REIMBURSABLE"
	StatusCancelled       ClaimStatus = "CANCELLED"
)

var submittableFrom = map[ClaimStatus]bool{StatusDraft: true, StatusReturned: true}

// CanSubmit reports whether a claim in status s may be submitted.
func CanSubmit(s ClaimStatus) bool { return submittableFrom[s] }

// CanDecide reports whether a claim in status s may be approved, rejected,
// returned for correction, or have a policy exception recorded against it.
func CanDecide(s ClaimStatus) bool { return s == StatusPendingApproval }

var cancellableFrom = map[ClaimStatus]bool{StatusDraft: true, StatusPendingApproval: true, StatusReturned: true}

// CanCancel reports whether a claim in status s may be cancelled.
func CanCancel(s ClaimStatus) bool { return cancellableFrom[s] }

// CanAddLine reports whether a claim in status s may still accept new/
// amended expense lines.
func CanAddLine(s ClaimStatus) bool { return s == StatusDraft || s == StatusReturned }

// PolicyAssessmentResult mirrors policy-svc's own APPROVAL_THRESHOLD
// evaluate() result values exactly.
type PolicyAssessmentResult string

const (
	PolicyWithinThreshold  PolicyAssessmentResult = "WITHIN_THRESHOLD"
	PolicyApprovalRequired PolicyAssessmentResult = "APPROVAL_REQUIRED"
	PolicyNotAssessed      PolicyAssessmentResult = "NOT_ASSESSED"
)

type ExpenseClaim struct {
	ClaimID              string
	TenantID             *string
	LegalEntityID        string
	ClaimantPrincipalID  string
	Currency             string
	BusinessPurpose      string
	ProjectCostCenter    string
	PaymentPreferenceRef string

	Status                 ClaimStatus
	RejectionReason        string
	ReturnReason           string
	HasPolicyException     bool
	PolicyExceptionReason  string
	PolicyAssessmentResult PolicyAssessmentResult
	PolicyVersionID        string

	ApprovedByPrincipalID *string
	ApprovedAt            *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

type ExpenseLine struct {
	LineID            string
	TenantID          *string
	ClaimID           string
	Merchant          string
	ExpenseDate       time.Time
	Amount            float64
	Currency          string
	Category          string
	ProjectCostCenter string
	ReceiptDocumentID string // "" means no receipt attached

	ClaimTaxRecovery bool
	Jurisdiction     string
	TaxCategory      string

	TaxDeterminationID  string
	TaxableAmount       float64
	CalculatedTaxAmount float64

	CreatedAt time.Time
}

// ExpenseClaimEvent is an append-only audit trail entry — GetClaimHistory's
// data source.
type ExpenseClaimEvent struct {
	EventID          string
	TenantID         *string
	ClaimID          string
	EventType        string
	Detail           string
	ActorPrincipalID string
	CreatedAt        time.Time
}

const (
	EventClaimCreated             = "EXPENSE_CLAIM_CREATED"
	EventClaimSubmitted           = "EXPENSE_CLAIM_SUBMITTED"
	EventClaimApproved            = "EXPENSE_CLAIM_APPROVED"
	EventClaimRejected            = "EXPENSE_CLAIM_REJECTED"
	EventClaimReturned            = "EXPENSE_CLAIM_RETURNED"
	EventClaimCancelled           = "EXPENSE_CLAIM_CANCELLED"
	EventPolicyExceptionRecorded  = "EXPENSE_CLAIM_POLICY_EXCEPTION_RECORDED"
	EventClaimPayableCreated      = "EXPENSE_CLAIM_PAYABLE_CREATED"
	EventClaimPayableCreateFailed = "EXPENSE_CLAIM_PAYABLE_CREATE_FAILED"
)

// ── request DTOs ────────────────────────────────────────────────────────────

type CreateExpenseClaimRequest struct {
	LegalEntityID        string
	ClaimantPrincipalID  string
	Currency             string
	BusinessPurpose      string
	ProjectCostCenter    string
	PaymentPreferenceRef string
}

type AddExpenseLineRequest struct {
	Merchant          string
	ExpenseDate       time.Time
	Amount            float64
	Currency          string
	Category          string
	ProjectCostCenter string
	ReceiptDocumentID string
	ClaimTaxRecovery  bool
	Jurisdiction      string
	TaxCategory       string
}

type RejectClaimRequest struct {
	Reason string
}

type ReturnClaimRequest struct {
	Reason string
}

type CancelClaimRequest struct {
	Reason string
}

type RecordPolicyExceptionRequest struct {
	Reason string
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrClaimNotFound              = sentinel("expense claim not found")
	ErrInvalidTransition          = sentinel("invalid expense claim state transition")
	ErrLineNotFound               = sentinel("expense line not found")
	ErrClaimantNotEligible        = sentinel("claimant does not exist or is not an active employee")
	ErrClaimantServiceUnavailable = sentinel("employee-master-svc unavailable")
	ErrDuplicateReceipt           = sentinel("receipt document is already attached to another expense line")
	ErrDocumentNotFound           = sentinel("receipt document not found")
	ErrDocumentMismatch           = sentinel("receipt document does not belong to the caller's tenant/legal entity")
	ErrDocumentNotUsable          = sentinel("receipt document is not in a usable state")
	ErrDocumentServiceUnavailable = sentinel("document-vault-svc unavailable")
	ErrMissingRequiredReceipt     = sentinel("one or more expense lines exceed the receipt-required threshold without an attached, verified receipt")
	ErrTaxDeterminationFailed     = sentinel("tax-determination-svc call failed for a line claiming tax recovery")
	ErrPolicyServiceUnavailable   = sentinel("policy-svc unavailable")
	ErrPayableServiceUnavailable  = sentinel("payable-open-item-svc unavailable")
	ErrStoreUnavailable           = sentinel("store unavailable")
)
