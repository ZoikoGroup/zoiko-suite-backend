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

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other service in this platform.
	ErrIdentityMissing = errorString("caller identity missing")
)
