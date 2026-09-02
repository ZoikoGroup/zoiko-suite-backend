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
// UPDATE — this gap is now half-closed by wiring to BNK-06/BNK-07 (the two
// real Banking services this package doc originally said didn't exist yet
// as real callers of AP-11). What changed, command by command:
//
//   - SubmitPaymentRun now calls BNK-06's PrepareAttempt+SubmitAttempt for
//     every instruction (internal/provideradapter client), and records the
//     resulting canonical execution record with BNK-07's RecordPaymentStatus
//     (internal/paymentstatus client) before ever transitioning the run
//     itself from LOCKED to SUBMITTED — mirroring LockPaymentRun's own
//     "do the real work first, only then transition; any failure raises
//     EXCEPTION instead of a silent partial state" discipline. This is a
//     REAL handoff to Banking now, not a no-op.
//   - What is still honestly not real: BNK-06's own Provider Adapter behind
//     that call is a documented stub (see payment-initiation-adapter-svc's
//     own package doc) — no actual bank/PSP network call exists anywhere in
//     this codebase. Wiring AP-11 to BNK-06/BNK-07 moves the one remaining
//     honest gap down to exactly that boundary, and no further.
//   - PollInstructionStatus (new) queries BNK-07's real canonical
//     GetPaymentStatus for the instruction's own bnk07_payment_id and
//     reconciles the run from that real answer — this is what
//     ReconcilePaymentRunStatus used to have to fake with a caller-attested
//     ExternalStatus. ReconcilePaymentRunStatus itself is kept as a
//     deliberate, separate manual-override path (an operator recording a
//     real external fact some other way BNK-07 didn't capture) rather than
//     removed — the same "record a real external fact a human observed"
//     doctrine used throughout this session. Negative-path scenario #2
//     ("provider callback forged/replayed") is enforced identically either
//     way: a repeat call with the same ProviderEventRef against the same
//     instruction is idempotent (a genuine database uniqueness constraint),
//     never double-applied.
//   - PayerAccountVerified is asserted true when calling BNK-06 — AP-09/
//     AP-10 already treat the paying bank account reference as opaque (no
//     real account-status source exists anywhere in this codebase, exactly
//     as BNK-06's own package doc says), so this is the same caller-
//     attestation gate BNK-06 itself defines, not a new fabrication.
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

	// ProviderAttemptID correlates to BNK-06's PaymentInitiationAttempt;
	// Bnk07PaymentID correlates to BNK-07's PaymentExecutionState. Both are
	// set exactly once, at first real submission to Banking (SubmitPaymentRun),
	// and are immutable afterward — enforced by a database trigger.
	ProviderAttemptID string
	Bnk07PaymentID    string

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
	ErrProviderAdapterUnavailable      = sentinel("payment-initiation-adapter-svc unavailable")
	ErrPaymentStatusUnavailable        = sentinel("payment-status-svc unavailable")
	ErrInstructionNotSubmitted         = sentinel("instruction was never submitted to Banking; nothing to poll")
)
