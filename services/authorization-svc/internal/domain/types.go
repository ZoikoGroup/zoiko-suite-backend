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

	// TenantID is the tenant the delegation belongs to. NOT NULL in the schema
	// since 000006: a NULL tenant matches no policy, so such a row would be a
	// delegation that exists and can never grant anything.
	TenantID string `json:"tenant_id"`

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

	// DelegatedActions is the subset of the delegator's authority this
	// delegation confers. Nil means the delegator's FULL authority, which is
	// what every row written before migration 000008 means.
	//
	// It exists because ScopeType has always accepted "ACTION_SUBSET" and
	// nothing ever read it: a delegation recorded as a subset conferred the
	// delegator's entire grant set anyway. The subset is intersected with the
	// delegator's LIVE grants at evaluation time, so a delegation can never
	// confer an action the delegator does not currently hold — see
	// Store.FindDelegatedActions.
	DelegatedActions []string `json:"delegated_actions,omitempty"`

	// SourceService and SourceDelegationID are set only on a row PROJECTED
	// from the authoritative Delegated Authority Service's authority.*
	// events. Both nil means the delegation was authored through this
	// service's own admin API.
	//
	// Doc 03 §9.3 names delegated-authority-svc as the owner of the concept
	// (tracker item 81), and this is how the two stop being rival write
	// models: that service remains authoritative, and this table is the
	// evaluation read-model /v1/authorize resolves against.
	SourceService      *string `json:"source_service,omitempty"`
	SourceDelegationID *string `json:"source_delegation_id,omitempty"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	// RevocationStatus: ACTIVE | REVOKED. One-way transition.
	RevocationStatus string `json:"revocation_status"`

	CreatedAt time.Time `json:"created_at"`
}

// ── ABAC ─────────────────────────────────────────────────────────────────────

// ABACRule is one declared attribute condition guarding one action —
// evaluated as layer 5 of /v1/authorize, after RBAC, delegation, static SoD
// and own-object SoD have all already granted.
//
// DENY-ONLY. A rule can remove an action the earlier layers granted; it can
// never add one. That is what keeps the layers composable (RBAC answers "does
// the principal hold this", ABAC answers "may it be exercised here, now,
// against this") and it keeps a malformed rule narrowing access rather than
// widening it.
//
// The TABLE ships empty. Every concrete rule — which attribute, which
// threshold, which action — is a business decision this service has no
// standing to invent, so with no rows this layer is a no-op and /v1/authorize
// behaves exactly as it did before it existed. What is implemented here is the
// mechanism the spec assigns this service, not a guess at the policy.
type ABACRule struct {
	ABACRuleID string `json:"abac_rule_id"`

	// TenantID is nil for a rule that applies across every tenant — same
	// convention as SoDRule.TenantID.
	TenantID *string `json:"tenant_id"`

	// RuleCode is the stable identifier DecisionBasis names on a denial, so
	// the rule that caused it is findable from the log.
	RuleCode string `json:"rule_code"`

	// ActionType is the action this condition guards.
	ActionType string `json:"action_type"`

	// Effect is EffectRequire or EffectForbid — data only.
	Effect string `json:"effect"`

	// AttributeKey names the attribute the calling service sends in
	// /v1/authorize's `attributes` map.
	AttributeKey string `json:"attribute_key"`

	// Operator is data only; the evaluator implements the set named by
	// ABACOperators and refuses an unrecognised one at creation.
	Operator string `json:"operator"`

	// AttributeValue is the comparison operand, nil for the operators that
	// take none (exists / not_exists).
	AttributeValue *string `json:"attribute_value"`

	ActiveFlag           bool      `json:"active_flag"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// The two ABAC effects.
//
//	EffectRequire — the condition MUST hold, or the action is denied. An
//	                attribute the caller did not send therefore DENIES: a
//	                required condition that cannot be evaluated has not been
//	                satisfied, and treating absence as a pass would let any
//	                caller bypass a rule by omitting a JSON field.
//	EffectForbid  — the condition must NOT hold, or the action is denied. An
//	                absent attribute here PASSES, because a condition that
//	                cannot be met cannot be violated.
const (
	EffectRequire = "REQUIRE"
	EffectForbid  = "FORBID"
)

// ABACOperators is the set of comparison operators the evaluator implements.
//
// The column is VARCHAR and operators are data, same doctrine as
// role_scope_type and conflict_type — but unlike those, an operator has to be
// executed by code, so one the evaluator does not implement is refused when
// the rule is CREATED rather than discovered when a request is denied by it.
// The value is the number of operands the operator takes: 0 for the presence
// checks, 1 for everything else.
var ABACOperators = map[string]int{
	"eq":         1,
	"ne":         1,
	"in":         1,
	"not_in":     1,
	"lt":         1,
	"lte":        1,
	"gt":         1,
	"gte":        1,
	"contains":   1,
	"exists":     0,
	"not_exists": 0,
}

type CreateABACRuleParams struct {
	ABACRuleID string
	// TenantID is nil for a platform-wide rule, which the handler gates
	// behind the platform-scope grant.
	TenantID             *string
	RuleCode             string
	ActionType           string
	Effect               string
	AttributeKey         string
	Operator             string
	AttributeValue       *string
	CreatedByPrincipalID string
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

	// TenantID is the tenant the decision was made in scope of. Nullable
	// because /v1/authorize cannot require it without breaking every caller
	// that predates it — see RecordAccessDecisionParams.TenantID.
	TenantID *string `json:"tenant_id,omitempty"`

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
	// TenantID is the caller's VERIFIED tenant scope, from X-Tenant-Id.
	// Required — the store refuses rather than writing a row no policy can
	// match, behind the handler's own requireTenant.
	TenantID             string
	DelegatorPrincipalID string
	DelegatePrincipalID  string
	ScopeType            string
	// LegalEntityID is nil for a tenant-wide delegation.
	LegalEntityID       *string
	AuthorityLimitType  *string
	AuthorityLimitValue *string
	// DelegatedActions is the subset conferred. Nil means the delegator's
	// full authority — see DelegatedAuthority.DelegatedActions.
	DelegatedActions []string
	EffectiveFrom    time.Time
	EffectiveTo      *time.Time

	// SourceService / SourceDelegationID identify the upstream record when
	// this row is projected from delegated-authority-svc's events rather
	// than authored here. Both empty for a locally-authored delegation.
	SourceService      string
	SourceDelegationID string
}

// ProjectDelegationParams is the write shape used by the authority.* event
// consumer. Distinct from CreateDelegatedAuthorityParams because a projection
// is an UPSERT keyed on the UPSTREAM id — the broker redelivers, and a
// consumer that inserted on every delivery would multiply one delegation into
// several rows that /v1/authorize would then union.
type ProjectDelegationParams struct {
	SourceService        string
	SourceDelegationID   string
	TenantID             string
	DelegatorPrincipalID string
	DelegatePrincipalID  string
	// LegalEntityID is nil for a tenant-wide delegation.
	LegalEntityID *string
	// DelegatedActions is the action set the upstream event named. Upstream
	// delegates ONE action per grant, so this normally holds exactly one —
	// which is precisely the subset case that had no representation in this
	// table before 000008.
	DelegatedActions []string
	EffectiveFrom    time.Time
	EffectiveTo      *time.Time
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

// RecordAccessDecisionParams is the write shape for access_decision_log.
// A struct rather than seven positional strings, because tenant_id was the
// seventh and adding it positionally is exactly how a caller ends up passing
// the correlation ID as the tenant.
type RecordAccessDecisionParams struct {
	PrincipalID   string
	LegalEntityID string
	ActionType    string
	Outcome       string
	Basis         string
	CorrelationID string

	// TenantID is the caller's verified tenant scope, empty when the caller
	// supplied none. Stored as SQL NULL when empty: /v1/authorize is called
	// by ~60 services and most do not send a tenant yet, so requiring one
	// would deny every one of those callers rather than record an
	// unattributed decision. A NULL-tenant row is deliberately NOT readable
	// through GET /v1/access-decisions/{id}, which is tenant-scoped.
	TenantID string
}

var ErrRoleNotFound = errorString("role not found")
var ErrRoleAssignmentNotFound = errorString("role assignment not found")
var ErrLegalEntityRequiredForRoleScope = errorString("legal_entity_id is required: this role's scope_type is not TENANT")
var ErrDelegatedAuthorityNotFound = errorString("delegated authority not found")

// ErrTenantScopeRequired means a delegation operation was attempted without a
// verified tenant. Since 000006 delegated_authorities.tenant_id is NOT NULL and
// every read carries a tenant predicate, so a tenantless call could only either
// write a row no policy can match or read across tenants. Refusing is the only
// correct outcome — this is a defence-in-depth guard behind the handler's own
// requireTenant, not the primary check.
var ErrTenantScopeRequired = errorString("delegation operations require a verified tenant scope")
var ErrAccessDecisionNotFound = errorString("access decision not found")

// ErrProjectionSourceRequired means a projection write arrived without the
// upstream (service, id) pair it is keyed on. Such a row could not be
// deduplicated on redelivery or revoked by a later event, so it is refused
// rather than written — see Store.ProjectDelegation.
var ErrProjectionSourceRequired = errorString("projected delegation requires source_service and source_delegation_id")

// ErrABACRuleNotFound means no abac_rules row with that id exists in the
// caller's tenant scope.
var ErrABACRuleNotFound = errorString("abac rule not found")

// ErrUnsupportedABACOperator means the rule named an operator the evaluator
// does not implement. Refused at creation (400) rather than discovered when a
// request is denied by a rule nobody can evaluate — see ABACOperators.
var ErrUnsupportedABACOperator = errorString("unsupported abac operator")

// ErrABACOperandRequired means a comparison operator was given no operand to
// compare against. Under REQUIRE such a rule denies every request for the
// action; under FORBID it permits every one. Both are silent, so the rule is
// refused rather than stored.
var ErrABACOperandRequired = errorString("this abac operator requires attribute_value")

// ErrUnsupportedABACEffect means the rule named neither REQUIRE nor FORBID.
// There is no third, safe interpretation of an unknown effect on a deny-only
// layer, so it is refused at creation.
var ErrUnsupportedABACEffect = errorString("unsupported abac effect: expected REQUIRE or FORBID")
var ErrInvalidTransition = errorString("invalid revocation status transition")
var ErrConflict = errorString("conflict: record already exists with differing attributes")
var ErrStoreUnavailable = errorString("authorization store unavailable")
var ErrJurisdictionNotFound = errorString("jurisdiction not found")
var ErrJurisdictionServiceUnavailable = errorString("jurisdiction-rules-svc unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
