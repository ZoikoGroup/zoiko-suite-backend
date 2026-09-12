// Package domain defines the authoritative domain types for
// access-control-svc.
//
// Per docs/architecture/03-microservices.md §9.4, this service "maintains
// role catalogues, permission bundles, and policy-linked access groupings."
//
// Scope decision (flagged, not silently assumed): authorization-svc already
// owns live RBAC role/assignment data and every other service's authz
// checks depend on it — migrating that data out from under the whole
// platform is a real architectural risk, not a weekend task. Rather than
// attempt that migration or leave this service as a disconnected shadow
// catalogue, this service is the governed AUTHORING layer for role and
// permission-bundle DEFINITIONS: creating a role/bundle here makes a real
// synchronous call into authorization-svc's existing admin API
// (POST /v1/admin/roles, POST /v1/admin/roles/{id}/permission-bundles) so
// the definition is actually provisioned for real enforcement.
// authorization-svc remains the enforcement source of truth; this service
// adds a governed (authz-checked, idempotent, correlation-tracked) front
// door in front of what is otherwise an unguarded admin API. Per-principal
// role ASSIGNMENTS are explicitly out of scope here — those stay exactly
// where they are.
package domain

import "time"

type RoleStatus string

const (
	RoleStatusActive  RoleStatus = "ACTIVE"
	RoleStatusRetired RoleStatus = "RETIRED"
)

// RoleDefinition is a reusable role catalogue entry. TenantID-scoped (not
// legal-entity-scoped) — matching authorization-svc's own role model, where
// legal-entity scoping happens at assignment time, not definition time.
type RoleDefinition struct {
	RoleDefinitionID     string     `json:"role_definition_id"`
	TenantID             string     `json:"tenant_id"`
	RoleCode             string     `json:"role_code"`
	RoleName             string     `json:"role_name"`
	RoleScopeType        string     `json:"role_scope_type"` // LEGAL_ENTITY, TENANT
	Status               RoleStatus `json:"status"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	// UpdatedByPrincipalID names the verified caller of the last update. Empty
	// for rows never updated since the 000003 column was introduced.
	UpdatedByPrincipalID string    `json:"updated_by_principal_id,omitempty"`
	CorrelationID        string    `json:"correlation_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// PermissionBundleDef is a named set of permitted actions attached to a
// role definition.
type PermissionBundleDef struct {
	BundleID             string    `json:"bundle_id"`
	TenantID             string    `json:"tenant_id"`
	RoleDefinitionID     string    `json:"role_definition_id"`
	BundleCode           string    `json:"bundle_code"`
	PermittedActions     []string  `json:"permitted_actions"`
	ActiveFlag           bool      `json:"active_flag"`
	CorrelationID        string    `json:"correlation_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
	UpdatedByPrincipalID string    `json:"updated_by_principal_id,omitempty"`
}

// ── wire types ───────────────────────────────────────────────────────────────

type CreateRoleRequest struct {
	LegalEntityID string `json:"legal_entity_id"` // used only for the caller's own authz check
	RoleCode      string `json:"role_code"`
	RoleName      string `json:"role_name"`
	RoleScopeType string `json:"role_scope_type"`
	CorrelationID string `json:"correlation_id"`
}

type UpdateRoleRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	RoleName      string `json:"role_name,omitempty"`
	Status        string `json:"status,omitempty"`
	CorrelationID string `json:"correlation_id"`
}

type CreateBundleRequest struct {
	LegalEntityID    string   `json:"legal_entity_id"`
	BundleCode       string   `json:"bundle_code"`
	PermittedActions []string `json:"permitted_actions"`
	CorrelationID    string   `json:"correlation_id"`
}

// UpdateBundleRequest is a partial update to ONE bundle: an omitted field means
// "leave it alone". PermittedActions is nil when absent so an omission can be
// told apart from an explicit empty list (which would be refused below — a
// bundle that grants nothing is a control in name only). ActiveFlag is a
// pointer for the same reason: false is a real detach request, not an absence.
type UpdateBundleRequest struct {
	LegalEntityID    string   `json:"legal_entity_id"`
	PermittedActions []string `json:"permitted_actions"`
	ActiveFlag       *bool    `json:"active_flag"`
	CorrelationID    string   `json:"correlation_id"`
}

// ListFilter narrows and paginates a role-catalogue read. Zero values mean "no
// constraint", except Limit where 0 means unlimited: a caller that wants a page
// must say so, because truncating a catalogue read by default would make its
// totals silently wrong.
type ListFilter struct {
	Status    string
	ScopeType string
	Query     string
	Limit     int
	Offset    int
}

// BundleListFilter narrows the flat permission-bundle catalogue. RoleID narrows
// to one role's bundles; ActiveFlag to one state; Query searches bundle_code.
type BundleListFilter struct {
	RoleID     string
	ActiveFlag *bool
	Query      string
	Limit      int
	Offset     int
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRoleNotFound     = errorString("role definition not found")
	ErrStoreUnavailable = errorString("access control store unavailable")

	// ErrBundleNotFound is returned when a permission bundle id does not exist
	// in the caller's tenant, or does not belong to the role it was addressed
	// under. Intentionally a single error — see ErrRoleNotFound's stance on
	// existence oracles.
	ErrBundleNotFound = errorString("permission bundle not found")

	ErrAuthorizationDenied     = errorString("authorization denied for this access control action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrAuthzAdminUnavailable is returned when provisioning the role or
	// bundle into authorization-svc's admin API fails. A role/bundle
	// definition is never recorded as created here without also having
	// been actually provisioned for enforcement.
	ErrAuthzAdminUnavailable = errorString("authorization-svc admin API unavailable")

	// ErrAuthzBundleNotFound is returned by the authorization-svc admin client
	// when a bundle to retire/reactivate by code has no counterpart there. The
	// register and the enforcement plane disagree, which is a state to surface
	// for investigation rather than one to paper over with a local-only change.
	ErrAuthzBundleNotFound = errorString("permission bundle not found in authorization-svc")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other service in this platform.
	ErrIdentityMissing = errorString("caller identity missing")

	// ErrTenantMissing is returned when a request carries no X-Tenant-Id header.
	ErrTenantMissing = errorString("caller tenant scope missing")
)
