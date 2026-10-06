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

import (
	"encoding/json"
	"strings"
	"time"
)

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
	// TemplateCode / TemplateVersion are set on a role instantiated from a
	// system role template (§9): its provenance, and the version its
	// template-managed bundle carries. Empty / 0 for a tenant custom role.
	TemplateCode    string `json:"template_code,omitempty"`
	TemplateVersion int    `json:"template_version,omitempty"`
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
	// TemplateCode marks the bundle a role template manages. Its actions are
	// the template version's, and they change only by a template upgrade.
	TemplateCode    string `json:"template_code,omitempty"`
	TemplateVersion int    `json:"template_version,omitempty"`
}

// ProtectedPermission is a platform-admin action that tenant roles may not include.
type ProtectedPermission struct {
	ActionName  string    `json:"action_name"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	ActiveFlag  bool      `json:"active_flag"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RefusedEscalation records a pre-provisioning refusal for evidence/audit.
// Per Doc 04 §20: "denials are as important as grants".
type RefusedEscalation struct {
	RefusedEscalationID string          `json:"refused_escalation_id"`
	TenantID            string          `json:"tenant_id"`
	LegalEntityID       string          `json:"legal_entity_id"`
	PrincipalID         string          `json:"principal_id"`
	CorrelationID       string          `json:"correlation_id"`
	ActionType          string          `json:"action_type"`
	RefusalReason       string          `json:"refusal_reason"`
	RequestedPayload    json.RawMessage `json:"requested_payload"`
	ErrorCode           string          `json:"error_code"`
	ErrorMessage        string          `json:"error_message"`
	CreatedAt           time.Time       `json:"created_at"`
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

// SoDCheckRequest asks authorization-svc whether a role's action set is
// internally conflicted (POST /v1/sod/validate, candidate-only form).
//
// CandidateActions is the WHOLE set the role would grant once the write lands:
// its other active bundles plus the bundle being written. A conflict split
// across two bundles of one role is still a conflict for everyone who holds
// the role, so checking the new bundle alone would miss exactly the case a
// second bundle is most often used to sneak in.
//
// No principal is sent in the body. Defining a role assigns it to nobody, so
// the question is "is this role conflicted", not "may this caller hold it";
// the caller rides X-Principal-Id only as the request's attribution.
type SoDCheckRequest struct {
	TenantID         string
	CallerID         string
	CorrelationID    string
	CandidateActions []string

	// SubjectPrincipalID + LegalEntityID ask the principal-aware form: the
	// candidates are checked against what the subject already holds in that
	// entity. Set for an assignment, empty for a role definition.
	SubjectPrincipalID string
	LegalEntityID      string
}

// SoDConflict is one conflicting pair as authorization-svc reports it.
type SoDConflict struct {
	CandidateAction string `json:"candidate_action"`
	ConflictsWith   string `json:"conflicts_with"`
	Source          string `json:"source"`
}

// SoDConflictError carries the pairs behind a refusal, so the caller is told
// which actions to split rather than only that something conflicted.
// errors.Is(err, ErrSoDConflict) holds for it.
type SoDConflictError struct{ Conflicts []SoDConflict }

func (e *SoDConflictError) Error() string {
	if len(e.Conflicts) == 0 {
		return string(ErrSoDConflict)
	}
	pairs := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		pairs = append(pairs, c.CandidateAction+" conflicts with "+c.ConflictsWith)
	}
	return string(ErrSoDConflict) + ": " + strings.Join(pairs, "; ")
}

func (e *SoDConflictError) Is(target error) bool { return target == ErrSoDConflict }

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

	// ErrRoleCodeExists is returned when a create names a role_code another
	// definition in this tenant already holds.
	//
	// It used to surface as ErrStoreUnavailable -- the UNIQUE (tenant_id,
	// role_code) violation reached the caller as 503 "store unavailable",
	// pointing on-call at a database that was working correctly and refusing a
	// request that was simply a duplicate. Worse, the refusal happened AFTER
	// the role had been provisioned into authorization-svc, so a rejected
	// create left a role there that this register did not record.
	ErrRoleCodeExists = errorString("a role definition with that role_code already exists in this tenant")

	// ErrBundleCodeExists is returned when a create names a bundle_code the
	// same role already carries.
	//
	// This is not a cosmetic uniqueness rule. authorization-svc identifies a
	// bundle by (role_id, bundle_code) and its attach endpoint is an
	// upsert-replace on that pair, so a second local bundle sharing a code
	// silently REPLACED the first one's permitted actions there while both
	// rows stayed ACTIVE here -- and detaching either one retired the single
	// remote bundle both of them pointed at. The register then showed an
	// ACTIVE bundle granting nothing.
	ErrBundleCodeExists = errorString("a permission bundle with that bundle_code is already attached to this role")

	// ErrProtectedAction is returned when a role or bundle includes an action
	// that is reserved for platform-admin roles (per Authorization Standard §9).
	ErrProtectedAction = errorString("permitted_actions includes protected platform-admin action(s); tenant roles may not include these")

	// ErrSoDConflict is returned when a role or bundle would create a
	// segregation-of-duties violation (Authorization Standard §10.1).
	ErrSoDConflict = errorString("segregation of duties conflict: the role's permitted actions conflict with each other")

	// ErrSoDUnavailable is returned when authorization-svc's SoD check cannot
	// be read: unreachable, non-200, or a body without a verdict. Fail closed.
	// This used to be indistinguishable from a conflict (every non-2xx was a
	// generic error, every error a 403 sod_conflict), and a 200 carrying
	// conflict_free:false counted as a pass.
	ErrSoDUnavailable = errorString("segregation-of-duties check unavailable")

	// ErrProtectedCatalogueUnavailable is returned when the protected-action
	// catalogue cannot be read, or reads empty. Fail closed: an empty list
	// used to mean "no check", so a missing catalogue let every platform-admin
	// action into a tenant bundle.
	ErrProtectedCatalogueUnavailable = errorString("protected-action catalogue unavailable")

	// ErrProvisioningForbidden is returned when authorization-svc's admin API
	// refuses the caller (403). It is a refusal, not an outage: the caller
	// lacks the iam.* grant authorization-svc requires for the write.
	ErrProvisioningForbidden = errorString("authorization-svc refused the provisioning call for this caller")
)
