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
package domain

import "time"

// DelegationStatus is a linear chain: ACTIVE -> REVOKED (explicit action)
// or ACTIVE -> EXPIRED (lazy, on read, once EffectiveTo has passed). Both
// are terminal.
type DelegationStatus string

const (
	DelegationStatusActive  DelegationStatus = "ACTIVE"
	DelegationStatusRevoked DelegationStatus = "REVOKED"
	DelegationStatusExpired DelegationStatus = "EXPIRED"
)

// DelegationGrant is one delegated-authority chain: DelegatorPrincipalID
// grants DelegatePrincipalID the ability to act as if authorized for
// ActionType, within EffectiveFrom/EffectiveTo, scoped to LegalEntityID.
// Entity-bound, never hard-deleted.
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
	ErrUnknownStatus = errorString("status must be one of ACTIVE, REVOKED, EXPIRED")

	// ErrInvalidPaging is returned for an out-of-range limit or offset.
	ErrInvalidPaging = errorString("limit must be between 1 and 500 and offset must not be negative")

	// ErrTenantMissing is returned when a request carries no X-Tenant-Id.
	// Distinct from ErrIdentityMissing so a forgotten tenant header is not
	// reported as a missing principal.
	ErrTenantMissing = errorString("tenant context missing")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other service in this platform.
	ErrIdentityMissing = errorString("caller identity missing")
)
