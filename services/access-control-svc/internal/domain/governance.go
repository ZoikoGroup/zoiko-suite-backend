package domain

import "time"

// The governance surface added on 6 Oct 2026 to close the Authorization
// Standard §5 / §9 / §24 gaps the 23 Sep audit recorded as "unbuilt":
//
//	permission taxonomy   §5, §22 permission_definition   (migration 000008)
//	system role templates §9, §9.1                        (000009)
//	assignment requests   §9, §21                         (000010)
//	access reviews        §9, §24                         (000011)
//
// Ownership, stated once. authorization-svc remains the enforcement store: it
// holds roles, bundles and principal_role_assignments and answers every
// decision. This service is the governed AUTHORING layer in front of it (§22's
// "IAM Policy" and "IAM Governance" owners): every write here is provisioned
// there before it is recorded here, and nothing here grants anything on its
// own.

// Risk tiers, from permission_definitions.risk_tier (000008).
const (
	RiskStandard = "STANDARD"
	RiskHigh     = "HIGH"
	RiskCritical = "CRITICAL"
)

// RiskRank orders the tiers so the highest of a set can be taken.
func RiskRank(tier string) int {
	switch tier {
	case RiskCritical:
		return 2
	case RiskHigh:
		return 1
	default:
		return 0
	}
}

// PermissionDefinition is one registered, stable capability (§5).
type PermissionDefinition struct {
	ActionName  string `json:"action_name"`
	Naming      string `json:"naming"` // TAXONOMY | LEGACY
	RiskTier    string `json:"risk_tier"`
	Description string `json:"description"`
	Protected   bool   `json:"protected"`
}

// RoleTemplate is a ZoikoSuite-maintained role (§9 "System role template").
type RoleTemplate struct {
	TemplateCode  string   `json:"template_code"`
	TemplateName  string   `json:"template_name"`
	DefaultIntent string   `json:"default_intent"`
	RoleScopeType string   `json:"role_scope_type"`
	IsArchetype   bool     `json:"is_archetype"`
	Status        string   `json:"status"`
	LatestVersion int      `json:"latest_version"`
	LatestActions []string `json:"latest_permitted_actions"`
}

// RoleTemplateVersion is one immutable published version.
type RoleTemplateVersion struct {
	TemplateCode     string    `json:"template_code"`
	TemplateVersion  int       `json:"template_version"`
	PermittedActions []string  `json:"permitted_actions"`
	ChangeNote       string    `json:"change_note"`
	PublishedBy      string    `json:"published_by"`
	PublishedAt      time.Time `json:"published_at"`
}

type InstantiateTemplateRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	RoleCode        string `json:"role_code,omitempty"`
	RoleName        string `json:"role_name,omitempty"`
	TemplateVersion int    `json:"template_version,omitempty"`
	CorrelationID   string `json:"correlation_id"`
}

type TemplateUpgradeRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	TemplateVersion int    `json:"template_version"`
	CorrelationID   string `json:"correlation_id"`
}

// InstantiatedRole is the answer to an instantiation: the role and the one
// template-managed bundle it was created with.
type InstantiatedRole struct {
	Role   RoleDefinition      `json:"role"`
	Bundle PermissionBundleDef `json:"bundle"`
}

// ── assignment requests ─────────────────────────────────────────────────────

const (
	AssignmentPendingApproval = "PENDING_APPROVAL"
	AssignmentProvisioned     = "PROVISIONED"
	AssignmentRejected        = "REJECTED"
	AssignmentCancelled       = "CANCELLED"
	AssignmentRevoked         = "REVOKED"
	// AssignmentExpired: the assignment reached its effective_to (an end date
	// set at grant time) and the expiry sweep closed it.
	AssignmentExpired = "EXPIRED"
)

// ActionApprovePrivileged is the authority a CRITICAL-risk assignment request
// needs from its approver, on top of ROLE_MANAGE (§9: approval by "manager /
// data-owner / security ... depending on risk"). A HIGH request is approved by
// any independent role manager; a CRITICAL one by security.
const ActionApprovePrivileged = "iam.assignment.approve_privileged"

type AssignmentRequest struct {
	RequestID         string    `json:"request_id"`
	TenantID          string    `json:"tenant_id"`
	TargetPrincipalID string    `json:"target_principal_id"`
	RoleDefinitionID  string    `json:"role_definition_id"`
	LegalEntityID     string    `json:"legal_entity_id"`
	EffectiveFrom     time.Time `json:"effective_from"`
	// EffectiveTo is the assignment's end: set at grant time, or by an
	// effective-dated revoke. nil is open-ended.
	EffectiveTo            *time.Time `json:"effective_to,omitempty"`
	Justification          string     `json:"justification"`
	RiskTier               string     `json:"risk_tier"`
	ApprovalRequired       bool       `json:"approval_required"`
	ApprovalReason         string     `json:"approval_reason,omitempty"`
	Status                 string     `json:"status"`
	RequestedByPrincipalID string     `json:"requested_by_principal_id"`
	DecidedByPrincipalID   string     `json:"decided_by_principal_id,omitempty"`
	DecisionReason         string     `json:"decision_reason,omitempty"`
	DecidedAt              *time.Time `json:"decided_at,omitempty"`
	AuthzAssignmentID      string     `json:"authz_assignment_id,omitempty"`
	// GroupAssignmentID links a member's request to the group assignment
	// that fanned it out (migration 000014); empty for an individual request.
	GroupAssignmentID    string     `json:"group_assignment_id,omitempty"`
	RevokedByPrincipalID string     `json:"revoked_by_principal_id,omitempty"`
	RevocationReason     string     `json:"revocation_reason,omitempty"`
	RevokedAt            *time.Time `json:"revoked_at,omitempty"`
	CorrelationID        string     `json:"correlation_id"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type CreateAssignmentRequest struct {
	LegalEntityID     string     `json:"legal_entity_id"`
	TargetPrincipalID string     `json:"target_principal_id"`
	RoleDefinitionID  string     `json:"role_definition_id"`
	EffectiveFrom     time.Time  `json:"effective_from,omitempty"`
	EffectiveTo       *time.Time `json:"effective_to,omitempty"`
	Justification     string     `json:"justification"`
	CorrelationID     string     `json:"correlation_id"`
}

// AssignmentDecisionRequest carries approve / reject / cancel / revoke.
// EffectiveAt applies to revoke only: a future instant schedules the end
// (§9 "effective-dated removal"); omitted, the revoke is immediate.
type AssignmentDecisionRequest struct {
	LegalEntityID string     `json:"legal_entity_id"`
	Reason        string     `json:"reason"`
	EffectiveAt   *time.Time `json:"effective_at,omitempty"`
	CorrelationID string     `json:"correlation_id"`
}

type AssignmentListFilter struct {
	Status            string
	TargetPrincipalID string
	Limit             int
	Offset            int
}

// AuthzAssignment mirrors authorization-svc's principal_role_assignments row.
type AuthzAssignment struct {
	PrincipalRoleAssignmentID string     `json:"principal_role_assignment_id"`
	PrincipalID               string     `json:"principal_id"`
	RoleID                    string     `json:"role_id"`
	LegalEntityID             *string    `json:"legal_entity_id"`
	EffectiveFrom             time.Time  `json:"effective_from"`
	EffectiveTo               *time.Time `json:"effective_to"`
	AssignedBy                string     `json:"assigned_by"`
	CreatedAt                 time.Time  `json:"created_at"`
	// Read with include_usage=true, for reviews only.
	LastGrantedAt   *time.Time `json:"last_granted_at,omitempty"`
	PrincipalStatus string     `json:"principal_status,omitempty"`
	// authorization-svc's maker-checker state (its 000025): APPROVED grants;
	// PENDING_APPROVAL waits for its own checker and grants nothing.
	ApprovalStatus string `json:"approval_status,omitempty"`
}

// ── access reviews ──────────────────────────────────────────────────────────

const (
	ReviewItemOpen      = "OPEN"
	ReviewItemDecided   = "DECIDED"
	ReviewItemEscalated = "ESCALATED"
	ReviewItemExpired   = "EXPIRED"

	CampaignOpen      = "OPEN"
	CampaignCompleted = "COMPLETED"

	DecisionKeep     = "KEEP"
	DecisionRevoke   = "REVOKE"
	DecisionModify   = "MODIFY"
	DecisionEscalate = "ESCALATE"

	FlagOrphanedRole         = "ORPHANED_ROLE"
	FlagSoDConflict          = "SOD_CONFLICT"
	FlagSelfReviewReassigned = "SELF_REVIEW_REASSIGNED"
	// FlagDormant (§24 "Dormancy"): a HIGH/CRITICAL assignment older than the
	// campaign's dormancy window with no GRANTED decision on record inside it.
	// Flagged for removal, not removed: "usage alone does not prove necessity".
	FlagDormant = "DORMANT"
	// FlagSubjectInactive (§24 "Orphan detection": an assignment "without valid
	// owner"): the subject is SUSPENDED or DISABLED. Raises the item to HIGH,
	// so the campaign cannot close over it undecided.
	FlagSubjectInactive = "SUBJECT_INACTIVE"

	// DefaultDormancyDays is the dormancy window when a campaign names none.
	DefaultDormancyDays = 90
)

type ReviewCampaign struct {
	CampaignID                 string         `json:"campaign_id"`
	TenantID                   string         `json:"tenant_id"`
	CampaignName               string         `json:"campaign_name"`
	ReviewType                 string         `json:"review_type"`
	TriggerReason              string         `json:"trigger_reason,omitempty"`
	LegalEntityID              string         `json:"legal_entity_id"`
	DefaultReviewerPrincipalID string         `json:"default_reviewer_principal_id"`
	Status                     string         `json:"status"`
	DueAt                      time.Time      `json:"due_at"`
	DormancyDays               int            `json:"dormancy_days"`
	CreatedByPrincipalID       string         `json:"created_by_principal_id"`
	CompletedByPrincipalID     string         `json:"completed_by_principal_id,omitempty"`
	CompletedAt                *time.Time     `json:"completed_at,omitempty"`
	CorrelationID              string         `json:"correlation_id"`
	CreatedAt                  time.Time      `json:"created_at"`
	UpdatedAt                  time.Time      `json:"updated_at"`
	Items                      []ReviewItem   `json:"items,omitempty"`
	Summary                    map[string]int `json:"summary,omitempty"`
}

type ReviewItem struct {
	ItemID               string     `json:"item_id"`
	CampaignID           string     `json:"campaign_id"`
	TenantID             string     `json:"tenant_id"`
	AuthzAssignmentID    string     `json:"authz_assignment_id"`
	TargetPrincipalID    string     `json:"target_principal_id"`
	RoleDefinitionID     string     `json:"role_definition_id"`
	RoleCode             string     `json:"role_code"`
	LegalEntityID        string     `json:"legal_entity_id,omitempty"`
	GrantedActions       []string   `json:"granted_actions"`
	RiskTier             string     `json:"risk_tier"`
	Flags                []string   `json:"flags"`
	ReviewerPrincipalID  string     `json:"reviewer_principal_id"`
	Status               string     `json:"status"`
	Decision             string     `json:"decision,omitempty"`
	DecisionReason       string     `json:"decision_reason,omitempty"`
	DecidedByPrincipalID string     `json:"decided_by_principal_id,omitempty"`
	DecidedAt            *time.Time `json:"decided_at,omitempty"`
	RevocationApplied    bool       `json:"revocation_applied"`
	// Evidence for the DORMANT / SUBJECT_INACTIVE flags, as read at snapshot.
	LastGrantedAt *time.Time `json:"last_granted_at,omitempty"`
	SubjectStatus string     `json:"subject_status,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type CreateCampaignRequest struct {
	LegalEntityID              string `json:"legal_entity_id"`
	CampaignName               string `json:"campaign_name"`
	ReviewType                 string `json:"review_type"`
	TriggerReason              string `json:"trigger_reason,omitempty"`
	DefaultReviewerPrincipalID string `json:"default_reviewer_principal_id"`
	// EscalationReviewerPrincipalID reviews the items whose subject is the
	// default reviewer (no self-attestation). Required only when that happens.
	EscalationReviewerPrincipalID string   `json:"escalation_reviewer_principal_id,omitempty"`
	RoleDefinitionIDs             []string `json:"role_definition_ids,omitempty"`
	// SubjectPrincipalID narrows the campaign to one subject's assignments:
	// an EVENT_TRIGGERED review of a mover or leaver (§24).
	SubjectPrincipalID string    `json:"subject_principal_id,omitempty"`
	DueAt              time.Time `json:"due_at"`
	// DormancyDays is the §24 dormancy window; 0 means DefaultDormancyDays.
	DormancyDays  int    `json:"dormancy_days,omitempty"`
	CorrelationID string `json:"correlation_id"`
}

type DecideItemRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	Decision      string `json:"decision"`
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id"`
}

type ReassignItemRequest struct {
	LegalEntityID       string `json:"legal_entity_id"`
	ReviewerPrincipalID string `json:"reviewer_principal_id"`
	CorrelationID       string `json:"correlation_id"`
}

type CompleteCampaignRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	CorrelationID string `json:"correlation_id"`
}

// ── errors ──────────────────────────────────────────────────────────────────

var (
	ErrUnknownPermission       = errorString("permitted_actions includes action(s) not registered in the permission taxonomy")
	ErrTemplateNotFound        = errorString("role template not found")
	ErrTemplateVersionNotFound = errorString("role template version not found")
	ErrTemplateManagedBundle   = errorString("this bundle is managed by its role template; change it by upgrading the role to a new template version")
	ErrNotTemplateRole         = errorString("this role was not instantiated from a role template")
	ErrTemplateVersionNotNewer = errorString("template_version must be newer than the role's current version")

	ErrAssignmentNotFound    = errorString("assignment request not found")
	ErrAssignmentState       = errorString("the assignment request is not in a state that allows this transition")
	ErrSelfApproval          = errorString("the requester or the subject of an assignment cannot approve it")
	ErrRoleRetired           = errorString("a retired role cannot be assigned")
	ErrAuthzAssignmentAbsent = errorString("the assignment no longer exists in authorization-svc")
	// ErrAuthzApprovalPending: authorization-svc recorded the assignment but
	// parked it PENDING_APPROVAL — the provisioning principal is not an
	// independent approver it recognises for a privileged role there. Nothing
	// is in force, so this service must not record it as provisioned.
	ErrAuthzApprovalPending = errorString("authorization-svc holds the assignment pending its own approval; it is not in force")
	ErrEntityMismatch       = errorString("legal_entity_id must be the entity the record belongs to")
	ErrInvalidEffectiveTo   = errorString("effective_to must be after effective_from and in the future")
	ErrInvalidEffectiveAt   = errorString("effective_at must be in the future; omit it to revoke now")
	ErrAssignmentWindowOver = errorString("the requested assignment's effective_to has passed; it can no longer be approved")
	ErrSecurityApproval     = errorString("a CRITICAL-risk assignment needs a security approver holding " + ActionApprovePrivileged)

	ErrCampaignNotFound      = errorString("access review campaign not found")
	ErrReviewItemNotFound    = errorString("access review item not found")
	ErrCampaignClosed        = errorString("the access review campaign is already completed")
	ErrNotItemReviewer       = errorString("this access review item is assigned to another reviewer")
	ErrSelfAttestation       = errorString("the subject of an access review item cannot review it")
	ErrUnresolvedHighRisk    = errorString("high-risk access review items are undecided or escalated; the campaign cannot close")
	ErrItemAlreadyDecided    = errorString("this access review item is already decided")
	ErrEscalationReviewerReq = errorString("the default reviewer is the subject of at least one assignment; supply escalation_reviewer_principal_id")
	ErrAssignmentPending     = errorString("a request for this principal, role and entity is already awaiting approval")

	ErrGroupNotFound           = errorString("group not found")
	ErrGroupCodeExists         = errorString("a group with that group_code already exists in this tenant")
	ErrGroupRetired            = errorString("the group is retired")
	ErrGroupMemberExists       = errorString("the principal is already a member of this group")
	ErrGroupMemberNotFound     = errorString("the principal is not a member of this group")
	ErrGroupAssignmentNotFound = errorString("group assignment not found")
	ErrGroupAssignmentRevoked  = errorString("the group assignment is already revoked")
)

// ── groups (000014) ──────────────────────────────────────────────────────────

// Group is §2's "administrative collection of subjects". A role assigned to a
// group is never enforced as a group: it fans out to one governed assignment
// request per member, each with its own SoD check and risk-tiered approval.
type Group struct {
	GroupID              string        `json:"group_id"`
	TenantID             string        `json:"tenant_id"`
	LegalEntityID        string        `json:"legal_entity_id"`
	GroupCode            string        `json:"group_code"`
	GroupName            string        `json:"group_name"`
	Status               string        `json:"status"`
	Source               string        `json:"source"`
	CreatedByPrincipalID string        `json:"created_by_principal_id"`
	CorrelationID        string        `json:"correlation_id"`
	CreatedAt            time.Time     `json:"created_at"`
	UpdatedAt            time.Time     `json:"updated_at"`
	Members              []GroupMember `json:"members,omitempty"`
}

// GroupMember is one live membership. AddedAt distinguishes a re-added member
// from their earlier membership, so a fan-out for the new membership is a new
// request rather than a replay of the one revoked when they left.
type GroupMember struct {
	PrincipalID        string    `json:"principal_id"`
	AddedByPrincipalID string    `json:"added_by_principal_id"`
	AddedAt            time.Time `json:"added_at"`
}

const (
	GroupActive  = "ACTIVE"
	GroupRetired = "RETIRED"
	GroupManual  = "MANUAL"
	GroupSCIM    = "SCIM"
)

type CreateGroupRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	GroupCode     string `json:"group_code"`
	GroupName     string `json:"group_name"`
	Source        string `json:"source,omitempty"`
	CorrelationID string `json:"correlation_id"`
}

// GroupMemberRequest adds (principal_id) or removes (reason) a member.
type GroupMemberRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	PrincipalID   string `json:"principal_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
	CorrelationID string `json:"correlation_id"`
}

// GroupAssignment is "GROUP + role + scope + effective dates" (§9).
type GroupAssignment struct {
	GroupAssignmentID    string     `json:"group_assignment_id"`
	TenantID             string     `json:"tenant_id"`
	GroupID              string     `json:"group_id"`
	RoleDefinitionID     string     `json:"role_definition_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	Justification        string     `json:"justification"`
	Status               string     `json:"status"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	RevokedByPrincipalID string     `json:"revoked_by_principal_id,omitempty"`
	RevocationReason     string     `json:"revocation_reason,omitempty"`
	RevokedAt            *time.Time `json:"revoked_at,omitempty"`
	CorrelationID        string     `json:"correlation_id"`
	CreatedAt            time.Time  `json:"created_at"`
	// Members is the per-member outcome of a fan-out, on command responses.
	Members []GroupMemberOutcome `json:"members,omitempty"`
}

const (
	GroupAssignmentActive  = "ACTIVE"
	GroupAssignmentRevoked = "REVOKED"
)

type CreateGroupAssignmentRequest struct {
	LegalEntityID    string     `json:"legal_entity_id"`
	RoleDefinitionID string     `json:"role_definition_id"`
	EffectiveFrom    time.Time  `json:"effective_from,omitempty"`
	EffectiveTo      *time.Time `json:"effective_to,omitempty"`
	Justification    string     `json:"justification"`
	CorrelationID    string     `json:"correlation_id"`
}

// GroupMemberOutcome is what a fan-out did for one member: the request it
// created or touched, or the code it was refused with (an SoD conflict for
// one member does not block the others).
type GroupMemberOutcome struct {
	PrincipalID string `json:"principal_id"`
	RequestID   string `json:"request_id,omitempty"`
	Status      string `json:"status,omitempty"`
	Error       string `json:"error,omitempty"`
}

// ── subject links (000015) ───────────────────────────────────────────────────

// SubjectLink is the administered "employee E is principal P" an HR event is
// resolved through (S9-C2). Never inferred: an unlinked employee's event is
// skipped.
type SubjectLink struct {
	TenantID              string    `json:"tenant_id"`
	EmployeeID            string    `json:"employee_id"`
	PrincipalID           string    `json:"principal_id"`
	LegalEntityID         string    `json:"legal_entity_id"`
	LastManagerEmployeeID string    `json:"last_manager_employee_id,omitempty"`
	LastStatus            string    `json:"last_status,omitempty"`
	LinkedByPrincipalID   string    `json:"linked_by_principal_id"`
	CorrelationID         string    `json:"correlation_id"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type LinkSubjectRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	EmployeeID    string `json:"employee_id"`
	PrincipalID   string `json:"principal_id"`
	CorrelationID string `json:"correlation_id"`
}

// ReviewTrigger is one HR event resolved to a subject: what opens an
// EVENT_TRIGGERED review (§24 "manager change, entity transfer, ...").
type ReviewTrigger struct {
	TenantID            string
	LegalEntityID       string
	SubjectPrincipalID  string
	ReviewerPrincipalID string // empty: the configured default reviewer
	Reason              string
	CorrelationID       string // derived from the event id: a redelivery replays
}
