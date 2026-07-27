// Package domain defines the owned types and sentinel errors for access-control-svc.
//
// Access Control Service owns:
//   - Role catalogue (role definitions, scopes)
//   - PermissionBundle (named sets of permitted action strings)
//   - RolePermissionBundleLink (many-to-many: which bundles a role carries)
//
// It does NOT own PrincipalRoleAssignment or DelegatedAuthority — those write
// paths belong to this service's sibling services per §9.3–§9.4 in
// docs/architecture/03-microservices.md. The tables do exist in the DB schema
// so they can be READ and populated via Kafka events from authorization-svc.
//
// Every write in this service must be idempotent (doctrine §3.7).
// No material object is hard-deleted (doctrine §2.11 / no soft-delete).
package domain

import (
	"errors"
	"time"
)

// ── sentinel errors ───────────────────────────────────────────────────────────

var (
	ErrRoleNotFound         = errors.New("role not found")
	ErrBundleNotFound       = errors.New("permission bundle not found")
	ErrLinkNotFound         = errors.New("role-bundle link not found")
	ErrConflict             = errors.New("conflict: non-matching duplicate detected")
	ErrStoreUnavailable     = errors.New("store unavailable")
	ErrRoleDeactivated      = errors.New("role is deactivated")
)

// ── domain types ─────────────────────────────────────────────────────────────

// Role represents an access-control role definition owned by this service.
// Roles are multi-tenant; role_code is unique within a tenant.
//
// From data-model §6.1 Role entity.
type Role struct {
	RoleID               string    `json:"role_id"`
	TenantID             string    `json:"tenant_id"`
	RoleName             string    `json:"role_name"`
	RoleCode             string    `json:"role_code"`
	RoleScopeType        string    `json:"role_scope_type"`
	ActiveFlag           bool      `json:"active_flag"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// PermissionBundle represents a named, versioned set of permitted action strings
// that can be linked to one or more Roles.
//
// From data-model §6.1 PermissionBundle entity.
type PermissionBundle struct {
	PermissionBundleID string    `json:"permission_bundle_id"`
	TenantID           string    `json:"tenant_id"`
	BundleName         string    `json:"bundle_name"`
	BundleCode         string    `json:"bundle_code"`
	PermittedActions   []string  `json:"permitted_actions"`
	ActiveFlag         bool      `json:"active_flag"`
	CreatedAt          time.Time `json:"created_at"`
}

// RolePermissionBundleLink is the link record joining a Role to a
// PermissionBundle. A role may carry multiple bundles; a bundle may be
// shared across multiple roles.
type RolePermissionBundleLink struct {
	LinkID             string    `json:"link_id"`
	RoleID             string    `json:"role_id"`
	PermissionBundleID string    `json:"permission_bundle_id"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedBy          string    `json:"created_by"`
}

// ── parameter types (passed from handler → store) ────────────────────────────

type CreateRoleParams struct {
	RoleID               string // optional: caller-supplied idempotency key
	TenantID             string
	RoleCode             string
	RoleName             string
	RoleScopeType        string
	CreatedByPrincipalID string
}

type DeactivateRoleParams struct {
	RoleID string
}

type CreatePermissionBundleParams struct {
	PermissionBundleID string // optional: caller-supplied idempotency key
	TenantID           string
	BundleCode         string
	BundleName         string
	PermittedActions   []string
}

type UpdatePermissionBundleActionsParams struct {
	PermissionBundleID string
	PermittedActions   []string
}

type CreateRolePermissionBundleLinkParams struct {
	LinkID             string // optional
	RoleID             string
	PermissionBundleID string
	CreatedBy          string
}

type RemoveRolePermissionBundleLinkParams struct {
	RoleID             string
	PermissionBundleID string
}
