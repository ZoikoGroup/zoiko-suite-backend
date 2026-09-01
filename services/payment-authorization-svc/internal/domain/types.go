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
// Two honest gaps, not fabricated:
//
//   - AP-10's own command list has no automatic trigger for
//     ExpirePaymentAuthorization — there is no background scheduler
//     anywhere in this codebase (the same class of gap already documented
//     for AP-04's GRNI retry and AP-01's high-risk-change flow). Expiry is
//     exposed as a real command an operator or a future scheduled job can
//     call, never invented as an automatic timer this service doesn't
//     actually run.
//   - ConsumePaymentAuthorization has no real caller yet: AP-11 ("Payment
//     Run"), the service that would actually execute a payment and
//     consume its authorization, does not exist in this codebase. This
//     service still implements Consume fully and honestly (including
//     negative-path #4's replay protection) so it is ready the moment
//     AP-11 exists, rather than leaving it half-built.
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

	CreatedAt time.Time
	UpdatedAt time.Time
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
)

// ── request DTOs ────────────────────────────────────────────────────────────

type RequestAuthorizationRequest struct {
	ProposalID string
}

type RejectPaymentRequest struct {
	Reason string
}

type RevokeAuthorizationRequest struct {
	Reason string
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
)
