// Package domain defines the authoritative domain types for
// payment-initiation-adapter-svc — BNK-06 of the Banking, Cash & Treasury
// baseline. Its job per spec: "Transmit an already approved payment
// instruction to a bank/PSP through a durable, idempotent external attempt
// while preserving exact authorization subject and provider evidence."
//
// THE PROVIDER ADAPTER BOUNDARY — read before anything else. The Banking
// spec itself names "Provider Adapter" as a dependency of BNK-06 but never
// gives it its own numbered section or contract anywhere in the spec set —
// it is the genuinely pluggable point where a real bank/PSP integration
// (a specific bank's API, a SWIFT gateway, a payment processor's SDK,
// real credentials) would be implemented. This service defines that
// boundary as a real Go interface (ProviderAdapter) and ships a real,
// working, clearly-labeled StubProviderAdapter behind it — deterministic,
// simulates the full range of real outcomes (accepted / timeout /
// rejected), and is never disguised as a real bank connection. Swapping in
// a real provider means implementing this same interface against a real
// API; nothing else in this service would need to change. This is not a
// shortcut — separating orchestration/durability from the provider-specific
// wire protocol is how real payment-initiation systems are actually built.
//
// What IS real, not fabricated, on this side of that boundary:
//
//   - Durable-attempt-before-network-call is enforced by construction:
//     PrepareAttempt persists a PREPARED row BEFORE SubmitAttempt is ever
//     called, and SubmitAttempt requires an existing PREPARED row — there
//     is no code path that calls the adapter without a durable record
//     already committed. This is the literal fix for the spec's own named
//     negative-path scenario ("attempt record created after external
//     call").
//   - "Provider timeout triggers new payment ID" is structurally
//     impossible: RetrySameAttempt re-submits the SAME AttemptID/
//     IdempotencyKey row — there is no code path that creates a second
//     attempt for the same payment intent. A timeout moves the attempt to
//     PENDING_UNKNOWN, never REJECTED, matching "UNKNOWN is a first-class
//     financial state" from the spec's own refinements.
//   - Once PREPARED, an attempt's authorized fields (amount, currency,
//     payer/payee references, authorization fingerprint) are immutable —
//     enforced by a database trigger — the direct fix for "payment amount
//     changed after authorization."
//   - "Payment sent from suspended/unverified account" has no real
//     account-status source to check against (AP-09/AP-10 already treat
//     bank account references as opaque — see their own package docs), so
//     this service requires the caller to explicitly assert
//     PayerAccountVerified=true at PrepareAttempt time — a real,
//     caller-attested gate, not a fabricated automatic check.
package domain

import "time"

type AttemptStatus string

const (
	StatusPrepared                 AttemptStatus = "PREPARED"
	StatusSubmitted                AttemptStatus = "SUBMITTED"
	StatusPendingUnknown           AttemptStatus = "PENDING_UNKNOWN"
	StatusRejectedBeforeSubmission AttemptStatus = "REJECTED_BEFORE_SUBMISSION"
	StatusCancelled                AttemptStatus = "CANCELLED"
	StatusQuarantined              AttemptStatus = "QUARANTINED"
)

// AuthorizationSourcePaymentAuthorization is the AP-10 source
// (payment-authorization-svc) PrepareAttempt independently verifies via
// AuthorizationClient (Wave 11a).
//
// AuthorizationSourceTreasury is the BNK-09 source (Wave 11b, closing the
// carve-out AuthorizationSourcePaymentAuthorization's Wave 11a comment
// used to document): treasury-svc's own ExecuteTreasuryTransfer now sends
// a real fingerprint — domain.TransferFingerprint over the approved
// transfer's identity/amount/account/checker fields — and PrepareAttempt
// independently re-fetches and compares it against treasury-svc's
// GetTreasuryTransferFingerprint via TreasuryClient, the same
// live-refetch-and-compare idiom as the AP-10 path, just against a
// different upstream.
//
// Any AuthorizationSource value outside these two, other than empty, is
// rejected (ErrUnrecognizedAuthorizationSource) rather than silently
// trusted — the two documented callers of this endpoint (payment-run-svc
// for AP-10, treasury-svc for BNK-09) now both declare their source, so a
// third, unrecognized one is more likely a bug or spoofing attempt than a
// legitimate new caller. Empty remains exempted from verification for
// backward compatibility with any caller that predates this field.
const (
	AuthorizationSourcePaymentAuthorization = "PAYMENT_AUTHORIZATION_SVC"
	AuthorizationSourceTreasury             = "TREASURY_SVC"
)

func CanSubmit(s AttemptStatus) bool                 { return s == StatusPrepared }
func CanCancelBeforeSubmission(s AttemptStatus) bool { return s == StatusPrepared }
func CanRetry(s AttemptStatus) bool                  { return s == StatusPendingUnknown }
func CanResolveAmbiguous(s AttemptStatus) bool       { return s == StatusPendingUnknown }
func CanQuarantine(s AttemptStatus) bool {
	return s == StatusPrepared || s == StatusPendingUnknown
}

type PaymentInitiationAttempt struct {
	AttemptID                string
	TenantID                 *string
	LegalEntityID            string
	SourceReference          string // caller's own reference (e.g. AP-11's instruction_id)
	AuthorizationFingerprint string
	// AuthorizationID/AuthorizationSource identify which upstream record
	// and service AuthorizationFingerprint was derived from, so
	// PrepareAttempt can independently re-fetch and compare it rather than
	// trusting it as given — see AuthorizationSourcePaymentAuthorization's
	// own doc comment.
	AuthorizationID   string
	AuthorizationSource string
	PayerAccountRef          string
	PayeeRef                 string
	Amount                   float64
	Currency                 string
	ExecutionDate            time.Time
	PaymentReference         string
	PayerAccountVerified     bool

	IdempotencyKey string

	Status                  AttemptStatus
	ProviderRequestID       string
	ProviderResponseRef     string
	RejectionReason         string
	QuarantineReason        string
	AmbiguousResolutionNote string

	SubmittedAt *time.Time
	ResolvedAt  *time.Time

	CreatedByPrincipalID string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type AttemptEvent struct {
	EventID          string
	TenantID         *string
	AttemptID        string
	EventType        string
	Detail           string
	ActorPrincipalID string
	CreatedAt        time.Time
}

const (
	EventInitiationPrepared  = "PAYMENT_INITIATION_PREPARED"
	EventPaymentSubmitted    = "PAYMENT_SUBMITTED"
	EventSubmissionAmbiguous = "PAYMENT_SUBMISSION_AMBIGUOUS"
	EventSubmissionRejected  = "PAYMENT_SUBMISSION_REJECTED"
	EventAttemptCancelled    = "PAYMENT_ATTEMPT_CANCELLED"
	EventAttemptQuarantined  = "PAYMENT_ATTEMPT_QUARANTINED"
	EventAmbiguousResolved   = "PAYMENT_SUBMISSION_AMBIGUITY_RESOLVED"
)

// ── request DTOs ────────────────────────────────────────────────────────────

type PrepareAttemptRequest struct {
	LegalEntityID            string
	SourceReference          string
	AuthorizationFingerprint string
	AuthorizationID          string
	AuthorizationSource      string
	PayerAccountRef          string
	PayeeRef                 string
	Amount                   float64
	Currency                 string
	ExecutionDate            time.Time
	PaymentReference         string
	PayerAccountVerified     bool
	IdempotencyKey           string
}

type ResolveAmbiguousRequest struct {
	ResolvedStatus AttemptStatus // SUBMITTED or REJECTED_BEFORE_SUBMISSION
	Note           string
}

type QuarantineRequest struct {
	Reason string
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrAttemptNotFound            = sentinel("payment initiation attempt not found")
	ErrInvalidTransition          = sentinel("invalid payment initiation attempt state transition")
	ErrIdempotencyKeyRequired     = sentinel("idempotency_key is required")
	ErrDuplicateIdempotencyKey    = sentinel("an attempt already exists for this idempotency key")
	// ErrUnresolvedAttemptExists is Invariant #16: a source_reference with
	// an attempt in PREPARED/PENDING_UNKNOWN/SUBMITTED must not get a
	// second one — the existing one has to resolve (get rejected or get
	// cancelled) first. SUBMITTED is included: this service never
	// transitions an attempt OUT of SUBMITTED (finality tracking is
	// BNK-07/payment-status-svc's own concern), so a second send for the
	// same source_reference must never happen once the first was already
	// submitted, regardless of how it later resolves downstream. A
	// genuine retry after a real failure mints a NEW source_reference.
	// Unlike ErrDuplicateIdempotencyKey this is checked even when the
	// caller supplies a brand-new idempotency_key, which is exactly the
	// gap this closes: a caller retrying with a fresh key for the same
	// underlying instruction could otherwise create a second, concurrent
	// live attempt.
	ErrUnresolvedAttemptExists    = sentinel("an unresolved attempt already exists for this source_reference")
	ErrPayerAccountNotVerified    = sentinel("payer_account_verified must be true to submit a payment")
	ErrInvalidResolution          = sentinel("resolved_status must be SUBMITTED or REJECTED_BEFORE_SUBMISSION")
	ErrProviderAdapterUnavailable = sentinel("provider adapter unavailable")
	ErrStoreUnavailable           = sentinel("store unavailable")

	// ErrAuthorizationFingerprintRequired/Mismatch/Unavailable are Wave
	// 11a's real fingerprint verification outcomes for an
	// AuthorizationSourcePaymentAuthorization attempt — see
	// clients/authorization.go.
	ErrAuthorizationFingerprintRequired  = sentinel("authorization_fingerprint and authorization_id are required for this authorization_source")
	ErrAuthorizationFingerprintMismatch  = sentinel("authorization_fingerprint does not match the live authorization record")
	ErrAuthorizationServiceUnavailable   = sentinel("payment-authorization-svc unavailable")

	// ErrTreasuryFingerprintMismatch/Unavailable are Wave 11b's fingerprint
	// verification outcomes for an AuthorizationSourceTreasury attempt —
	// see clients/treasury.go's VerifyTransferFingerprint.
	ErrTreasuryFingerprintMismatch = sentinel("authorization_fingerprint does not match the live treasury transfer record")

	// ErrUnrecognizedAuthorizationSource (Wave 11b): a non-empty
	// authorization_source outside AuthorizationSourcePaymentAuthorization/
	// AuthorizationSourceTreasury is refused rather than silently trusted
	// — see this file's doc comment on those two constants.
	ErrUnrecognizedAuthorizationSource = sentinel("unrecognized authorization_source")
)
