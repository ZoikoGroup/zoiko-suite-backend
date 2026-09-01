// Package domain defines the authoritative domain types for
// supplier-recovery-svc — AP-12 of the Procurement, Expenses & Accounts
// Payable baseline, the last of AP-01 through AP-12. Its job per spec:
// "Govern recovery of supplier overpayments, duplicate payments, supplier
// credits and contractual amounts through confirmed inbound refund or
// approved offset, preserving source linkage, legal/tax treatment and
// accounting reconciliation."
//
// This closes the last remaining named dependency in the domain:
// payable-open-item-svc's (AP-08) own ApplyRecovery command has had no real
// caller since AP-08 was built — this service is that caller.
//
// What IS real, not fabricated:
//   - ApplyApprovedOffset is the first real caller of AP-08's
//     ApplyRecovery anywhere in this codebase — an approved offset here
//     genuinely reduces the source payable's residual in AP-08, not just a
//     locally-recorded intention.
//   - LinkConfirmedSupplierRefund is the literal enforcement of
//     negative-path #1 ("supplier refund marked received before bank
//     confirmation"): it calls bank-reconciliation-svc's real
//     GetStatementLine and requires that statement line to already be
//     MATCHED — the only real "a bank event genuinely happened" fact
//     available anywhere in this codebase for an inbound receipt. A
//     line that is still UNMATCHED or in EXCEPTION is refused outright,
//     never treated as good enough.
//   - Negative-path #3 ("recovery write-off self-approved") and the
//     spec's own SoD line ("case owner cannot self-approve material
//     offset/write-off") are enforced via authorization-svc's dynamic
//     own-object SoD layer — reused here because the spec explicitly
//     names self-approval as the exact scenario to block, unlike AP-09/
//     AP-11's own deliberate non-reuse where the spec didn't call for it.
//   - Negative-path #4 ("recovery closes while AP/GL/bank difference
//     remains") is enforced structurally: CloseRecoveryCase is only
//     reachable once RecoveredAmount has reached TotalAmount exactly
//     (state RECOVERED), computed server-side from the same
//     RecoveryApplication ledger every offset/refund is recorded in —
//     never from a caller-supplied "yes it's fully recovered" flag.
//   - Negative-path #2 ("offset applied without legal/policy authority")
//     is enforced by requiring RECOVERY_OFFSET_APPROVE authority (checked
//     via authorization-svc) in addition to the maker-checker SoD gate —
//     an offset is never appliable on ordinary RECOVERY_MANAGE authority
//     alone.
//
// Honest gaps, not fabricated:
//   - The spec names TAX and Legal/Contract as dependencies for recovery
//     basis/treatment — no tax-determination-svc call is made here because
//     the spec gives AP-12 no analogue of AP-07/AP-09's real withholding/
//     reclaim call shape (no request contract naming a recovery-specific
//     tax operation). RecoveryReason and any legal/tax basis are
//     caller-supplied evidence fields, not independently verified — the
//     same "record a real fact a human observed" doctrine used throughout
//     this session.
//   - RecordSupplierCommitment is entirely caller-attested: no supplier
//     communication channel exists anywhere in this codebase to verify a
//     supplier's own commitment against.
package domain

import "time"

type CaseStatus string

const (
	StatusOpen               CaseStatus = "OPEN"
	StatusApproved           CaseStatus = "APPROVED"
	StatusInRecovery         CaseStatus = "IN_RECOVERY"
	StatusPartiallyRecovered CaseStatus = "PARTIALLY_RECOVERED"
	StatusRecovered          CaseStatus = "RECOVERED"
	StatusClosed             CaseStatus = "CLOSED"
	StatusEscalated          CaseStatus = "ESCALATED"
	StatusWrittenOff         CaseStatus = "WRITTEN_OFF"
)

// CanApprove reports whether a case in status s may have its recovery plan
// approved. ApproveRecoveryPlan moves a case directly to IN_RECOVERY, not
// the separately-named APPROVED — the spec's own state model has no
// command that would ever leave a case sitting in APPROVED with nothing
// recovered yet, so this collapses the two exactly like AP-08's own
// CreatePayableFromApprovedSource landing directly in OPEN. APPROVED is
// kept in the status CHECK constraint for forward compatibility only.
func CanApprove(s CaseStatus) bool { return s == StatusOpen }

// activeRecoveryStatuses are the statuses in which a case still has real,
// unresolved recovery capacity — an offset or refund may be applied, the
// case may be escalated, or (from here or ESCALATED) written off.
var activeRecoveryStatuses = map[CaseStatus]bool{
	StatusApproved: true, StatusInRecovery: true, StatusPartiallyRecovered: true,
}

// CanApplyRecovery reports whether a case in status s may accept a further
// offset/refund application.
func CanApplyRecovery(s CaseStatus) bool { return activeRecoveryStatuses[s] }

// CanEscalate reports whether a case in status s may be escalated.
func CanEscalate(s CaseStatus) bool { return s == StatusOpen || activeRecoveryStatuses[s] }

// CanWriteOff reports whether a case in status s may be written off.
func CanWriteOff(s CaseStatus) bool {
	return s == StatusOpen || activeRecoveryStatuses[s] || s == StatusEscalated
}

// CanClose is the literal enforcement of negative-path #4 — a case may
// only close once fully recovered, never while any AP/GL/bank difference
// remains.
func CanClose(s CaseStatus) bool { return s == StatusRecovered }

type RecoveryBasis string

const (
	BasisOverpayment      RecoveryBasis = "OVERPAYMENT"
	BasisDuplicatePayment RecoveryBasis = "DUPLICATE_PAYMENT"
	BasisSupplierCredit   RecoveryBasis = "SUPPLIER_CREDIT"
	BasisContractual      RecoveryBasis = "CONTRACTUAL"
)

type SupplierRecoveryCase struct {
	CaseID          string
	TenantID        *string
	LegalEntityID   string
	SupplierRef     string
	RecoveryBasis   RecoveryBasis
	SourcePayableID string // the AP-08 payable this recovery is sourced from
	TotalAmount     float64
	RecoveredAmount float64
	Currency        string
	RecoveryReason  string

	Status CaseStatus

	EscalationReason string
	WriteOffReason   string
	CloseNote        string

	CreatedByPrincipalID  string
	ApprovedByPrincipalID string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// RecoveryApplication is an append-only record of every real recovery
// event applied to a case — an approved offset against the source AP-08
// payable, or a bank-confirmed inbound refund — each carrying its own
// idempotency reference. Mirrors payable-open-item-svc's own
// SettlementApplication ledger discipline exactly.
type RecoveryApplication struct {
	ApplicationID    string
	TenantID         *string
	CaseID           string
	ApplicationType  string // OFFSET | REFUND
	Amount           float64
	IdempotencyRef   string // AP-08 recovery ref (OFFSET) or bank statement_line_id (REFUND)
	Detail           string
	ActorPrincipalID string
	CreatedAt        time.Time
}

type RecoveryCommitment struct {
	CommitmentID     string
	TenantID         *string
	CaseID           string
	Detail           string
	ExpectedMethod   string
	ActorPrincipalID string
	CreatedAt        time.Time
}

const (
	EventRecoveryCaseCreated     = "SUPPLIER_RECOVERY_CASE_CREATED"
	EventRecoveryPlanApproved    = "SUPPLIER_RECOVERY_PLAN_APPROVED"
	EventSupplierRefundConfirmed = "SUPPLIER_REFUND_CONFIRMED"
	EventRecoveryOffsetApplied   = "SUPPLIER_RECOVERY_OFFSET_APPLIED"
	EventRecoveryEscalated       = "SUPPLIER_RECOVERY_ESCALATED"
	EventRecoveryClosed          = "SUPPLIER_RECOVERY_CLOSED"
	EventRecoveryWrittenOff      = "SUPPLIER_RECOVERY_WRITTEN_OFF"
	EventCommitmentRecorded      = "SUPPLIER_RECOVERY_COMMITMENT_RECORDED"
)

// ── request DTOs ────────────────────────────────────────────────────────────

type CreateCaseRequest struct {
	LegalEntityID   string
	SupplierRef     string
	RecoveryBasis   RecoveryBasis
	SourcePayableID string
	TotalAmount     float64
	Currency        string
	RecoveryReason  string
}

type RecordCommitmentRequest struct {
	Detail         string
	ExpectedMethod string
}

type ApplyOffsetRequest struct {
	Amount      float64
	RecoveryRef string // this case's own idempotency reference, also passed through to AP-08
}

type LinkRefundRequest struct {
	StatementLineID string
}

type EscalateRequest struct {
	Reason string
}

type CloseCaseRequest struct {
	Note string
}

type WriteOffRequest struct {
	Reason string
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrCaseNotFound                  = sentinel("supplier recovery case not found")
	ErrInvalidTransition             = sentinel("invalid supplier recovery case state transition")
	ErrRecoveryExceedsOutstanding    = sentinel("this application would recover more than the outstanding amount")
	ErrApplicationAlreadyApplied     = sentinel("this recovery application has already been applied")
	ErrPayableServiceUnavailable     = sentinel("payable-open-item-svc unavailable")
	ErrOffsetFailedAtPayable         = sentinel("payable-open-item-svc rejected the offset application")
	ErrBankReconciliationUnavailable = sentinel("bank-reconciliation-svc unavailable")
	ErrStatementLineNotFound         = sentinel("bank statement line not found")
	ErrStatementLineNotConfirmed     = sentinel("bank statement line is not yet MATCHED; refund cannot be marked received before bank confirmation")
	ErrStatementLineMismatch         = sentinel("bank statement line does not belong to this legal entity or is not an inbound amount")
	ErrStoreUnavailable              = sentinel("store unavailable")
)
