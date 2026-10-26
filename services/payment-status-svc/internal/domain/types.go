// Package domain defines the authoritative domain types for
// payment-status-svc — BNK-07 of the Banking, Cash & Treasury baseline.
// Its job per spec: "Resolve and preserve canonical provider/network
// payment execution state and finality from authenticated callbacks,
// polls, bank reports and statement evidence."
//
// UNLIKE BNK-06, this service's central capability IS fully real: a
// provider webhook receiver with genuine HMAC-SHA256 signature
// verification over the raw request body. There is no real bank/PSP on
// the other end sending these callbacks in this environment, but the
// RECEIVING side's security, ordering and idempotency logic is exactly
// what a real integration with a real provider (Stripe, Adyen, a bank's
// own webhook feed — all of which use this same signed-payload pattern)
// would need, and none of it is faked: an invalid signature is genuinely
// rejected, a genuinely duplicated event is genuinely a no-op, and a
// genuinely out-of-order status genuinely cannot regress a settled
// payment. Swapping in a real provider means pointing its webhook
// configuration at this endpoint and sharing its real signing secret;
// nothing about this service's own logic would need to change.
//
// What IS real, not fabricated:
//
//   - ProcessProviderCallback verifies an HMAC-SHA256 signature (the
//     X-Webhook-Signature header, computed over the exact raw request
//     body) against a configured shared secret before touching anything —
//     the literal fix for negative-path #1 ("forged callback accepted").
//   - Negative-path #2 ("out-of-order status regresses settled payment"):
//     once a payment reaches a governed final state (SETTLED, REJECTED,
//     CANCELLED), no callback or statement link can ever change its status
//     again — only the distinct, explicit RecordReturn command can move a
//     SETTLED payment to RETURNED, matching the spec's own words
//     ("governed final states cannot regress without explicit
//     return/reversal semantics").
//   - Negative-path #3 ("duplicate callback creates duplicate accounting
//     effect") is enforced by a genuine database uniqueness constraint on
//     (payment_id, provider_event_ref) — a repeat callback for the same
//     real-world event is idempotent, never double-applied.
//   - Negative-path #4 ("provider says settled but statement evidence
//     conflicts and is ignored"): LinkStatementConfirmation compares its
//     caller-supplied statement status against the current canonical
//     status; a mismatch raises a real STATUS_CONFLICT record and blocks
//     the update rather than silently overwriting either side — only
//     ResolveStatusConflict (an operator, exceptional-workflow command,
//     matching the SoD line "manual finality override requires exceptional
//     controlled workflow and evidence") can close it.
//
// Honest gap: PollPaymentStatus has no real provider to poll (same reason
// as BNK-06's Provider Adapter boundary — no real bank/PSP integration
// exists in this codebase), so it is a caller-attested command, not a real
// outbound poll — the same "record a real external fact a human observed"
// doctrine used throughout this session (AP-01's high-risk-change
// approvals, AP-11's ReconcilePaymentRunStatus).
package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type ExecutionStatus string

const (
	StatusPrepared  ExecutionStatus = "PREPARED"
	StatusSubmitted ExecutionStatus = "SUBMITTED"
	StatusAccepted  ExecutionStatus = "ACCEPTED"
	StatusPending   ExecutionStatus = "PENDING"
	StatusSettled   ExecutionStatus = "SETTLED"
	StatusRejected  ExecutionStatus = "REJECTED"
	StatusReturned  ExecutionStatus = "RETURNED"
	StatusCancelled ExecutionStatus = "CANCELLED"
)

// finalStates are governed final states — see package doc. Once reached,
// only RecordReturn (from SETTLED specifically) may transition further.
var finalStates = map[ExecutionStatus]bool{
	StatusSettled: true, StatusRejected: true, StatusCancelled: true, StatusReturned: true,
}

// IsFinal reports whether s is a governed final state that ordinary
// callbacks/statement links can never move past.
func IsFinal(s ExecutionStatus) bool { return finalStates[s] }

func ValidCallbackStatus(s ExecutionStatus) bool {
	switch s {
	case StatusAccepted, StatusPending, StatusSettled, StatusRejected:
		return true
	default:
		return false
	}
}

func CanCancel(s ExecutionStatus) bool { return s == StatusPrepared || s == StatusSubmitted }
func CanReturn(s ExecutionStatus) bool { return s == StatusSettled }

// CanConfirmFinality reports whether a manual finality command may act on s.
// Both ConfirmSettlement and RecordRejection are evidence-bearing decisions
// from any not-yet-governed state; a SETTLED state can still move to RETURNED
// through RecordReturn, but no final state may be re-confirmed. Keeping this
// predicate in the domain means a future PENDING_UNKNOWN state (A4) is accepted
// here without another handler change.
func CanConfirmFinality(s ExecutionStatus) bool { return !IsFinal(s) }

// ValidResolutionStatus reports whether s is a status an operator may set
// when closing a provider/statement conflict. A conflict is resolved to a
// governed final outcome, not to an intermediate stage; RETURNED is excluded
// because it has its own command (RecordReturn) and its own preconditions.
func ValidResolutionStatus(s ExecutionStatus) bool {
	switch s {
	case StatusSettled, StatusRejected, StatusCancelled:
		return true
	default:
		return false
	}
}

type PaymentExecutionState struct {
	PaymentID         string
	TenantID          *string
	LegalEntityID     string
	ProviderRequestID string // correlates to BNK-06's PaymentInitiationAttempt
	SourceReference   string
	// ProviderKey is the certified provider profile that owns this payment.
	// It is optional at RecordPaymentStatus (a caller may not know it yet) and
	// is bound by the first applied callback; once set, a callback from a
	// different provider is refused rather than allowed to mutate the state.
	ProviderKey string

	Status ExecutionStatus

	FinalitySource string // "PROVIDER_CALLBACK" | "STATEMENT" | "MANUAL_POLL" | "MANUAL_OVERRIDE" | "MANUAL_CONFIRMATION" | "MANUAL_REJECTION"
	MappingVersion string

	HasOpenConflict bool
	ConflictReason  string

	CreatedByPrincipalID string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// StatusEvent is an append-only status-history entry — every real change,
// every rejected duplicate, and every raised conflict is recorded here,
// never just overwritten in place.
type StatusEvent struct {
	EventID          string
	TenantID         *string
	PaymentID        string
	EventType        string
	FromStatus       string
	ToStatus         string
	ProviderEventRef string
	Detail           string
	ActorPrincipalID string
	CreatedAt        time.Time
}

const (
	EventPaymentAccepted               = "PAYMENT_ACCEPTED"
	EventPaymentPending                = "PAYMENT_PENDING"
	EventPaymentSettled                = "PAYMENT_SETTLED"
	EventPaymentRejected               = "PAYMENT_REJECTED"
	EventPaymentReturned               = "PAYMENT_RETURNED"
	EventPaymentCancelled              = "PAYMENT_CANCELLED"
	EventPaymentStatusConflictRaised   = "PAYMENT_STATUS_CONFLICT_RAISED"
	EventPaymentStatusConflictResolved = "PAYMENT_STATUS_CONFLICT_RESOLVED"
	EventCallbackRejectedForgery       = "PAYMENT_CALLBACK_REJECTED_FORGERY"
	EventCallbackRejectedRegression    = "PAYMENT_CALLBACK_REJECTED_REGRESSION"
	EventCallbackDuplicateIgnored      = "PAYMENT_CALLBACK_DUPLICATE_IGNORED"
)

// Manual finality command identities, persisted with their evidence and used
// as the operation discriminator in the command-idempotency record. They are
// deliberately distinct: reusing one Idempotency-Key for a settlement and a
// rejection is a conflict, not a replay.
const (
	CommandConfirmSettlement = "CONFIRM_SETTLEMENT"
	CommandRecordRejection   = "RECORD_REJECTION"

	FinalitySourceManualConfirmation = "MANUAL_CONFIRMATION"
	FinalitySourceManualRejection    = "MANUAL_REJECTION"

	ManualConfirmationMappingVersion = "payment-status-svc.confirm-settlement.v1"
	ManualRejectionMappingVersion    = "payment-status-svc.record-rejection.v1"
)

// ── request DTOs ────────────────────────────────────────────────────────────

type RecordPaymentStatusRequest struct {
	LegalEntityID     string
	ProviderRequestID string
	SourceReference   string
	// ProviderKey optionally binds the execution state to the certified
	// provider profile BNK-06 used. Empty leaves it unbound; the first
	// applied callback then binds it.
	ProviderKey string
}

// ProviderCallbackPayload is the parsed body of a real webhook call — its
// authenticity is verified (HMAC over the raw bytes) BEFORE this is ever
// parsed or acted on.
type ProviderCallbackPayload struct {
	PaymentID        string
	ProviderEventRef string
	ReportedStatus   ExecutionStatus
	MappingVersion   string
}

type LinkStatementRequest struct {
	StatementReference string          `json:"statement_reference"`
	ReportedStatus     ExecutionStatus `json:"reported_status"`

	// BNK-05 correlation fields — carried through to the conflict event so
	// bank-reconciliation-svc can correlate without fuzzy matching.
	StatementLineID   string `json:"statement_line_id,omitempty"`   // bank-reconciliation-svc statement_line_id
	ProviderRequestID string `json:"provider_request_id,omitempty"` // BNK-06 provider_request_id
	BankRecStatus     string `json:"bank_rec_status,omitempty"`     // bank-rec status at time of link
}

type ResolveConflictRequest struct {
	FinalStatus ExecutionStatus
	Reason      string
}

type RecordReturnRequest struct {
	ProviderEventRef string
	Reason           string
}

type CancelRequest struct {
	Reason string
}

// ConfirmSettlementRequest is the first-class manual settlement command. BNK-07
// §7 names the command and its contract fields (provider/bank references,
// confirmation evidence, effective timestamps, finality basis) but does not
// prescribe a per-command JSON schema; these fields are the minimum needed to
// record that evidence. At least one of provider_reference and bank_reference
// is required, as is an evidence reference. effective_date accepts RFC3339 or
// YYYY-MM-DD.
type ConfirmSettlementRequest struct {
	ProviderReference string `json:"provider_reference"`
	BankReference     string `json:"bank_reference"`
	Reason            string `json:"reason"`
	EffectiveDate     string `json:"effective_date"`
	EvidenceReference string `json:"evidence_reference"`
	EvidenceHash      string `json:"evidence_hash,omitempty"`
}

func (r ConfirmSettlementRequest) Validate() error {
	return validateFinalityRequest(r.ProviderReference, r.BankReference, r.Reason, r.EffectiveDate, r.EvidenceReference, r.EvidenceHash)
}

// RecordRejectionRequest is the rejection counterpart. A rejection always needs
// a reason as well as the same provider/bank and evidence fields.
type RecordRejectionRequest struct {
	ProviderReference string `json:"provider_reference"`
	BankReference     string `json:"bank_reference"`
	Reason            string `json:"reason"`
	EffectiveDate     string `json:"effective_date"`
	EvidenceReference string `json:"evidence_reference"`
	EvidenceHash      string `json:"evidence_hash,omitempty"`
}

func (r RecordRejectionRequest) Validate() error {
	return validateFinalityRequest(r.ProviderReference, r.BankReference, r.Reason, r.EffectiveDate, r.EvidenceReference, r.EvidenceHash)
}

// FinalityEvidence is the append-only record written by ConfirmSettlement or
// RecordRejection in the same transaction as the final status change and its
// outbox event. It is returned by the command and by GetFinalityEvidence.
type FinalityEvidence struct {
	EvidenceID        string    `json:"evidence_id"`
	TenantID          *string   `json:"tenant_id,omitempty"`
	PaymentID         string    `json:"payment_id"`
	Command           string    `json:"command"`
	ProviderReference string    `json:"provider_reference,omitempty"`
	BankReference     string    `json:"bank_reference,omitempty"`
	Reason            string    `json:"reason"`
	EffectiveAt       time.Time `json:"effective_at"`
	EvidenceReference string    `json:"evidence_reference"`
	EvidenceHash      string    `json:"evidence_hash,omitempty"`
	MappingVersion    string    `json:"mapping_version"`
	FinalitySource    string    `json:"finality_source"`
	ActorPrincipalID  string    `json:"actor_principal_id"`
	IdempotencyKey    string    `json:"idempotency_key,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// ParseEffectiveDate accepts the two forms an external evidence feed commonly
// supplies: a full RFC3339 timestamp, or a date-only value (interpreted as UTC
// midnight).
func ParseEffectiveDate(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fmt.Errorf("%w: effective_date is required", ErrInvalidFinalityRequest)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%w: effective_date must be RFC3339 or YYYY-MM-DD", ErrInvalidFinalityRequest)
}

func validateFinalityRequest(providerReference, bankReference, reason, effectiveDate, evidenceReference, evidenceHash string) error {
	providerReference = strings.TrimSpace(providerReference)
	bankReference = strings.TrimSpace(bankReference)
	reason = strings.TrimSpace(reason)
	evidenceReference = strings.TrimSpace(evidenceReference)
	if providerReference == "" && bankReference == "" {
		return fmt.Errorf("%w: provider_reference or bank_reference is required", ErrInvalidFinalityRequest)
	}
	if len(providerReference) > 256 || len(bankReference) > 256 {
		return fmt.Errorf("%w: provider_reference and bank_reference must be at most 256 characters", ErrInvalidFinalityRequest)
	}
	if reason == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidFinalityRequest)
	}
	if len(reason) > 2000 {
		return fmt.Errorf("%w: reason must be at most 2000 characters", ErrInvalidFinalityRequest)
	}
	if _, err := ParseEffectiveDate(effectiveDate); err != nil {
		return err
	}
	if evidenceReference == "" {
		return fmt.Errorf("%w: evidence_reference is required", ErrInvalidFinalityRequest)
	}
	if len(evidenceReference) > 512 {
		return fmt.Errorf("%w: evidence_reference must be at most 512 characters", ErrInvalidFinalityRequest)
	}
	h := strings.TrimSpace(evidenceHash)
	if h != "" {
		if len(h) != 64 {
			return fmt.Errorf("%w: evidence_hash must be a 64-character SHA-256 hex digest", ErrInvalidFinalityRequest)
		}
		if _, err := hex.DecodeString(h); err != nil {
			return fmt.Errorf("%w: evidence_hash must be hexadecimal", ErrInvalidFinalityRequest)
		}
	}
	return nil
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrPaymentNotFound       = sentinel("payment execution state not found")
	ErrInvalidTransition     = sentinel("invalid payment execution state transition")
	ErrInvalidSignature      = sentinel("webhook signature is invalid or missing")
	ErrInvalidCallbackStatus = sentinel("reported_status must be ACCEPTED, PENDING, SETTLED, or REJECTED")
	ErrInvalidResolution     = sentinel("final_status must be SETTLED, REJECTED or CANCELLED")
	ErrOpenConflictExists    = sentinel("payment already has an open, unresolved status conflict")
	ErrStoreUnavailable      = sentinel("store unavailable")
	// ErrInvalidFinalityRequest is the evidence/contract validation for
	// ConfirmSettlement and RecordRejection.
	ErrInvalidFinalityRequest = sentinel("invalid finality command request")
	// ErrIdempotencyKeyRequired/Conflict are the same command-idempotency
	// rules consolidation-svc and the platform's command_idempotency pattern
	// use: the key is claimed in the transaction that makes the change, a
	// repeat with the same request replays the result, and the same key for a
	// different command, payment or request is refused.
	ErrIdempotencyKeyRequired = sentinel("Idempotency-Key header is required for this command")
	ErrIdempotencyKeyConflict = sentinel("Idempotency-Key was already used with a different request")
	// ErrSelfResolutionForbidden is the SoD rule the spec names as "manual
	// finality override requires exceptional controlled workflow and
	// evidence": the principal who created a payment cannot also be the
	// one who manually settles/resolves its finality. Role-permission
	// separation (PaymentFinalityConfirm vs the ingest actions) is real
	// but insufficient alone — a principal holding both roles could
	// otherwise create then resolve the same payment themselves.
	ErrSelfResolutionForbidden = sentinel("the principal who created this payment cannot also resolve its finality")
)
