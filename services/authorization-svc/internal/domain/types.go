// Package domain contains the authoritative domain types for authorization-svc.
//
// role_scope_type, authorization_source_type, decision_outcome, and
// conflict_type are all plain strings — no Go enums, iota, or switch/case
// branches in validation logic. New values are added via data only, same
// doctrine as every other service in this platform. revocation_status IS a
// real (tiny) state machine: ACTIVE -> REVOKED, one-way, enforced in code.
package domain

import (
	"encoding/base64"
	"strings"
	"time"
)

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

// ── principal status ─────────────────────────────────────────────────────────

// PrincipalStatusProjection is the read-model of identity-context-svc's
// principal status, evaluated as LAYER 0 of /v1/authorize — before RBAC, and
// therefore before anything else can grant.
//
// DENY-ONLY, and ABSENT MEANS ACTIVE. A principal with no projected row
// evaluates exactly as it did before this layer existed, which is what lets
// the table ship empty (same shape as ABACRule). The layer can only ever
// remove access from a principal identity-context-svc has explicitly said is
// not active; it never confers anything.
//
// The status is per tenant because that is how identity-context-svc publishes
// it. See Store.FindPrincipalStatus for what a tenantless caller resolves,
// which is the case that matters most: most of this endpoint's callers send no
// tenant, and a gate they could escape by omitting a header would not be one.
type PrincipalStatusProjection struct {
	PrincipalID string `json:"principal_id"`
	TenantID    string `json:"tenant_id"`

	// Status is identity-context-svc's PrincipalStatus verbatim. Data only —
	// anything other than PrincipalStatusActive denies, INCLUDING a value this
	// build has never seen, which is the fail-closed direction for a status
	// nobody here can interpret.
	Status string `json:"status"`

	SourceService string `json:"source_service"`

	// StatusChangedAt is when upstream says the change happened, nil when the
	// payload did not carry it.
	StatusChangedAt *time.Time `json:"status_changed_at,omitempty"`
	ProjectedAt     time.Time  `json:"projected_at"`
}

// PrincipalStatusActive is the ONE status that permits evaluation to continue.
// Spelled as the allow-list rather than as a deny-list of SUSPENDED/DISABLED:
// identity-context-svc may add a fourth status, and a deny-list would silently
// admit it.
const PrincipalStatusActive = "ACTIVE"

// ProjectPrincipalStatusParams is the write shape used by the principal
// lifecycle consumer. An UPSERT on (tenant_id, principal_id) — the broker
// redelivers, and a status is a current value rather than an append-only
// history, so the latest event wins.
type ProjectPrincipalStatusParams struct {
	PrincipalID string
	// TenantID is required: the consumer refuses to project without one
	// rather than write a row no policy can match.
	TenantID        string
	Status          string
	SourceService   string
	StatusChangedAt *time.Time
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

// ListAccessDecisionsParams filters the decision log for
// GET /v1/access-decisions.
//
// WHY THIS EXISTS. Doc 03 §8.3 sets two evidence obligations on this service:
// "every decision logged with actor, action, basis, and outcome", and "denials
// must be evidentially retrievable". The first held from 000001. The second did
// not, because the only read was FindAccessDecisionByID — retrieval by primary
// key, which is retrieval only for somebody who already holds the key. A
// denial's id exists in one place: the response handed to the service that was
// refused. Auditing a denial therefore meant reading the CALLING service's logs
// to find a UUID to give back to this one, which is not what "evidentially
// retrievable" describes.
//
// Every field is optional; each one narrows. TenantID is NOT here — it comes
// from the caller's verified scope, never from a filter, so a filter can never
// widen the read past the tenant. See Store.ListAccessDecisions.
type ListAccessDecisionsParams struct {
	// PrincipalID narrows to one actor: "what has this principal been
	// refused". Served by 000009's (principal_id, decided_at DESC) index.
	PrincipalID string

	// Outcome narrows to GRANTED or DENIED. The reason 000012 exists: DENIED
	// is a small minority of a very large table, and it is the half the
	// evidence obligation names.
	Outcome string

	// ActionType and LegalEntityID narrow further. Neither is a leading
	// predicate — both are applied after tenant and date have already reduced
	// the scan — which is why 000012 deliberately indexes neither.
	ActionType    string
	LegalEntityID string

	// DecidedFrom / DecidedTo bound the window, inclusive-exclusive. Nil means
	// unbounded on that side. access_decision_log is partitioned by
	// decided_at, so a bounded window is also partition pruning.
	DecidedFrom *time.Time
	DecidedTo   *time.Time

	// Limit is capped by the store — see MaxAccessDecisionPageSize.
	Limit int

	// Cursor continues a previous page. Empty starts at the newest decision.
	Cursor AccessDecisionCursor
}

// AccessDecisionCursor is a keyset position in the decision log, ordered
// newest-first by (decided_at, access_decision_id).
//
// KEYSET, NOT OFFSET, and on this table that is not a preference. OFFSET makes
// the database walk and discard every skipped row, so page 200 of an audit
// costs 200 pages of work — on the largest table on the platform, across every
// monthly partition. Worse, the log is append-only and constantly appended to,
// so a row inserted during paging shifts every subsequent offset and an
// auditor silently never sees one decision while seeing another twice.
//
// A keyset position is stable against concurrent inserts: new rows are newer
// than the cursor and sort ahead of it, so they appear on a re-read of page one
// and never displace what a later page returns.
//
// access_decision_id is the tie-breaker because decided_at is not unique —
// /v1/authorize is called concurrently by 111 services and two decisions can
// share a timestamp. Ordering on the timestamp alone would let a page boundary
// fall between two rows with equal timestamps and drop or repeat one.
type AccessDecisionCursor struct {
	DecidedAt        time.Time
	AccessDecisionID string
}

// IsZero reports whether this cursor names no position, i.e. start from the
// newest decision.
func (c AccessDecisionCursor) IsZero() bool {
	return c.AccessDecisionID == "" || c.DecidedAt.IsZero()
}

// Encode renders the cursor as one opaque token for the wire.
//
// Opaque on purpose: it is a position, not an offset an API consumer should be
// composing by hand. The encoding is base64 of "RFC3339Nano|uuid", which is
// legible in a log when somebody has to debug a page boundary, without being a
// shape callers will start constructing.
func (c AccessDecisionCursor) Encode() string {
	if c.IsZero() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.DecidedAt.UTC().Format(time.RFC3339Nano) + "|" + c.AccessDecisionID))
}

// DecodeAccessDecisionCursor parses a token produced by
// AccessDecisionCursor.Encode.
//
// A malformed token is an error rather than a silent reset to page one:
// starting over when the caller asked to continue would hand an auditor the
// newest page while they believed they were reading the oldest, which is the
// kind of wrong that is not visible in the output.
func DecodeAccessDecisionCursor(token string) (AccessDecisionCursor, error) {
	if token == "" {
		return AccessDecisionCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return AccessDecisionCursor{}, ErrInvalidCursor
	}
	at, id, found := strings.Cut(string(raw), "|")
	if !found || at == "" || id == "" {
		return AccessDecisionCursor{}, ErrInvalidCursor
	}
	decidedAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return AccessDecisionCursor{}, ErrInvalidCursor
	}
	return AccessDecisionCursor{DecidedAt: decidedAt, AccessDecisionID: id}, nil
}

// AccessDecisionPage is one page of the decision log plus where to continue.
type AccessDecisionPage struct {
	Decisions []AccessDecisionLog `json:"decisions"`

	// NextCursor is empty when this is the last page. Its presence is the
	// only correct test for "there is more" — a full page is not proof, and a
	// short page is not proof of the end either once a filter is applied.
	NextCursor string `json:"next_cursor,omitempty"`
}

// The page-size bounds for ListAccessDecisions.
//
// MaxAccessDecisionPageSize is a CAP, not a suggestion: this is an
// unauthenticated-by-action read of the platform's audit trail, and an
// unbounded limit is how one console request scans a month of every service's
// decisions. A caller wanting more pages, not a bigger page, is the intended
// shape.
const (
	DefaultAccessDecisionPageSize = 50
	MaxAccessDecisionPageSize     = 200
)

// ErrInvalidCursor means the continuation token was not one this service
// issued. Refused rather than ignored — see DecodeAccessDecisionCursor.
var ErrInvalidCursor = errorString("invalid pagination cursor")

// ErrInvalidDecisionOutcomeFilter means the outcome filter named neither
// GRANTED nor DENIED. Refused rather than matching nothing, because a listing
// that is empty because of a typo looks exactly like a tenant with no
// decisions — and on an audit read those two must not be confusable.
var ErrInvalidDecisionOutcomeFilter = errorString("decision_outcome filter must be GRANTED or DENIED")

// ErrPrincipalStatusIncomplete means a status projection arrived without the
// principal or the status it is about. Refused rather than written: a row with
// an empty status would be read as not-ACTIVE by the gate and would deny that
// principal everything, from a malformed event.
var ErrPrincipalStatusIncomplete = errorString("principal status projection requires principal_id and status")

// ErrPrincipalStatusStale means a newer status for that principal is already
// stored, so the event was not applied. NOT an error condition for the
// consumer — see Store.ProjectPrincipalStatus. It exists so "did not apply
// because it was old" is distinguishable from "did not apply because the write
// failed", which are the same return value otherwise.
var ErrPrincipalStatusStale = errorString("a newer principal status is already projected")

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

// ErrSoDRuleNotFound means no sod_rules row with that id exists in the
// caller's tenant scope. A PLATFORM-WIDE rule (tenant_id NULL) reads as absent
// from any one tenant's scope on purpose — see Store.SetSoDRuleActive.
var ErrSoDRuleNotFound = errorString("sod rule not found")

// ErrABACRuleNotFound means no abac_rules row with that id exists in the
// caller's tenant scope.
var ErrABACRuleNotFound = errorString("abac rule not found")

// ErrPermissionBundleNotFound means no permission_bundles row with that id is
// reachable from the caller's tenant scope. permission_bundles carries no
// tenant_id of its own, so "reachable" means its role_id resolves to a role
// in that tenant — the same route 000007's policy takes.
var ErrPermissionBundleNotFound = errorString("permission bundle not found")

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
