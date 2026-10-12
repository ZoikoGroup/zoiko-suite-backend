// Package domain defines the authoritative domain types for
// payment-authorization-svc — AP-10 of the Procurement, Expenses & Accounts
// Payable baseline. Its job: authorize the exact frozen payment subject
// produced by AP-09, using delegated limits, maker-checker, payee-identity
// re-verification, and a protected-field fingerprint that becomes invalid
// on any material change.
//
// AP-10 is the first consumer of AP-09's own FROZEN state and
// GetFingerprint endpoint — both built earlier this session specifically so
// a real downstream authorization step could exist. Real integrations,
// reusing contracts this session already built and verified rather than
// researching new ones:
//
//   - payment-proposal-svc (AP-09)'s real GET /ap09/proposals/{id} and
//     GET /ap09/proposals/{id}/fingerprint: a proposal must be FROZEN to
//     request authorization against it, and its fingerprint is captured at
//     request time as ProposalFingerprint.
//   - supplier-financial-profile-svc (AP-01): AP-09 already snapshots each
//     AP_INVOICE item's payee updated_at at freeze time, but that
//     guarantee is only as fresh as AP-09's own freeze moment — time
//     passes between freeze and authorization, and further between
//     authorization and consumption. This service copies AP-09's captured
//     snapshots at RequestPaymentAuthorization time into its own
//     authorization_payee_snapshots (its own authoritative record, per its
//     own contract), then independently re-verifies them live against
//     supplier-financial-profile-svc at BOTH ApprovePayment and
//     ConsumePaymentAuthorization — two further, later checkpoints than
//     AP-09 ever had the chance to check. This is the literal enforcement
//     of negative-path scenario #1 ("payee bank details changed after
//     approval"): any mismatch found at either checkpoint moves the
//     authorization to INVALIDATED (a genuinely reachable state, not just
//     a rejected request — matching the state model's own words, "any
//     protected-field mismatch invalidates").
//   - policy-svc's real POST /v1/policies/evaluate
//     (policy_type=APPROVAL_THRESHOLD) against the proposal's net amount —
//     the same integration AP-07 and AP-09 already use, reused here as
//     AP-10's own "delegated signing limit" (negative-path scenario #2): a
//     signer without PAYMENT_AUTHORIZE_HIGHVALUE cannot approve a payment
//     policy-svc flags APPROVAL_REQUIRED.
//   - authorization-svc's dynamic own-object SoD layer, with the
//     proposal's own preparer (fetched from AP-09) as resource owner — the
//     FIFTH reuse of that feature this session (after AP-01, AP-04, AP-07,
//     AP-09), directly enforcing negative-path scenario #3 ("proposal
//     maker self-authorizes where prohibited").
//   - UPDATE: payee-banking-identity-svc (ORG-10)'s real
//     GetActivePayeeDestination is now consulted at RequestPaymentAuthorization
//     time for every AP_INVOICE-sourced payee, pinning its active
//     destination as PayeeSnapshot.DestinationID when ORG-10 has one on
//     file — closing ORG-10's own named dependency ("AP-10 fingerprints
//     active version"). verifyStillEligible re-checks it live at both
//     ApprovePayment and ConsumePaymentAuthorization exactly like the
//     supplier-profile identity check: a destination that has since been
//     superseded or suspended invalidates the authorization, the same
//     "any protected-field mismatch invalidates" doctrine. When ORG-10 has
//     no destination on file for a payee (real, current coverage gap —
//     ORG-10 is new), DestinationID stays empty and no re-check is made
//     for that payee — an honest absence, not a fabricated pass.
//
// Controls added on top of that (ZS-SVC-D-001 AP-10):
//
//   - Signer quorum. A payment policy-svc flags APPROVAL_REQUIRED needs
//     HIGH_VALUE_REQUIRED_SIGNATURES (default 2) DISTINCT signers, each
//     holding PAYMENT_AUTHORIZE_HIGHVALUE and none of them the proposal's
//     preparer. Each signature is an append-only row under a unique
//     (authorization, signer) index, so the same person can never count
//     twice; the authorization becomes APPROVED, and PaymentAuthorized is
//     published, only when the count is reached. The required count can be
//     raised but never lowered.
//   - Expiry. expires_at is set at request time (AUTHORIZATION_TTL, default
//     24h). Approve and Consume refuse an overdue authorization on their
//     own, and a sweeper (internal/expiry) expires overdue ones and
//     publishes PaymentAuthorizationExpired. Rows that predate expiry keep a
//     NULL expires_at and are never auto-expired.
//   - Payee-bank changer. Whoever proposed, verified or approved a payee's
//     active ORG-10 banking destination cannot request or sign a payment to
//     it (SOD_CONFLICT); the check fails closed if ORG-10 cannot be asked.
//   - Idempotency-Key is required on request, approve and consume; a replay
//     returns the stored response (Idempotent-Replay: true), the same key on
//     a different request is IDEMPOTENCY_KEY_REUSED. expected_version is
//     honoured on approve/reject/revoke when supplied (STALE_VERSION); it is
//     not required here because the subject fingerprint and the live
//     re-validation already bind exactly what is being approved.
//   - Every terminal outcome (rejected, invalidated, revoked, expired,
//     consumed) and the final approval go through the transactional outbox.
//     Intermediate signatures are local evidence only.
//   - Errors carry a stable "code" next to the human "error" text.
//
// Remaining gaps, stated plainly:
//
//   - No step-up / session-assurance evidence is collected for signers (the
//     spec lists it as optional input); there is no break-glass path.
//   - A payee ORG-10 has no destination for is still allowed through
//     unpinned (see above), so the bank-changer rule cannot apply to it.
//   - The AP-01 supplier profile's own "last payee-related change by"
//     principal is not yet consulted; only ORG-10's record is.
package domain

import "time"

type AuthorizationStatus string

const (
	StatusPending     AuthorizationStatus = "PENDING"
	StatusApproved    AuthorizationStatus = "APPROVED"
	StatusRejected    AuthorizationStatus = "REJECTED"
	StatusConsumed    AuthorizationStatus = "CONSUMED"
	StatusRevoked     AuthorizationStatus = "REVOKED"
	StatusExpired     AuthorizationStatus = "EXPIRED"
	StatusInvalidated AuthorizationStatus = "INVALIDATED"
)

// CanDecide reports whether an authorization in status s may be approved or
// rejected.
func CanDecide(s AuthorizationStatus) bool { return s == StatusPending }

// CanConsume reports whether an authorization in status s may be consumed —
// exactly once, ever (negative-path scenario #4).
func CanConsume(s AuthorizationStatus) bool { return s == StatusApproved }

// CanRevoke reports whether an authorization in status s may be revoked.
func CanRevoke(s AuthorizationStatus) bool { return s == StatusPending || s == StatusApproved }

// CanExpire reports whether an authorization in status s may be expired.
func CanExpire(s AuthorizationStatus) bool { return s == StatusPending || s == StatusApproved }

type PaymentAuthorization struct {
	AuthorizationID     string
	TenantID            *string
	LegalEntityID       string
	ProposalID          string
	ProposalFingerprint string
	NetAmount           float64
	Currency            string

	Status AuthorizationStatus

	PolicyAssessmentResult string
	PolicyVersionID        string

	RequestedByPrincipalID string

	ApprovedByPrincipalID *string
	ApprovedAt            *time.Time
	RejectedReason        string

	RevokedByPrincipalID *string
	RevokedReason        string
	RevokedAt            *time.Time

	ExpiredAt *time.Time

	ConsumedByPrincipalID *string
	ConsumedAt            *time.Time

	InvalidatedReason string

	// Version is bumped by the database on every update; a caller can pass it
	// back as expected_version to refuse acting on a stale view.
	Version int
	// RequiredSignatures is how many distinct signers must approve (set from
	// the policy result at the first signature); SignatureCount how many have.
	RequiredSignatures int
	SignatureCount     int
	// ExpiresAt is when an unused authorization stops being valid; nil for
	// rows created before expiry existed.
	ExpiresAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Signature is one signer's approval of an authorization.
type Signature struct {
	SignatureID       string
	TenantID          *string
	AuthorizationID   string
	SignerPrincipalID string
	PolicyResult      string
	PolicyVersionID   string
	SignedAt          time.Time
}

// ExpiredRef identifies an authorization past its expires_at, for the sweeper.
type ExpiredRef struct {
	AuthorizationID string
	TenantID        string
}

// IdempotencyRecord is the stored state of an Idempotency-Key.
type IdempotencyRecord struct {
	RequestHash string
	Completed   bool
	StatusCode  int
	Body        []byte
}

// PayeeSnapshot is AP-10's own authoritative copy of the payee identity
// version it authorized against — copied from AP-09's own item snapshots
// at RequestPaymentAuthorization time, then independently re-verified
// live at ApprovePayment and ConsumePaymentAuthorization. See package doc.
//
// DestinationID additionally pins payee-banking-identity-svc's (ORG-10)
// active destination for this payee at request time, when ORG-10 has one
// on file — empty otherwise (ORG-10 coverage is not yet complete across
// every existing payee, a real, current gap rather than a fabricated
// absence). When non-empty, verifyStillEligible re-checks it live exactly
// like the supplier-profile identity check.
type PayeeSnapshot struct {
	SnapshotID      string
	TenantID        *string
	AuthorizationID string
	PayeeRef        string
	PayeeSnapshotAt time.Time
	DestinationID   string
	CreatedAt       time.Time
}

type AuthorizationEvent struct {
	EventID          string
	TenantID         *string
	AuthorizationID  string
	EventType        string
	Detail           string
	ActorPrincipalID string
	CreatedAt        time.Time
}

const (
	EventAuthorizationRequested   = "PAYMENT_AUTHORIZATION_REQUESTED"
	EventPaymentAuthorized        = "PAYMENT_AUTHORIZED"
	EventAuthorizationRejected    = "PAYMENT_AUTHORIZATION_REJECTED"
	EventAuthorizationInvalidated = "PAYMENT_AUTHORIZATION_INVALIDATED"
	EventAuthorizationConsumed    = "PAYMENT_AUTHORIZATION_CONSUMED"
	EventAuthorizationRevoked     = "PAYMENT_AUTHORIZATION_REVOKED"
	EventAuthorizationExpired     = "PAYMENT_AUTHORIZATION_EXPIRED"
	// EventAuthorizationSigned is local evidence of one signature toward a
	// quorum; it is not published (PaymentAuthorized is, once the quorum is met).
	EventAuthorizationSigned = "PAYMENT_AUTHORIZATION_SIGNED"
)

// ── request DTOs ────────────────────────────────────────────────────────────

type RequestAuthorizationRequest struct {
	ProposalID string
}

// ApproveRequest is the optional body of ApprovePayment.
type ApproveRequest struct {
	ExpectedVersion *int `json:"expected_version,omitempty"`
}

type RejectPaymentRequest struct {
	Reason          string
	ExpectedVersion *int `json:"expected_version,omitempty"`
}

type RevokeAuthorizationRequest struct {
	Reason          string
	ExpectedVersion *int `json:"expected_version,omitempty"`
}

// ── sentinel errors ─────────────────────────────────────────────────────────

type sentinel string

func (s sentinel) Error() string { return string(s) }

const (
	ErrAuthorizationNotFound              = sentinel("payment authorization not found")
	ErrInvalidTransition                  = sentinel("invalid payment authorization state transition")
	ErrProposalNotEligible                = sentinel("proposal does not exist, does not belong to this legal entity, or is not FROZEN")
	ErrProposalServiceUnavailable         = sentinel("payment-proposal-svc unavailable")
	ErrProposalAlreadyRequested           = sentinel("proposal already has an active (non-terminal) authorization request")
	ErrFingerprintMismatch                = sentinel("proposal fingerprint no longer matches the one captured at request time")
	ErrPayeeIdentityStale                 = sentinel("a payee identity has changed since this authorization was requested")
	ErrPayeeServiceUnavailable            = sentinel("supplier-financial-profile-svc unavailable")
	ErrPayeeDestinationChanged            = sentinel("a payee's active banking destination has changed since this authorization was requested")
	ErrPayeeDestinationServiceUnavailable = sentinel("payee-banking-identity-svc unavailable")
	ErrNoActiveDestination                = sentinel("payee-banking-identity-svc has no active destination on file for this payee")
	ErrPolicyServiceUnavailable           = sentinel("policy-svc unavailable")
	ErrStoreUnavailable                   = sentinel("store unavailable")
	ErrAlreadySigned                      = sentinel("this principal has already signed this authorization")
	ErrStaleVersion                       = sentinel("the authorization changed since the version the caller acted on")
	ErrAuthorizationExpired               = sentinel("the authorization has expired")
	ErrBankDetailChangerConflict          = sentinel("a principal who proposed, verified or approved this payee's banking destination cannot authorize a payment to it")
)
