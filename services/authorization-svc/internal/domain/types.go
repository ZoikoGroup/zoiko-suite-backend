// Package domain contains the authoritative domain types for authorization-svc.
//
// role_scope_type, authorization_source_type, decision_outcome, and
// conflict_type are all plain strings — no Go enums, iota, or switch/case
// branches in validation logic. New values are added via data only, same
// doctrine as every other service in this platform. revocation_status IS a
// real (tiny) state machine: ACTIVE -> REVOKED, one-way, enforced in code.
package domain

import "time"

// Role is a tenant-scoped grantable role. No hard-delete: a role is
// deactivated via ActiveFlag, never removed — role assignments referencing
// it must remain resolvable for audit history.
type Role struct {
	RoleID   string `json:"role_id"`
	TenantID string `json:"tenant_id"`

	// RoleCode is a stable, human-readable identifier and the idempotent
	// creation dedup key (unique within a tenant) — DATA ONLY.
	RoleCode string `json:"role_code"`
	RoleName string `json:"role_name"`

	// RoleScopeType is data only (e.g. "TENANT", "LEGAL_ENTITY").
	RoleScopeType string `json:"role_scope_type"`

	ActiveFlag bool `json:"active_flag"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// PermissionBundle is the set of actions a Role grants. One role may own
// multiple bundles (e.g. versioned or split by domain); the evaluation
// engine unions every active bundle attached to a role.
type PermissionBundle struct {
	PermissionBundleID string    `json:"permission_bundle_id"`
	RoleID             string    `json:"role_id"`
	BundleCode         string    `json:"bundle_code"`
	PermittedActions   []string  `json:"permitted_actions"`
	ActiveFlag         bool      `json:"active_flag"`
	CreatedAt          time.Time `json:"created_at"`
}

// PrincipalRoleAssignment grants a Role to a principal, effective-dated,
// scoped either to one legal entity or — when LegalEntityID is nil — to the
// whole tenant (only legal for a role whose RoleScopeType is "TENANT"; see
// Store.CreateRoleAssignment). No hard-delete: ending an assignment sets
// EffectiveTo, never removes the row — see Store.RevokeRoleAssignment.
type PrincipalRoleAssignment struct {
	PrincipalRoleAssignmentID string `json:"principal_role_assignment_id"`

	PrincipalID   string  `json:"principal_id"`
	RoleID        string  `json:"role_id"`
	LegalEntityID *string `json:"legal_entity_id"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	AssignedBy string    `json:"assigned_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// DelegatedAuthority grants a delegate principal the ability to act within
// the scope of the delegator's own grants. Revocation is a one-way state
// machine (ACTIVE -> REVOKED) — never deleted, never re-activated once
// revoked, matching the platform's evidentiary requirements around access.
type DelegatedAuthority struct {
	DelegatedAuthorityID string `json:"delegated_authority_id"`

	DelegatorPrincipalID string `json:"delegator_principal_id"`
	DelegatePrincipalID  string `json:"delegate_principal_id"`

	// ScopeType is data only (e.g. "FULL", "ACTION_SUBSET").
	ScopeType string `json:"scope_type"`
	// LegalEntityID is nil when the delegation applies across the whole
	// tenant rather than one entity.
	LegalEntityID *string `json:"legal_entity_id"`

	// AuthorityLimitType/AuthorityLimitValue are optional (e.g. "AMOUNT_CAP" / "5000").
	AuthorityLimitType  *string `json:"authority_limit_type"`
	AuthorityLimitValue *string `json:"authority_limit_value"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	// RevocationStatus: ACTIVE | REVOKED. One-way transition.
	RevocationStatus string `json:"revocation_status"`

	CreatedAt time.Time `json:"created_at"`
}

// SoDRule expresses a Separation-of-Duties conflict: a principal holding a
// grant for ActionA must not also be granted ActionB (within the same
// domain, optionally scoped to one jurisdiction).
type SoDRule struct {
	SoDRuleID string `json:"sod_rule_id"`

	DomainCode string `json:"domain_code"`
	ActionA    string `json:"action_a"`
	ActionB    string `json:"action_b"`

	// ConflictType is data only (e.g. "MUTUALLY_EXCLUSIVE").
	ConflictType string `json:"conflict_type"`

	// JurisdictionID is nil for a globally-applicable rule.
	JurisdictionID *string `json:"jurisdiction_id"`

	// TenantID is nil for a rule that applies across every tenant. A
	// non-nil value scopes the rule to one tenant only — mirrors
	// JurisdictionID's own NULL = global convention on this same table.
	TenantID *string `json:"tenant_id"`

	ActiveFlag bool      `json:"active_flag"`
	CreatedAt  time.Time `json:"created_at"`
}

// ConflictTypeOwnObjectForbidden is the conflict_type convention for a
// dynamic, own-object Separation-of-Duties rule (ZS-IAM-001 §10.2): a
// principal who prepared/owns a resource must not also perform ActionType
// against that same resource (the spec's own example: "a preparer cannot
// approve their own object" — resource.preparer_id == subject_id AND
// action == approve → DENY). Expressed as a self-referential SoDRule row
// (ActionA == ActionB == the guarded action) rather than a new table or
// column: conflict_type is already documented as data only, so a new
// value needs no schema change, and CheckSoDConflict's existing
// held-actions-pair query cannot accidentally match it — that query
// excludes the candidate action from the caller's "other held actions"
// set before searching, so a self-referential row is invisible to it. The
// evaluation path for this convention is CheckOwnObjectSoD, a distinct
// query keyed on the action alone.
const ConflictTypeOwnObjectForbidden = "OWN_OBJECT_FORBIDDEN"

// AccessDecisionLog is the append-only evidence record for every
// authorization evaluation — grant or deny. Critical constraint: "no
// material action executes without an authorization decision artifact."
// Never updated or deleted once written.
type AccessDecisionLog struct {
	AccessDecisionID string `json:"access_decision_id"`

	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`

	// DecisionOutcome: GRANTED | DENIED.
	DecisionOutcome string `json:"decision_outcome"`

	// DecisionBasis is a human-readable explanation of which layer produced
	// the outcome (e.g. "rbac:role=FINANCE_APPROVER", "sod:conflict with
	// PAYMENT_INITIATE", "no_grant") — never just "denied" with no reason.
	DecisionBasis string `json:"decision_basis"`

	CorrelationID string    `json:"correlation_id"`
	DecidedAt     time.Time `json:"decided_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateRoleParams struct {
	RoleID               string
	TenantID             string
	RoleCode             string
	RoleName             string
	RoleScopeType        string
	CreatedByPrincipalID string
}

type CreatePermissionBundleParams struct {
	PermissionBundleID string
	RoleID             string
	BundleCode         string
	PermittedActions   []string
}

type CreateRoleAssignmentParams struct {
	PrincipalRoleAssignmentID string
	PrincipalID               string
	RoleID                    string
	// LegalEntityID is nil for a tenant-wide assignment — only accepted
	// when the target role's RoleScopeType is "TENANT"; see
	// Store.CreateRoleAssignment.
	LegalEntityID *string
	EffectiveFrom time.Time
	AssignedBy    string
}

type CreateDelegatedAuthorityParams struct {
	DelegatedAuthorityID string
	DelegatorPrincipalID string
	DelegatePrincipalID  string
	ScopeType            string
	// LegalEntityID is nil for a tenant-wide delegation.
	LegalEntityID       *string
	AuthorityLimitType  *string
	AuthorityLimitValue *string
	EffectiveFrom       time.Time
	EffectiveTo         *time.Time
}

type CreateSoDRuleParams struct {
	SoDRuleID      string
	DomainCode     string
	ActionA        string
	ActionB        string
	ConflictType   string
	JurisdictionID *string
	TenantID       *string
}

// EvaluateParams holds input for the core authorization evaluation.
type EvaluateParams struct {
	PrincipalID   string
	LegalEntityID string
	ActionType    string
	CorrelationID string
	// TenantID is optional — omit it to preserve today's behavior (only
	// globally-applicable SoD rules are considered). Callers that supply
	// it also get tenant-scoped SoD rules evaluated.
	TenantID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var ErrRoleNotFound = errorString("role not found")
var ErrRoleAssignmentNotFound = errorString("role assignment not found")
var ErrLegalEntityRequiredForRoleScope = errorString("legal_entity_id is required: this role's scope_type is not TENANT")
var ErrDelegatedAuthorityNotFound = errorString("delegated authority not found")
var ErrAccessDecisionNotFound = errorString("access decision not found")
var ErrInvalidTransition = errorString("invalid revocation status transition")
var ErrConflict = errorString("conflict: record already exists with differing attributes")
var ErrStoreUnavailable = errorString("authorization store unavailable")
var ErrJurisdictionNotFound = errorString("jurisdiction not found")
var ErrJurisdictionServiceUnavailable = errorString("jurisdiction-rules-svc unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
