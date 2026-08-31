// Package domain defines the authoritative domain types for
// payment-run-svc — AP-11 of the Procurement, Expenses & Accounts Payable
// baseline. Its job per spec: "Orchestrate authorized payable instructions
// into controlled payment runs and hand them to Banking for external
// initiation/status, while preserving idempotency, external UNKNOWN states
// and payment-clearing/accounting lineage."
//
// THE CENTRAL, HONEST GAP OF THIS SERVICE — read before anything else.
// A direct search of this entire codebase (services/*, every banking-
// adjacent service: banking-connector-svc, bank-reconciliation-svc,
// treasury-svc) found no real endpoint anywhere that initiates an outbound
// payment to an external bank/provider, and no real webhook/callback
// receiver for a provider's status response. treasury-svc's own
// /v1/treasury/transfers "InitiateTransfer" only inserts a row and moves
// balances between two of its own internal ledger rows — it never calls
// out to anything external. This is not a field-level gap like AP-01's
// PayeeReference or AP-09's PayingBankAccountRef; it is the literal,
// stated purpose of AP-11 itself. Confirmed directly with the user before
// building this service at all, rather than silently fabricating a fake
// bank integration to look complete.
//
// What that means concretely, command by command:
//
//   - SubmitPaymentRun does not call any bank. It records that submission
//     was attempted (moving LOCKED -> SUBMITTED) and nothing more. There is
//     no real "Pending/Unknown" distinction to make without a real
//     provider response, so this service collapses the spec's
//     Submitted -> Pending/Unknown into one SUBMITTED status.
//   - ReconcilePaymentRunStatus is not a webhook receiver — there is no
//     real callback to receive. It is a command an operator calls with a
//     caller-attested ExternalStatus and ProviderEventRef, the same
//     "record a real external fact a human observed, then enforce the
//     machine-checkable invariant on top of it" doctrine already used for
//     AP-01's high-risk-change approvals and every opaque reference field
//     in this domain. Negative-path scenario #2 ("provider callback
//     forged/replayed") is reinterpreted as its nearest real, buildable
//     equivalent: a repeat call with the same ProviderEventRef against the
//     same instruction is idempotent (a genuine database uniqueness
//     constraint), never double-applied — the closest this service can
//     honestly get to "replay protection" without a real callback to
//     verify a signature on.
//
// What IS real, not fabricated:
//
//   - This is the FIRST real caller of payment-authorization-svc (AP-10)'s
//     ConsumePaymentAuthorization — a gap AP-10 itself documented two
//     services ago ("no real caller yet"). LockPaymentRun calls AP-10's
//     real ValidateAuthorization (live re-check) then ConsumeAuthorization
//     for every instruction in the run, atomically enough that a
//     mid-sequence failure moves the whole run to EXCEPTION rather than
//     silently leaving some authorizations consumed and others not.
//   - Negative-path scenario #4 ("cross-tenant payable included in run") is
//     enforced for real: every authorization named in CreatePaymentRun is
//     fetched live from AP-10 and its LegalEntityID/TenantID must match the
//     run's own, or the whole create is rejected.
//   - Negative-path scenario #1 ("payment run replayed after timeout") is
//     enforced by a real database uniqueness constraint on the run's own
//     idempotency key, captured once at first SubmitPaymentRun and rejected
//     if a later call names a different one.
//   - Negative-path scenario #3 ("run marks payment settled from initiation
//     response") is enforced structurally, not by a runtime check that
//     could be forgotten: SubmitPaymentRun's own method signature has no
//     path to any status beyond SUBMITTED. Only a later,
//     separate ReconcilePaymentRunStatus call can ever move a run to
//     ACCEPTED/REJECTED/PARTIALLY_ACCEPTED/SETTLED — matching the spec's
//     own words, "status reflects BNK authority, not inference."
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
func CanReconcile(s RunStatus) bool {
	return s == StatusSubmitted || s == StatusAccepted || s == StatusPartiallyAccepted
}
func CanRetry(s RunStatus) bool { return s == StatusSubmitted }
func CanClose(s RunStatus) bool {
	return s == StatusSettled || s == StatusRejected || s == StatusPartiallyAccepted || s == StatusException
}

type InstructionStatus string

const (
	InstructionPending   InstructionStatus = "PENDING"
	InstructionAccepted  InstructionStatus = "ACCEPTED"
	InstructionRejected  InstructionStatus = "REJECTED"
	InstructionSettled   InstructionStatus = "SETTLED"
	InstructionException InstructionStatus = "EXCEPTION"
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
	PayeeRef        string
	NetAmount       float64
	Currency        string

	Status           InstructionStatus
	ConsumedAt       *time.Time
	ProviderEventRef string

	CreatedAt time.Time
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

type ReconcileInstructionRequest struct {
	InstructionID    string
	ExternalStatus   InstructionStatus // ACCEPTED, REJECTED, SETTLED, or EXCEPTION
	ProviderEventRef string
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
)
