// Package domain defines the authoritative domain types for
// delegated-authority-svc.
//
// Per docs/architecture/03-microservices.md §9.3, this service "maintains
// time-bound, scope-bound, approval-bound delegated authority chains," with
// a hard constraint: "delegated authority must never exceed the delegator's
// own authority." That constraint is enforced at creation time via a real
// synchronous call to authorization-svc — CheckAllowed on the delegator,
// for the exact action_type being delegated — rather than trusted from the
// request body. This service does not itself decide whether the delegator
// holds the authority; authorization-svc remains the single source of
// truth for that.
//
// Additionally, per ORG-06 §4.6, "Cannot delegate around SoD" — a delegation
// must not create a segregation-of-duties conflict. This is enforced by
// consulting authorization-svc's SoD engine before creating the delegation.
package domain

import (
	"fmt"
	"strings"
	"time"
)

// DelegationStatus is a linear chain: ACTIVE -> REVOKED (explicit action)
// or ACTIVE -> EXPIRED (lazy, on read, once EffectiveTo has passed). Both
// are terminal.
//
// PROPOSED: awaiting approval from the delegator (or an approver).
// SUSPENDED: temporarily inactive, can be reactivated.
// These states add a review step before a delegation becomes ACTIVE, and
// allow temporary removal without revocation.
type DelegationStatus string

const (
	DelegationStatusProposed  DelegationStatus = "PROPOSED"
	DelegationStatusActive    DelegationStatus = "ACTIVE"
	DelegationStatusSuspended DelegationStatus = "SUSPENDED"
	DelegationStatusRevoked   DelegationStatus = "REVOKED"
	DelegationStatusExpired   DelegationStatus = "EXPIRED"
)

// DelegationGrant is one delegated-authority chain: DelegatorPrincipalID
// grants DelegatePrincipalID the ability to act as if authorized for
// ActionType, within EffectiveFrom/EffectiveTo, scoped to LegalEntityID.
// Entity-bound, never hard-deleted.
//
// ORG-06 §4.5: AuthorityLimit adds a monetary/quantitative ceiling to the
// delegation — e.g., "approve payments up to 50,000 cents (500.00 USD)" or
// "issue up to 10 purchase orders". Both limits are optional; an absent limit
// means no ceiling (subject to the delegator's own limits).
type DelegationGrant struct {
	DelegationID         string           `json:"delegation_id"`
	TenantID             string           `json:"tenant_id"`
	LegalEntityID        string           `json:"legal_entity_id"`
	DelegatorPrincipalID string           `json:"delegator_principal_id"`
	DelegatePrincipalID  string           `json:"delegate_principal_id"`
	ActionType           string           `json:"action_type"`
	EffectiveFrom        time.Time        `json:"effective_from"`
	EffectiveTo          time.Time        `json:"effective_to"`
	Status               DelegationStatus `json:"status"`
	CreatedByPrincipalID string           `json:"created_by_principal_id"`
	CorrelationID        string           `json:"correlation_id"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
	RevokedByPrincipalID *string          `json:"revoked_by_principal_id,omitempty"`
	RevokedAt            *time.Time       `json:"revoked_at,omitempty"`
	ExpiredAt            *time.Time       `json:"expired_at,omitempty"`

	// AuthorityLimit: optional monetary ceiling in minor units (cents) + ISO 4217 currency.
	AuthorityLimitCents    *int64  `json:"authority_limit_cents,omitempty"`
	AuthorityLimitCurrency *string `json:"authority_limit_currency,omitempty"`

	// AuthorityLimitQuantity: optional quantitative ceiling (e.g., max count of actions).
	AuthorityLimitQuantity *int64 `json:"authority_limit_quantity,omitempty"`

	// Version is the optimistic locking version. Incremented on every update.
	// Clients should provide expected_version on protected mutations (revoke,
	// extend) to prevent lost updates.
	Version int64 `json:"version"`

	// Evidence (ORG-06 §4.6: delegator, authority basis, scope, limits, valid
	// time, approval, revocation reason).
	Reason                 string     `json:"reason,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ApprovalMethod         *string    `json:"approval_method,omitempty"`
	SuspendedByPrincipalID *string    `json:"suspended_by_principal_id,omitempty"`
	SuspendedAt            *time.Time `json:"suspended_at,omitempty"`
	SuspensionReason       *string    `json:"suspension_reason,omitempty"`
	RevocationReason       *string    `json:"revocation_reason,omitempty"`
}

// Approval methods recorded on activation (migration 000010).
const (
	// ApprovalDelegatorSelf: the delegator created the grant of their own
	// authority, which is itself the approval.
	ApprovalDelegatorSelf = "DELEGATOR_SELF"
	// ApprovalDelegator: the delegator approved a proposal someone else made.
	ApprovalDelegator = "DELEGATOR_APPROVAL"
	// ApprovalAdministrator: a second administrator — neither the maker nor
	// the delegate — approved the proposal.
	ApprovalAdministrator = "ADMINISTRATOR_APPROVAL"
)

// Transition names recorded in delegation_history.
const (
	TransitionProposed  = "PROPOSED"
	TransitionActivated = "ACTIVATED"
	TransitionSuspended = "SUSPENDED"
	TransitionResumed   = "RESUMED"
	TransitionExtended  = "EXTENDED"
	TransitionRevoked   = "REVOKED"
	TransitionExpired   = "EXPIRED"
)

// TransitionInput is what every protected state change carries: the version
// the caller read (required — ORG-06 "all protected changes require
// authoritative current version") and, where the change needs one, a reason.
type TransitionInput struct {
	ExpectedVersion  int64
	ActorPrincipalID string
	Reason           string
	// ApprovalMethod is set on activation only.
	ApprovalMethod string
	// NewEffectiveTo is set on extension only.
	NewEffectiveTo time.Time
}

// minorUnitExponent is ISO 4217's minor-unit count for the currencies that
// differ from the usual two; authority_limit_cents is in minor units.
var minorUnitExponent = map[string]int{
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "UGX": 0, "XAF": 0, "XOF": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3, "LYD": 3, "IQD": 3,
}

// FormatMinorUnits renders an amount in minor units as a decimal string in
// major units ("50000", "USD" → "500.00"; "500", "JPY" → "500").
func FormatMinorUnits(minor int64, currency string) string {
	exp, ok := minorUnitExponent[currency]
	if !ok {
		exp = 2
	}
	if exp == 0 {
		return fmt.Sprintf("%d", minor)
	}
	div := int64(1)
	for i := 0; i < exp; i++ {
		div *= 10
	}
	return fmt.Sprintf("%d.%0*d", minor/div, exp, minor%div)
}

// ValidateLimit checks a requested authority limit: a monetary ceiling needs
// a positive amount and an ISO 4217 code together; a quantity is positive.
func ValidateLimit(cents *int64, currency *string, quantity *int64) error {
	if (cents == nil) != (currency == nil) {
		return ErrInvalidLimit
	}
	if cents != nil {
		c := *currency
		if *cents <= 0 || len(c) != 3 || strings.ToUpper(c) != c || strings.Trim(c, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			return ErrInvalidLimit
		}
	}
	if quantity != nil && *quantity <= 0 {
		return ErrInvalidLimit
	}
	return nil
}

// ── wire types ───────────────────────────────────────────────────────────────

// ListDelegationsFilter is the parameter object for a register read.
//
// It carries SelfPrincipalID, which is the whole of the fix for an unscoped
// read: a caller who names no legal entity is answered with the delegations
// they are personally party to, rather than the tenant's entire map of who
// may act for whom.
type ListDelegationsFilter struct {
	LegalEntityID        string
	DelegatorPrincipalID string
	DelegatePrincipalID  string
	Status               string

	// SelfPrincipalID, when non-empty, restricts the result to rows where
	// this principal is the delegator or the delegate. Set only for reads
	// that were not authorized against a legal entity.
	SelfPrincipalID string

	Limit  int
	Offset int
}

type CreateDelegationRequest struct {
	LegalEntityID        string    `json:"legal_entity_id"`
	DelegatorPrincipalID string    `json:"delegator_principal_id"`
	DelegatePrincipalID  string    `json:"delegate_principal_id"`
	ActionType           string    `json:"action_type"`
	EffectiveFrom        time.Time `json:"effective_from"`
	EffectiveTo          time.Time `json:"effective_to"`
	CorrelationID        string    `json:"correlation_id"`

	// AuthorityLimit: optional monetary ceiling in minor units (cents) + ISO 4217 currency.
	AuthorityLimitCents    *int64  `json:"authority_limit_cents,omitempty"`
	AuthorityLimitCurrency *string `json:"authority_limit_currency,omitempty"`

	// AuthorityLimitQuantity: optional quantitative ceiling (e.g., max count of actions).
	AuthorityLimitQuantity *int64 `json:"authority_limit_quantity,omitempty"`
	// Reason is required (ORG-06 §4.6 required source input).
	Reason string `json:"reason"`
	// Propose asks for a PROPOSED grant even when the delegator creates it.
	// A grant created on someone else's behalf is always PROPOSED.
	Propose bool `json:"propose,omitempty"`
}

// TransitionRequest is the body of activate / suspend / resume / revoke.
type TransitionRequest struct {
	Reason string `json:"reason"`
}

// ExtendDelegationRequest extends an ACTIVE delegation's effective_to.
type ExtendDelegationRequest struct {
	DelegationID   string    `json:"delegation_id"`
	NewEffectiveTo time.Time `json:"new_effective_to"`
	CorrelationID  string    `json:"correlation_id"`
	Reason         string    `json:"reason"`
}

// RefusedEscalation records a refused escalation attempt for audit.
// ORG-06 §4.2: "Every refused escalation attempt leaves durable evidence."
type RefusedEscalation struct {
	RefusedID            string    `json:"refused_id"`
	TenantID             string    `json:"tenant_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	CallerPrincipalID    string    `json:"caller_principal_id"`
	DelegatorPrincipalID string    `json:"delegator_principal_id"`
	DelegatePrincipalID  string    `json:"delegate_principal_id"`
	ActionType           string    `json:"action_type"`
	EffectiveFrom        time.Time `json:"effective_from"`
	EffectiveTo          time.Time `json:"effective_to"`
	RefusalReason        string    `json:"refusal_reason"` // self_dealing, delegator_mismatch, delegator_lacks_authority, sod_conflict, overlap_conflict, invalid_window, no_create_grant
	CorrelationID        string    `json:"correlation_id,omitempty"`
	IdempotencyKey       string    `json:"idempotency_key,omitempty"`
	RequestID            string    `json:"request_id,omitempty"`
	SourceChannel        string    `json:"source_channel,omitempty"`
	RefusedAt            time.Time `json:"refused_at"`
}

// RefusedEscalationFilter for listing refused escalations.
type RefusedEscalationFilter struct {
	TenantID             string
	LegalEntityID        string
	CallerPrincipalID    string
	DelegatorPrincipalID string
	DelegatePrincipalID  string
	RefusalReason        string
	Limit                int
	Offset               int
}

// DelegationChainStep represents one step in a delegation chain.
// Each step shows a delegator granting authority to a delegate.
type DelegationChainStep struct {
	StepNumber           int              `json:"step_number"`
	DelegatorPrincipalID string           `json:"delegator_principal_id"`
	DelegatePrincipalID  string           `json:"delegate_principal_id"`
	ActionType           string           `json:"action_type"`
	EffectiveFrom        time.Time        `json:"effective_from"`
	EffectiveTo          time.Time        `json:"effective_to"`
	Status               DelegationStatus `json:"status"`
	DelegationID         string           `json:"delegation_id"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrDelegationNotFound = errorString("delegation not found")
	ErrInvalidTransition  = errorString("invalid delegation status transition")
	ErrStoreUnavailable   = errorString("delegated authority store unavailable")

	ErrAuthorizationDenied     = errorString("authorization denied for this delegation action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrDelegatorLacksAuthority is returned when the delegator does not
	// hold a GRANTED decision for the action_type being delegated — the
	// platform's core delegation invariant.
	ErrDelegatorLacksAuthority = errorString("delegator does not hold the authority being delegated")

	// ErrInvalidTimeWindow is returned when effective_to is not strictly
	// after effective_from — a delegation must have a real, positive
	// duration to be "time-bound" at all.
	ErrInvalidTimeWindow = errorString("effective_to must be after effective_from")

	// ErrDelegatorMismatch is returned when the caller names someone else as
	// the delegator without holding DELEGATION_ADMINISTER on the entity.
	//
	// This is the gap that made DELEGATION_CREATE a privilege-escalation
	// primitive. The service verified that the NAMED delegator held the
	// authority being delegated -- and never that the caller was that
	// delegator, or had any right to act for them. A principal holding only
	// DELEGATION_CREATE could name any colleague as delegator and themselves
	// as delegate, and so mint themselves that colleague's authority. Both
	// checks that existed passed, because the invariant they enforce -- "a
	// delegation must not exceed the delegator's authority" -- was never the
	// one being violated.
	ErrDelegatorMismatch = errorString("caller may only delegate their own authority")

	// ErrSelfDealing is returned when a caller acting under
	// DELEGATION_ADMINISTER routes another principal's authority to
	// themselves. Administering delegations for other people is a legitimate
	// administrative power; being the beneficiary of one you created is not,
	// and it is the same escalation by a longer route.
	ErrSelfDealing = errorString("a delegation administered for another principal may not name the caller as delegate")

	// ErrDelegateIsDelegator is returned when both ends of the grant are the
	// same principal -- not a delegation chain, a no-op that reads as one.
	ErrDelegateIsDelegator = errorString("delegate_principal_id must differ from delegator_principal_id")

	// ErrUnknownStatus is returned for a status filter outside the domain
	// vocabulary. Silently returning no rows for a typo is a dangerous answer
	// on a governance register: "no delegations" and "you misspelled the
	// filter" must not look identical.
	ErrUnknownStatus = errorString("status must be one of PROPOSED, ACTIVE, SUSPENDED, REVOKED, EXPIRED")

	// ErrVersionMismatch is returned when the expected_version provided on a
	// protected mutation (revoke, extend) does not match the current version
	// in the store — preventing lost updates via optimistic locking.
	ErrVersionMismatch = errorString("expected_version does not match current version")

	// ErrSODConflict is returned when the delegation would create a
	// segregation-of-duties conflict — e.g., delegating PAYMENT_APPROVE to a
	// principal who already holds PAYMENT_RELEASE, which the static conflict
	// matrix forbids.
	ErrSODConflict = errorString("delegation would create a segregation-of-duties conflict")

	// ErrOverlapConflict is returned when the delegation would create an
	// overlapping time window for the same (delegate, action_type) on the
	// same entity — ORG-06 §4.4 forbids a delegate from holding two ACTIVE
	// delegations for the same action on the same entity at the same time.
	ErrOverlapConflict = errorString("delegation would create an overlapping active delegation for the same action")

	// ErrInvalidPaging is returned for an out-of-range limit or offset.
	ErrInvalidPaging = errorString("limit must be between 1 and 500 and offset must not be negative")

	// ErrCannotExtend is returned when a delegation cannot be extended —
	// e.g., it's not ACTIVE or the new effective_to is not after the current one.
	ErrCannotExtend = errorString("delegation cannot be extended from its current state")

	// ErrTenantMissing is returned when a request carries no X-Tenant-Id.
	// Distinct from ErrIdentityMissing so a forgotten tenant header is not
	// reported as a missing principal.
	ErrTenantMissing = errorString("tenant context missing")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other service in this platform.
	ErrIdentityMissing = errorString("caller identity missing")

	// ErrVersionRequired: a protected change named no expected_version.
	ErrVersionRequired = errorString("expected_version is required: read the delegation and send the version you read")
	// ErrReasonRequired: a change that needs a stated reason had none.
	ErrReasonRequired = errorString("reason is required")
	// ErrInvalidLimit: a malformed authority limit.
	ErrInvalidLimit = errorString("authority_limit_cents and authority_limit_currency go together; the amount and authority_limit_quantity must be positive and the currency an ISO 4217 code")
	// ErrDelegatorExceedsLimit: ORG-06 negative case 11 — the ceiling being
	// delegated is above the delegator's own authority limit.
	ErrDelegatorExceedsLimit = errorString("the delegated limit exceeds the delegator's own authority limit")
	// ErrApprovalNotSegregated: the approver is the maker or the delegate.
	ErrApprovalNotSegregated = errorString("a proposal is approved by its delegator, or by an administrator who is neither its maker nor its delegate")
	// ErrLapsed: the delegation's window has already ended.
	ErrLapsed = errorString("the delegation's window has already ended; no grace extension")
)
