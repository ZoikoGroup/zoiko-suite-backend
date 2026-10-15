// Package domain defines the authoritative domain types for
// expense-claim-svc — AP-07 of the Procurement, Expenses & Accounts Payable
// baseline. Its job: capture employee/authorized-claimant expenses with
// receipt, business-purpose, policy and tax evidence, creating a
// reimbursement/payable basis, without allowing self-approval or direct
// bank execution.
//
// State model (spec §10): Draft → Submitted → PendingApproval →
// Approved/Rejected/Returned → Reimbursable → Closed, plus Cancelled. The
// transition table below is the single source of truth; the same table is
// enforced in Postgres by a trigger (migration 000004), so a handler bug
// cannot skip a state.
//
//   - SUBMITTED: the claim's evidence is frozen into an immutable,
//     hash-addressed submission snapshot (expense_claim_submissions) and
//     tax determinations are recorded; policy assessment is pending.
//     SubmitExpenseClaim on a SUBMITTED claim resumes routing.
//   - PENDING_APPROVAL: policy assessed, awaiting a decision.
//   - RETURNED: the approver sent it back. The prior submission snapshot is
//     preserved untouched; correction voids/adds lines (lines are never
//     edited or deleted) and the resubmission writes snapshot version N+1.
//   - APPROVED: the approval is final, the payee has not yet been resolved
//     and the AP-08 payable has not yet been created. A durable
//     payable_requests row was written in the same transaction.
//   - REIMBURSABLE: AP-08 holds a payable for this claim (created
//     idempotently by the payable relay with source_reference = claim id).
//   - CLOSED: AP-08 reports the payable SETTLED (observed by the relay, or
//     asserted via CloseExpenseClaim — which still verifies against AP-08).
//
// Real integrations (verified against each peer's code, not assumed):
//
//   - Claimant identity: employee-master-svc GET /v1/employees/{id}
//     (ACTIVE) at create time and again before any payable is requested.
//   - Receipt evidence: document-vault-svc; a partial UNIQUE index on
//     receipt_document_id (excluding voided lines) is the database
//     invariant for "same receipt on two claims".
//   - Tax: tax-determination-svc, per line claiming recovery. A reclaim
//     figure only ever comes from that response; approval re-verifies that
//     every recovery line carries a determination id (TAX_UNAVAILABLE).
//   - Policy: policy-svc APPROVAL_THRESHOLD. "No applicable policy" (404)
//     and service failure FAIL CLOSED for controlled categories
//     (POLICY_CONTROLLED_CATEGORIES, default every category); only a claim
//     made entirely of non-controlled categories may proceed unassessed.
//   - Payee: the reimbursement payee must be an ACTIVE controlled
//     destination in payee-banking-identity-svc (ORG-10) for the claim's
//     payment_preference_ref (or the claimant's own party reference when
//     none was given). With none, the claim is NOT payable: payable_state
//     is BLOCKED with a stable reason and the relay keeps re-checking. The
//     claimant principal id is never silently used as a payee.
//   - AP-08: payable-open-item-svc CreatePayableFromApprovedSource, due
//     date from reimbursement terms (REIMBURSEMENT_TERMS_DAYS, overridable
//     per tenant via configuration-feature-flag-svc), never "now".
//
// Events are written to a transactional outbox (spec names such as
// ExpenseClaimSubmitted, plus the pre-existing EXPENSE_CLAIM_* strings as
// aliases). Approval also emits an accounting.event.requested fact shaped
// like services/_contract/accounting.AccountingEvent — this service never
// writes the ledger.
package domain

import (
	"encoding/json"
	"time"
)

type ClaimStatus string

const (
	StatusDraft           ClaimStatus = "DRAFT"
	StatusSubmitted       ClaimStatus = "SUBMITTED"
	StatusPendingApproval ClaimStatus = "PENDING_APPROVAL"
	StatusApproved        ClaimStatus = "APPROVED"
	StatusRejected        ClaimStatus = "REJECTED"
	StatusReturned        ClaimStatus = "RETURNED"
	StatusReimbursable    ClaimStatus = "REIMBURSABLE"
	StatusClosed          ClaimStatus = "CLOSED"
	StatusCancelled       ClaimStatus = "CANCELLED"
)

// AllStatuses lists every status, for table-driven parity tests.
var AllStatuses = []ClaimStatus{
	StatusDraft, StatusSubmitted, StatusPendingApproval, StatusApproved, StatusRejected,
	StatusReturned, StatusReimbursable, StatusClosed, StatusCancelled,
}

// transitions is the claim state machine. Never mutated at runtime.
var transitions = map[ClaimStatus][]ClaimStatus{
	StatusDraft:           {StatusSubmitted, StatusCancelled},
	StatusSubmitted:       {StatusPendingApproval, StatusCancelled},
	StatusPendingApproval: {StatusApproved, StatusRejected, StatusReturned, StatusCancelled},
	StatusReturned:        {StatusSubmitted, StatusCancelled},
	StatusApproved:        {StatusReimbursable},
	StatusReimbursable:    {StatusClosed},
}

// CanTransition reports whether from → to is a legal state change.
func CanTransition(from, to ClaimStatus) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether no further state change is possible.
func IsTerminal(s ClaimStatus) bool { return len(transitions[s]) == 0 }

// CanSubmit reports whether SubmitExpenseClaim applies: a fresh/returned
// claim is submitted, a SUBMITTED claim has its routing resumed.
func CanSubmit(s ClaimStatus) bool {
	return s == StatusDraft || s == StatusReturned || s == StatusSubmitted
}

// CanDecide reports whether a claim in status s may be approved, rejected,
// returned for correction, or have a policy exception recorded against it.
func CanDecide(s ClaimStatus) bool { return s == StatusPendingApproval }

// CanCancel reports whether a claim in status s may be cancelled.
func CanCancel(s ClaimStatus) bool { return CanTransition(s, StatusCancelled) }

// CanAddLine reports whether a claim in status s may still accept new/
// voided expense lines.
func CanAddLine(s ClaimStatus) bool { return s == StatusDraft || s == StatusReturned }

// CanClose reports whether a claim may be closed (once reimbursable).
func CanClose(s ClaimStatus) bool { return s == StatusReimbursable }

// PolicyAssessmentResult mirrors policy-svc's own APPROVAL_THRESHOLD
// evaluate() result values exactly.
type PolicyAssessmentResult string

const (
	PolicyWithinThreshold  PolicyAssessmentResult = "WITHIN_THRESHOLD"
	PolicyApprovalRequired PolicyAssessmentResult = "APPROVAL_REQUIRED"
	PolicyNotAssessed      PolicyAssessmentResult = "NOT_ASSESSED"
)

// PayableState is the AP-08 hand-off state of an approved claim.
type PayableState string

const (
	PayableNone    PayableState = "NONE"
	PayablePending PayableState = "PENDING"
	// PayableBlocked: the claim is approved but NOT payable (see
	// PayableBlockedReason); the relay keeps re-checking.
	PayableBlocked PayableState = "BLOCKED"
	PayableCreated PayableState = "CREATED"
)

// Stable reasons a payable hand-off is blocked.
const (
	BlockedNoControlledPayee  = "NO_CONTROLLED_PAYEE"
	BlockedClaimantNotActive  = "CLAIMANT_NOT_ACTIVE"
	BlockedPayeeLegalEntityMM = "PAYEE_LEGAL_ENTITY_MISMATCH"
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
	Version                int
	SubmittedVersion       int
	RejectionReason        string
	ReturnReason           string
	HasPolicyException     bool
	PolicyExceptionReason  string
	PolicyAssessmentResult PolicyAssessmentResult
	PolicyVersionID        string

	ApprovedByPrincipalID *string
	ApprovedAt            *time.Time

	PayableState         PayableState
	PayableID            string
	PayableBlockedReason string
	PayeeDestinationID   string

	ClosedAt    *time.Time
	CloseReason string

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

	VoidedAt   *time.Time
	VoidReason string

	CreatedAt time.Time
}

// ActiveLines drops voided lines — the lines that count for totals,
// receipts, tax and snapshots.
func ActiveLines(lines []ExpenseLine) []ExpenseLine {
	out := make([]ExpenseLine, 0, len(lines))
	for _, l := range lines {
		if l.VoidedAt == nil {
			out = append(out, l)
		}
	}
	return out
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

// ExpenseClaimSubmission is one immutable, hash-addressed submission
// version of a claim.
type ExpenseClaimSubmission struct {
	SubmissionID string
	ClaimID      string
	VersionNo    int
	Snapshot     json.RawMessage
	SnapshotHash string
	SubmittedBy  string
	SubmittedAt  time.Time
}

// PayableRequest is the durable AP-08 hand-off record written in the
// approval transaction.
type PayableRequest struct {
	RequestID            string
	TenantID             string
	LegalEntityID        string
	ClaimID              string
	ClaimantPrincipalID  string
	PaymentPreferenceRef string
	RequestedBy          string
	CorrelationID        string
	Amount               float64
	Currency             string
	DueDate              time.Time
	State                PayableState
	BlockedReason        string
	Attempts             int
}

// History event types (stored in expense_claim_events). Each is also
// emitted through the outbox under a spec name (see OutboxTypes).
const (
	EventClaimCreated             = "EXPENSE_CLAIM_CREATED"
	EventClaimSubmitted           = "EXPENSE_CLAIM_SUBMITTED"
	EventClaimRouted              = "EXPENSE_CLAIM_PENDING_APPROVAL"
	EventClaimApproved            = "EXPENSE_CLAIM_APPROVED"
	EventClaimRejected            = "EXPENSE_CLAIM_REJECTED"
	EventClaimReturned            = "EXPENSE_CLAIM_RETURNED"
	EventClaimCancelled           = "EXPENSE_CLAIM_CANCELLED"
	EventClaimClosed              = "EXPENSE_CLAIM_CLOSED"
	EventLineVoided               = "EXPENSE_CLAIM_LINE_VOIDED"
	EventPolicyExceptionRecorded  = "EXPENSE_CLAIM_POLICY_EXCEPTION_RECORDED"
	EventClaimPayableRequested    = "EXPENSE_CLAIM_PAYABLE_REQUESTED"
	EventClaimPayableBlocked      = "EXPENSE_CLAIM_PAYABLE_BLOCKED"
	EventClaimPayableCreated      = "EXPENSE_CLAIM_PAYABLE_CREATED"
	EventClaimPayableCreateFailed = "EXPENSE_CLAIM_PAYABLE_CREATE_FAILED"

	// EventAccountingRequested carries an accounting-contract payload; the
	// Accounting Kernel is the only consumer allowed to post it.
	EventAccountingRequested = "accounting.event.requested"
)

var outboxTypes = map[string]string{
	EventClaimCreated:             "ExpenseClaimCreated",
	EventClaimSubmitted:           "ExpenseClaimSubmitted",
	EventClaimRouted:              "ExpenseClaimPendingApproval",
	EventClaimApproved:            "ExpenseClaimApproved",
	EventClaimRejected:            "ExpenseClaimRejected",
	EventClaimReturned:            "ExpenseClaimReturned",
	EventClaimCancelled:           "ExpenseClaimCancelled",
	EventClaimClosed:              "ExpenseClaimClosed",
	EventLineVoided:               "ExpenseClaimLineVoided",
	EventPolicyExceptionRecorded:  "ExpenseClaimPolicyExceptionApproved",
	EventClaimPayableRequested:    "ExpenseClaimPayableRequested",
	EventClaimPayableBlocked:      "ExpenseClaimPayableBlocked",
	EventClaimPayableCreated:      "ExpenseClaimPayableCreated",
	EventClaimPayableCreateFailed: "ExpenseClaimPayableCreateFailed",
}

// OutboxTypes returns every event type a history event is published as: the
// spec name first, then the pre-existing EXPENSE_CLAIM_* alias. The
// accounting event has a single name.
func OutboxTypes(historyType string) []string {
	if spec, ok := outboxTypes[historyType]; ok {
		return []string{spec, historyType}
	}
	return []string{historyType}
}

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

// DecisionRequest is the body shared by the commands that carry a reason and
// an optimistic version.
type DecisionRequest struct {
	Reason          string `json:"reason"`
	ExpectedVersion *int   `json:"expected_version"`
}

type RejectClaimRequest = DecisionRequest
type ReturnClaimRequest = DecisionRequest
type CancelClaimRequest = DecisionRequest
type RecordPolicyExceptionRequest = DecisionRequest
type CloseClaimRequest = DecisionRequest

// VersionedRequest is the body of commands that carry only expected_version.
type VersionedRequest struct {
	ExpectedVersion *int `json:"expected_version"`
}

type VoidLineRequest struct {
	Reason string `json:"reason"`
}

// ── idempotency ─────────────────────────────────────────────────────────────

// IdemKey identifies one idempotent command invocation.
type IdemKey struct {
	Key         string
	Operation   string
	RequestHash string
}

// IdemRecord is a stored command result.
type IdemRecord struct {
	Operation   string
	RequestHash string
	StatusCode  int
	Body        []byte
}

// CommandParams is the common input of the state-changing store commands.
type CommandParams struct {
	ClaimID         string
	PrincipalID     string
	CorrelationID   string
	Reason          string
	ExpectedVersion *int
	Idem            *IdemKey
}

type RoutingParams struct {
	CommandParams
	PolicyResult    PolicyAssessmentResult
	PolicyVersionID string
}

type ApproveParams struct {
	CommandParams
	// Posting holds the ACC-02 mapping keys for the accounting request.
	Posting PostingConfig
	// DueDate is the reimbursement due date derived from payment terms.
	DueDate time.Time
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrClaimNotFound              = sentinel("expense claim not found")
	ErrInvalidTransition          = sentinel("invalid expense claim state transition")
	ErrStaleVersion               = sentinel("expense claim version changed; reload and retry")
	ErrLineNotFound               = sentinel("expense line not found")
	ErrNoLines                    = sentinel("a claim needs at least one active expense line")
	ErrCurrencyMismatch           = sentinel("expense line currency differs from the claim currency; no implicit conversion")
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
	ErrNoApplicablePolicy         = sentinel("policy-svc has no applicable approval-threshold policy")
	ErrPayableServiceUnavailable  = sentinel("payable-open-item-svc unavailable")
	ErrNoControlledPayee          = sentinel("no active controlled reimbursement payee in payee-banking-identity-svc")
	ErrPayeeServiceUnavailable    = sentinel("payee-banking-identity-svc unavailable")
	ErrIdempotencyConflict        = sentinel("idempotency key already used by a concurrent request")
	ErrStoreUnavailable           = sentinel("store unavailable")
)

// SettlementCandidate is a REIMBURSABLE claim whose AP-08 payable the relay
// polls for settlement.
type SettlementCandidate struct {
	TenantID      string
	ClaimID       string
	LegalEntityID string
	PayableID     string
	PrincipalID   string
}
