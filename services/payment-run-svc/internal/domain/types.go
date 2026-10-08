// Package domain defines the authoritative domain types for
// payment-run-svc — AP-11 of the Procurement, Expenses & Accounts Payable
// baseline. Its job per spec: "Orchestrate authorized payable instructions
// into controlled payment runs and hand them to Banking for external
// initiation/status, while preserving idempotency, external UNKNOWN states
// and payment-clearing/accounting lineage."
//
// How a run flows (ZS-SVC-D-001 §14, invariants #16–#19):
//
//   - CreateRun reads each AP-10 authorization and its frozen AP-09
//     proposal, and creates ONE INSTRUCTION PER PAYEE with that payee's own
//     net total and the payables it settles. The proposal must be the exact
//     subject AP-10 approved (FROZEN, same fingerprint, same net total), and
//     the run may not change any authorized field (paying account,
//     currency, method, value date). Cross-tenant/cross-entity
//     authorizations are refused (negative-path #4).
//   - LockPaymentRun live-validates, then consumes each distinct
//     authorization exactly once; a consumption failure moves the run to
//     EXCEPTION before anything is sent.
//   - SubmitPaymentRun binds the run's idempotency key, then hands each
//     instruction to Banking: BNK-06 PrepareAttempt (attempt id persisted
//     BEFORE submit), SubmitAttempt, then BNK-07 RecordPaymentStatus. A
//     timeout or lost response makes the instruction PENDING_UNKNOWN —
//     never failed, never re-initiated (invariant #18, negative-paths
//     #32/#33). A replay with the same key resumes without re-sending
//     (negative-path #1).
//   - PollInstructionStatus is the only path to ACCEPTED/REJECTED/SETTLED:
//     it reads BNK-06/BNK-07's own records, idempotently per real-world
//     status (negative-path #2). A SETTLED instruction then settles its
//     AP-08 payables (net + withholding), retried on later polls until
//     AP-08 accepts them. Nothing is settled from an initiation response
//     (negative-path #3, invariant #19).
//   - RetrySafeInstruction re-sends a PENDING_UNKNOWN instruction through
//     BNK-06 RetrySameAttempt — same attempt, same key.
//   - ReconcilePaymentRunStatus (manual) can only flag EXCEPTION with a
//     reason and evidence reference.
//
// Remaining boundary: BNK-06's own Provider Adapter is a documented stub
// (see payment-initiation-adapter-svc), so no real bank/PSP call happens
// yet; AP-11 does not yet emit payment-clearing accounting events.
//
// One more scope note: this service's own SoD line ("run operator cannot
// alter authorized fields; unauthorized re-initiation prohibited") is a
// data-immutability and idempotency requirement, not a maker/checker
// person-pair rule — unlike AP-01/AP-04/AP-07/AP-09/AP-10, this service
// does not call authorization-svc's dynamic own-object SoD layer, because
// the spec itself doesn't ask for one here. Not every service needs it;
// forcing a sixth reuse where the spec doesn't call for one would be the
// same kind of fabrication this whole doctrine exists to avoid.
package domain

import "time"

type RunStatus string

const (
	StatusDraft             RunStatus = "DRAFT"
	StatusValidated         RunStatus = "VALIDATED"
	StatusLocked            RunStatus = "LOCKED"
	StatusSubmitted         RunStatus = "SUBMITTED"
	StatusPendingUnknown    RunStatus = "PENDING_UNKNOWN"
	StatusAccepted          RunStatus = "ACCEPTED"
	StatusRejected          RunStatus = "REJECTED"
	StatusPartiallyAccepted RunStatus = "PARTIALLY_ACCEPTED"
	StatusSettled           RunStatus = "SETTLED"
	StatusCompleted         RunStatus = "COMPLETED"
	StatusException         RunStatus = "EXCEPTION"
	StatusCancelled         RunStatus = "CANCELLED"
)

func CanValidate(s RunStatus) bool { return s == StatusDraft }
func CanLock(s RunStatus) bool     { return s == StatusValidated }
func CanSubmit(s RunStatus) bool   { return s == StatusLocked }
func CanCancel(s RunStatus) bool   { return s == StatusDraft || s == StatusValidated }

// CanResumeSubmit covers a SubmitPaymentRun replay with the same
// idempotency key: instructions not yet handed to Banking are handed off,
// already-handed-off ones are skipped.
func CanResumeSubmit(s RunStatus) bool {
	return s == StatusLocked || s == StatusSubmitted || s == StatusPendingUnknown
}

// CanReconcile includes EXCEPTION: one instruction failing must not strand
// the run's other instructions, which may already be at the bank.
func CanReconcile(s RunStatus) bool {
	return s == StatusSubmitted || s == StatusPendingUnknown || s == StatusAccepted ||
		s == StatusPartiallyAccepted || s == StatusException
}
func CanRetry(s RunStatus) bool {
	return s == StatusSubmitted || s == StatusPendingUnknown || s == StatusException
}
func CanClose(s RunStatus) bool {
	return s == StatusSettled || s == StatusRejected || s == StatusPartiallyAccepted || s == StatusException
}

type InstructionStatus string

const (
	InstructionPending InstructionStatus = "PENDING"
	// InstructionPendingUnknown: Banking was called but gave no
	// authoritative answer (timeout, lost response, BNK-06 PENDING_UNKNOWN).
	// The payment may or may not be at the bank. It is never treated as
	// failed and never re-initiated; it is resolved only by polling BNK-06/
	// BNK-07 or by RetrySafeInstruction re-using the same BNK-06 attempt.
	InstructionPendingUnknown InstructionStatus = "PENDING_UNKNOWN"
	InstructionAccepted       InstructionStatus = "ACCEPTED"
	InstructionRejected       InstructionStatus = "REJECTED"
	InstructionSettled        InstructionStatus = "SETTLED"
	InstructionException      InstructionStatus = "EXCEPTION"
)

type PaymentRun struct {
	RunID                string
	TenantID             *string
	LegalEntityID        string
	PayingBankAccountRef string
	Currency             string
	ValueDate            time.Time
	PaymentMethod        string

	Status         RunStatus
	IdempotencyKey string

	CreatedByPrincipalID string
	ValidatedAt          *time.Time
	LockedAt             *time.Time
	SubmittedAt          *time.Time
	ClosedAt             *time.Time
	ExceptionReason      string
	CancelReason         string
	CloseNote            string

	CreatedAt time.Time
	UpdatedAt time.Time
}

type RunInstruction struct {
	InstructionID   string
	TenantID        *string
	RunID           string
	AuthorizationID string
	// AuthorizationFingerprint is captured from payment-authorization-svc
	// at CreateRun time (Wave 11a) and carried through to BNK-06's
	// PrepareAttempt for independent re-verification there — never
	// recomputed or trusted blindly by this service in between.
	AuthorizationFingerprint string
	PayeeRef                 string
	NetAmount                float64
	Currency                 string

	Status           InstructionStatus
	StatusReason     string
	ConsumedAt       *time.Time
	ProviderEventRef string

	// ProviderAttemptID correlates to BNK-06's PaymentInitiationAttempt and
	// is recorded right after PrepareAttempt, before anything is sent, so a
	// lost submit response can always be traced to its attempt.
	// Bnk07PaymentID correlates to BNK-07's PaymentExecutionState and is
	// recorded once BNK-06 reports the attempt SUBMITTED. Each is set once
	// and immutable afterward — enforced by a database trigger.
	ProviderAttemptID string
	Bnk07PaymentID    string

	// Payables are the AP-08 payables this instruction settles, frozen from
	// the authorized AP-09 proposal items at CreateRun.
	Payables []InstructionPayable `json:",omitempty"`

	CreatedAt time.Time
}

// InstructionPayable is one authorized AP-09 proposal item an instruction
// pays. SourceReference is the AP-08 payable's source reference (invoice or
// claim id), which is what AP-09 records as its PayableID.
type InstructionPayable struct {
	InstructionID     string
	PayableSource     string // AP_INVOICE | EXPENSE_CLAIM
	SourceReference   string
	GrossAmount       float64
	WithholdingAmount float64
	NetAmount         float64
	PayableAppliedAt  *time.Time
}

type RunEvent struct {
	EventID          string
	TenantID         *string
	RunID            string
	EventType        string
	Detail           string
	ActorPrincipalID string
	CreatedAt        time.Time
}

const (
	EventRunCreated          = "PAYMENT_RUN_CREATED"
	EventRunValidated        = "PAYMENT_RUN_VALIDATED"
	EventRunLocked           = "PAYMENT_RUN_LOCKED"
	EventRunSubmitted        = "PAYMENT_RUN_SUBMITTED"
	EventInstructionPending  = "PAYMENT_INSTRUCTION_PENDING"
	EventInstructionUnknown  = "PAYMENT_INSTRUCTION_PENDING_UNKNOWN"
	EventInstructionAccepted = "PAYMENT_INSTRUCTION_ACCEPTED"
	EventInstructionRejected = "PAYMENT_INSTRUCTION_REJECTED"
	EventInstructionSettled  = "PAYMENT_INSTRUCTION_SETTLED"
	EventRunExceptionRaised  = "PAYMENT_RUN_EXCEPTION_RAISED"
	EventRunCompleted        = "PAYMENT_RUN_COMPLETED"
	EventRunCancelled        = "PAYMENT_RUN_CANCELLED"
	EventInstructionRetried  = "PAYMENT_INSTRUCTION_RETRIED"
)

// ── request DTOs ────────────────────────────────────────────────────────────

type CreateRunRequest struct {
	LegalEntityID        string
	PayingBankAccountRef string
	Currency             string
	ValueDate            time.Time
	PaymentMethod        string
	AuthorizationIDs     []string
}

type SubmitRunRequest struct {
	IdempotencyKey string
}

// ReconcileInstructionRequest records one status change on an instruction.
// From the manual ReconcilePaymentRunStatus endpoint only EXCEPTION is
// accepted (with a Reason); every other status comes from Banking.
type ReconcileInstructionRequest struct {
	InstructionID    string
	ExternalStatus   InstructionStatus
	ProviderEventRef string
	Reason           string
}

type CancelRunRequest struct {
	Reason string
}

type CloseRunRequest struct {
	Note string
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrRunNotFound                     = sentinel("payment run not found")
	ErrInvalidTransition               = sentinel("invalid payment run state transition")
	ErrInstructionNotFound             = sentinel("run instruction not found")
	ErrNoAuthorizationIDs              = sentinel("at least one authorization_id is required")
	ErrAuthorizationNotEligible        = sentinel("authorization does not exist, does not belong to this legal entity, or is not APPROVED")
	ErrAuthorizationServiceUnavailable = sentinel("payment-authorization-svc unavailable")
	ErrAuthorizationNoLongerValid      = sentinel("an authorization in this run is no longer valid")
	ErrAuthorizationConsumeFailed      = sentinel("failed to consume one or more authorizations; run moved to EXCEPTION")
	ErrIdempotencyKeyMismatch          = sentinel("run already submitted with a different idempotency key")
	ErrIdempotencyKeyRequired          = sentinel("idempotency_key is required")
	ErrProviderEventAlreadyApplied     = sentinel("this provider event has already been applied")
	ErrStoreUnavailable                = sentinel("store unavailable")
	ErrProviderAdapterUnavailable      = sentinel("payment-initiation-adapter-svc unavailable")
	ErrPaymentStatusUnavailable        = sentinel("payment-status-svc unavailable")
	ErrInstructionNotSubmitted         = sentinel("instruction was never submitted to Banking; nothing to poll")
	ErrInstructionNotRetryable         = sentinel("only a PENDING_UNKNOWN instruction with a Banking attempt can be retried")
	ErrManualStatusNotAllowed          = sentinel("manual reconciliation may only flag EXCEPTION; ACCEPTED/REJECTED/SETTLED come only from Banking")
	ErrReasonRequired                  = sentinel("reason is required")
	ErrProposalServiceUnavailable      = sentinel("payment-proposal-svc unavailable")
	ErrAuthorizedSubjectMismatch       = sentinel("authorized proposal does not match the authorization (status, fingerprint, currency or net total)")
	ErrPayableServiceUnavailable       = sentinel("payable-open-item-svc unavailable")
	// ErrBankingPrepareRejected is BNK-06 refusing PrepareAttempt with a 4xx
	// (fingerprint mismatch, payer account not eligible, bad request).
	// Nothing was sent to the bank.
	ErrBankingPrepareRejected = sentinel("payment-initiation-adapter-svc refused to prepare the attempt")
	// ErrBankingAttemptConflict is BNK-06 answering 409 to a submit/retry:
	// the attempt is not in the state the call expected, typically because
	// an earlier call reached it. The caller must re-read the attempt.
	ErrBankingAttemptConflict = sentinel("payment-initiation-adapter-svc attempt is not in the expected state")
)
