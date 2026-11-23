// Package domain contains the authoritative domain types for workflow-svc.
//
// workflow_type and stage_status are plain strings — no Go enums, same
// doctrine as every other service in this platform. workflow_status IS a
// real (small) state machine: PENDING -> APPROVED | REJECTED | ESCALATED |
// CANCELLED | INVALIDATED, enforced in application code.
package domain

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

// Canonical workflow statuses per ZS-STATE-001 §6.
const (
	WorkflowStatusPending     = "PENDING"
	WorkflowStatusApproved    = "APPROVED"
	WorkflowStatusRejected    = "REJECTED"
	WorkflowStatusEscalated   = "ESCALATED"
	WorkflowStatusCancelled   = "CANCELLED"
	WorkflowStatusInvalidated = "INVALIDATED"
	WorkflowStatusExpired     = "EXPIRED"
)

// WorkflowInstance is one approval request moving through an ordered chain
// of approval stages per ZS-STATE-001 §6. Critical constraint (mirrors every
// other service): entity-bound (LegalEntityID), never hard-deleted —
// cancellation and invalidation are status transitions, not row removals.
type WorkflowInstance struct {
	WorkflowInstanceID string `json:"workflow_instance_id"`

	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`

	// WorkflowType is data only (e.g. "PURCHASE_APPROVAL").
	WorkflowType string `json:"workflow_type"`

	// Approval subject binding per ZS-STATE-001 §6.1
	SubjectType        *string `json:"subject_type,omitempty"`
	SubjectID          *string `json:"subject_id,omitempty"`
	SubjectVersion     *int    `json:"subject_version,omitempty"`
	SubjectFingerprint *string `json:"subject_fingerprint,omitempty"`

	// WorkflowStatus: PENDING | APPROVED | REJECTED | ESCALATED | CANCELLED | INVALIDATED | EXPIRED.
	// APPROVED/REJECTED/CANCELLED/INVALIDATED/EXPIRED are terminal.
	WorkflowStatus string `json:"workflow_status"`

	// CurrentStage is the 1-based stage_order currently awaiting action.
	// 0 once the workflow reaches a terminal state.
	CurrentStage int `json:"current_stage"`

	InitiatedBy   string     `json:"initiated_by"`
	CorrelationID string     `json:"correlation_id"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at"`

	// Invalidation metadata per ZS-STATE-001 §6.1 / §7
	InvalidatedAt            *time.Time `json:"invalidated_at,omitempty"`
	InvalidationReasonCode   *string    `json:"invalidation_reason_code,omitempty"`
	InvalidationNarrative    *string    `json:"invalidation_narrative,omitempty"`
	InvalidationEvidenceRefs []string   `json:"invalidation_evidence_refs,omitempty"`

	// Deadline tracking for SLA/escalation per GOV-06 NFR / R-001 §4.4
	DueAt        *time.Time `json:"due_at,omitempty"`
	NextActionAt *time.Time `json:"next_action_at,omitempty"`

	// Escalation target per R-001 §4.2
	EscalationTarget *string `json:"escalation_target,omitempty"`

	// R-001 §9.1 common columns for optimistic concurrency and audit
	RowVersion      int       `json:"row_version"`
	UpdatedAt       time.Time `json:"updated_at"`
	CreatedBy       string    `json:"created_by"`
	Plane           string    `json:"plane"`
	DataClass       string    `json:"data_class"`
	ResidencyRegion string    `json:"residency_region"`

	// Idempotency key for INV-08 compliance
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Versioned workflow definition reference per R-001 WFC-02
	WorkflowDefinitionID      string `json:"workflow_definition_id,omitempty"`
	WorkflowDefinitionVersion int    `json:"workflow_definition_version,omitempty"`

	// ReviewCycle increments when changes are requested and the chain is
	// decided again (000020); EscalatedAt is when it was last escalated.
	ReviewCycle int        `json:"review_cycle"`
	EscalatedAt *time.Time `json:"escalated_at,omitempty"`
}

// WorkflowStage is one approval gate in a workflow's ordered chain,
// supplied by the caller at creation time — this service does not
// resolve "who should approve X" from any rule engine; no such rules
// are specified anywhere in the architecture docs. See progress.md.
//
// StageType is SINGLE (one named approver, the original shape) or
// QUORUM (an eligible pool with an N-of-M threshold, ZS-SVC-R-001
// §5.1/§8.3 — see quorum.go). Exactly one of
// ApproverPrincipalID/RequiredApprovals is set, matching which shape
// the stage is — enforced at the database by migration 000012's
// workflow_stages_shape_matches_type CHECK.
type WorkflowStage struct {
	WorkflowStageID    string `json:"workflow_stage_id"`
	WorkflowInstanceID string `json:"workflow_instance_id"`

	StageOrder int `json:"stage_order"`

	// StageType: SINGLE | QUORUM.
	StageType string `json:"stage_type"`

	// ApproverPrincipalID is set (non-empty) only for a SINGLE stage —
	// kept a plain string, not a pointer, so every existing SINGLE-stage
	// code path (which predates QUORUM entirely) is untouched.
	ApproverPrincipalID string `json:"approver_principal_id,omitempty"`
	// RequiredApprovals is set only for a QUORUM stage — the N in N-of-M.
	RequiredApprovals *int `json:"required_approvals,omitempty"`

	// StageStatus: PENDING | APPROVED | REJECTED | SKIPPED.
	StageStatus string `json:"stage_status"`

	ActedAt   *time.Time `json:"acted_at"`
	Rationale *string    `json:"rationale"`

	// R-001 §9.1 common columns for optimistic concurrency and audit
	RowVersion      int       `json:"row_version"`
	UpdatedAt       time.Time `json:"updated_at"`
	CreatedBy       string    `json:"created_by"`
	Plane           string    `json:"plane"`
	DataClass       string    `json:"data_class"`
	ResidencyRegion string    `json:"residency_region"`

	// ApproverRole is set only for a ROLE stage (routed by a definition);
	// DueAt is the stage deadline; ActedBy is who decided it (a delegate or
	// an escalation target acts for the stage, and the record says so).
	ApproverRole *string    `json:"approver_role,omitempty"`
	DueAt        *time.Time `json:"due_at,omitempty"`
	ActedBy      *string    `json:"acted_by,omitempty"`
}

// WorkflowTransition is the append-only audit trail — one row per state
// change, never updated or deleted. Every action taken on a workflow,
// approved or rejected, is evidence.
type WorkflowTransition struct {
	WorkflowTransitionID string `json:"workflow_transition_id"`
	WorkflowInstanceID   string `json:"workflow_instance_id"`

	FromState string  `json:"from_state"`
	ToState   string  `json:"to_state"`
	ActedBy   string  `json:"acted_by"`
	Rationale *string `json:"rationale"`

	// CorrelationID is carried forward from the owning WorkflowInstance's
	// own correlation_id. CausationID is nil unless the caller submitting
	// this specific action supplied one.
	CorrelationID *string `json:"correlation_id,omitempty"`
	CausationID   *string `json:"causation_id,omitempty"`

	ActedAt time.Time `json:"acted_at"`

	// Lineage (000020): the stage and review cycle decided, the authority
	// snapshot (access decision, basis, delegation), the route version, and
	// the governance-ledger decision id.
	StageOrder                *int    `json:"stage_order,omitempty"`
	ReviewCycle               *int    `json:"review_cycle,omitempty"`
	AccessDecisionID          *string `json:"access_decision_id,omitempty"`
	AuthorityBasis            *string `json:"authority_basis,omitempty"`
	DelegationID              *string `json:"delegation_id,omitempty"`
	WorkflowDefinitionVersion *int    `json:"workflow_definition_version,omitempty"`
	DecisionID                *string `json:"decision_id,omitempty"`
}

// ── params ───────────────────────────────────────────────────────────────────

// CreateWorkflowStageInput is one entry in the caller-supplied approval
// chain. A SINGLE stage (the default, when StageType is empty) sets
// ApproverPrincipalID only. A QUORUM stage sets StageType, RequiredApprovals
// and QuorumApprovers instead — see quorum.go.
type CreateWorkflowStageInput struct {
	StageType           string   `json:"stage_type,omitempty"`
	ApproverPrincipalID string   `json:"approver_principal_id,omitempty"`
	RequiredApprovals   int      `json:"required_approvals,omitempty"`
	QuorumApprovers     []string `json:"quorum_approvers,omitempty"`
	// ApproverRole routes a ROLE stage (definition-driven).
	ApproverRole string `json:"approver_role,omitempty"`
	// DueAtHours is the stage deadline, counted from when the stage becomes
	// current; when it passes the instance is escalated.
	DueAtHours int `json:"due_at_hours,omitempty"`
}

type CreateWorkflowParams struct {
	WorkflowInstanceID string
	TenantID           string
	LegalEntityID      string
	WorkflowType       string
	SubjectType        *string
	SubjectID          *string
	SubjectVersion     *int
	SubjectFingerprint *string
	InitiatedBy        string
	CorrelationID      string
	IdempotencyKey     string
	EscalationTarget   *string
	// WorkflowDefinitionID/Version pin the instance to the definition that
	// was in force when it started. Later definition changes must not
	// retroactively alter an in-flight approval chain.
	WorkflowDefinitionID      string
	WorkflowDefinitionVersion int
	Stages                    []CreateWorkflowStageInput
	// DueAt is when the whole approval expires (R-001 Expire): past it, a
	// PENDING or ESCALATED instance becomes EXPIRED. nil = no expiry.
	DueAt *time.Time
	// Signatory, when set, binds the workflow to a verified policy-svc
	// SIGNATORY_MATRIX decision (ZS-GOV-INT-002 §4.2); it is stored in the
	// same transaction as the instance.
	Signatory *SignatoryBinding
}

// SubmitActionParams holds input for approving or rejecting the current stage.
type SubmitActionParams struct {
	WorkflowInstanceID string
	ActorPrincipalID   string
	AssignedApproverID string // optional: the assigned approver for this stage (for delegation)
	// Action: APPROVE | REJECT.
	Action    string
	Rationale *string
	// CausationID is optional: the event/decision that caused this specific
	// action, when the caller knows it.
	CausationID *string

	// ActingAs is the stage the actor decides when they are not its named
	// approver: "escalation" (the instance's escalation target on an
	// ESCALATED stage) or "role" (a ROLE stage). Empty for a named approver
	// or a delegate (AssignedApproverID).
	ActingAs  string
	Authority ActionAuthority
	Meta      CommandMeta
}

// InvalidateWorkflowParams holds input for invalidating a workflow on material change.
type InvalidateWorkflowParams struct {
	WorkflowInstanceID string
	TenantID           string
	ActorPrincipalID   string
	ReasonCode         string
	Narrative          *string
	EvidenceRefs       []string
	CorrelationID      string
	CausationID        *string
	// ExpectedVersion is the caller's X-Expected-Version precondition. When
	// present, the update fails with ErrConcurrencyConflict unless it matches
	// the instance's current row_version.
	ExpectedVersion *int

	Authority ActionAuthority
	Meta      CommandMeta
}

// EscalateToWorkflowParams holds input for escalating a workflow to a different target.
type EscalateToWorkflowParams struct {
	WorkflowInstanceID string
	ActorPrincipalID   string
	EscalationTarget   string
	ReasonCode         string
	Narrative          *string
	CorrelationID      string

	Authority ActionAuthority
	Meta      CommandMeta
}

// VerifyReleaseParams holds input for verifying an approval before release.
type VerifyReleaseParams struct {
	WorkflowInstanceID        string
	TenantID                  string
	ExpectedSubjectVersion    *int
	CurrentSubjectFingerprint string
}

// ReleaseVerificationResult is the outcome of a release gate evaluation.
type ReleaseVerificationResult struct {
	WorkflowInstanceID string  `json:"workflow_instance_id"`
	CanRelease         bool    `json:"can_release"`
	Status             string  `json:"status"` // "VALID" | "INVALID"
	Reason             *string `json:"reason,omitempty"`
	WorkflowStatus     string  `json:"workflow_status"`
	SubjectFingerprint *string `json:"subject_fingerprint,omitempty"`
}

// WorkflowDefinition is a versioned, immutable definition of an approval workflow.
// Per R-001 WFC-02 and GOV-06, definitions are separate from instances.
type WorkflowDefinition struct {
	WorkflowDefinitionID string    `json:"workflow_definition_id"`
	TenantID             string    `json:"tenant_id"`
	WorkflowType         string    `json:"workflow_type"`
	Version              int       `json:"version"`
	StagesJSON           []byte    `json:"stages_json"`
	Name                 *string   `json:"name,omitempty"`
	Description          *string   `json:"description,omitempty"`
	CreatedBy            string    `json:"created_by"`
	CreatedAt            time.Time `json:"created_at"`
	SupersededBy         *string   `json:"superseded_by,omitempty"`
	IsActive             bool      `json:"is_active"`

	// R-001 §9.1 common columns
	RowVersion      int       `json:"row_version"`
	UpdatedAt       time.Time `json:"updated_at"`
	Plane           string    `json:"plane"`
	DataClass       string    `json:"data_class"`
	ResidencyRegion string    `json:"residency_region"`

	// ExpiryHours: an instance started from this definition expires this
	// long after it starts (000020). EscalationTarget: who an instance is
	// escalated to when a stage deadline passes.
	ExpiryHours      *int    `json:"expiry_hours,omitempty"`
	EscalationTarget *string `json:"escalation_target,omitempty"`
}

// CreateWorkflowDefinitionParams holds input for creating a workflow definition.
type CreateWorkflowDefinitionParams struct {
	TenantID         string
	WorkflowType     string
	Version          int
	StagesJSON       []byte
	Name             *string
	Description      *string
	CreatedBy        string
	ExpiryHours      *int
	EscalationTarget *string
}

// GetWorkflowDefinitionParams holds input for retrieving a workflow definition.
type GetWorkflowDefinitionParams struct {
	TenantID             string
	WorkflowType         string
	Version              *int // nil = latest active
	WorkflowDefinitionID *string
}

// ListWorkflowDefinitionsParams holds input for listing workflow definitions with cursor paging (API-4).
type ListWorkflowDefinitionsParams struct {
	TenantID     string
	WorkflowType *string
	IsActive     *bool
	Limit        int
	Cursor       *PaginationCursor
}

// ListPendingApprovalsParams holds input for listing pending approvals with cursor paging (API-4).
type ListPendingApprovalsParams struct {
	TenantID string
	Limit    int
	Cursor   *PaginationCursor
}

// ListPendingSubmissionsParams holds input for listing pending submissions with cursor paging (API-4).
type ListPendingSubmissionsParams struct {
	TenantID string
	FormID   string
	Limit    int
	Cursor   *PaginationCursor
}

// ── errors ───────────────────────────────────────────────────────────────────

var ErrWorkflowNotFound = errorString("workflow not found")
var ErrNoStages = errorString("workflow must have at least one stage")
var ErrInvalidTransition = errorString("invalid workflow status transition")
var ErrWrongApprover = errorString("actor is not the approver for the current stage")
var ErrStoreUnavailable = errorString("workflow store unavailable")
var ErrAuthorizationDenied = errorString("authorization denied for this approval action")
var ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

// ErrConcurrencyConflict is returned when an optimistic concurrency check fails:
// the workflow or stage was modified by another transaction concurrently.
var ErrConcurrencyConflict = errorString("concurrency conflict: workflow was modified by another request")

// ErrIdempotencyMismatch is returned when the same idempotency key is used
// with a different request body — INV-08 / GCP §16 IDEMPOTENCY_MISMATCH.
var ErrIdempotencyMismatch = errorString("idempotency key mismatch: same key used with different request body")

// ErrInitiatorCannotBeApprover is a creation-time validation error:
// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3)
// forbids a workflow's initiator from being listed as an approver in any
// of its own stages.
var ErrInitiatorCannotBeApprover = errorString("initiated_by may not appear as an approver in any stage")

// ErrSelfApprovalNotAllowed is the decision-time, defense-in-depth
// enforcement of the same Segregation of Duties doctrine: the principal who
// initiated a workflow may not submit an approve/reject action on it, even
// if they were (incorrectly) recorded as an assigned approver for the
// current stage.
var ErrSelfApprovalNotAllowed = errorString("principal may not approve or decide on their own submission")

// ErrTenantMissing is returned when a tenant-scoped operation is attempted
// without a valid tenant ID in the context.
var ErrTenantMissing = errorString("tenant ID missing from context")

// ErrInvalidIdentifier is returned when a database identifier (UUID, etc.)
// cannot be parsed from the request.
var ErrInvalidIdentifier = errorString("invalid identifier")

// ErrInvalidField is returned when a request field has an invalid value.
var ErrInvalidField = errorString("invalid field")

// Subject binding, invalidation & release gate errors per ZS-STATE-001.
var ErrInvalidSubjectFingerprint = errorString("invalid subject fingerprint format")
var ErrInvalidSubjectVersion = errorString("subject version must be non-negative")
var ErrMissingSubjectField = errorString("subject_type and subject_id must both be provided if either is present")
var ErrWorkflowNotApproved = errorString("workflow is not approved")
var ErrWorkflowInvalidated = errorString("workflow approval has been invalidated")
var ErrSubjectFingerprintMismatch = errorString("subject fingerprint does not match approved fingerprint")
var ErrSubjectVersionMismatch = errorString("subject version does not match expected version")
var ErrWorkflowUnboundSubject = errorString("workflow has no bound subject fingerprint for release verification")
var ErrInvalidReasonCode = errorString("invalid or unrecognized reason code")

// Quorum stage errors (ZS-SVC-R-001 §5.1/§8.3) — see quorum.go.
var ErrInvalidStageType = errorString("stage_type must be SINGLE or QUORUM")
var ErrQuorumRequiresApprovers = errorString("a QUORUM stage requires at least one quorum_approvers entry")
var ErrQuorumThresholdExceedsPool = errorString("required_approvals cannot exceed the number of quorum_approvers")
var ErrQuorumDuplicateApprover = errorString("quorum_approvers must not list the same principal twice")
var ErrNotQuorumStage = errorString("the current stage is not a QUORUM stage")
var ErrNotEligibleQuorumApprover = errorString("actor is not an eligible approver for this quorum stage")

// PaginationCursor is an opaque cursor for keyset pagination (API-4).
// It encodes the sort key of the last item returned (started_at DESC, workflow_instance_id DESC).
// The cursor is base64.RawURLEncoding encoded JSON.
type PaginationCursor struct {
	StartedAt  string `json:"started_at"`  // RFC3339 timestamp
	InstanceID string `json:"instance_id"` // workflow_instance_id
}

// EncodePaginationCursor encodes a cursor for transport.
func EncodePaginationCursor(c *PaginationCursor) (string, error) {
	if c == nil {
		return "", nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodePaginationCursor decodes a cursor from transport.
func DecodePaginationCursor(s string) (*PaginationCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c PaginationCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

type errorString string

func (e errorString) Error() string { return string(e) }

// =============================================================================
// WFC-04: Cases (WF-2)
// =============================================================================
// A Case is a distinct business object from a workflow instance. It represents
// a matter under review (e.g., an investigation, dispute, or exception) that
// may link to one or more workflows for approval steps. Cases have their own
// lifecycle and can exist independently of workflows.
//
// Case families: FINANCIAL, PAYROLL_HR, LEGAL_COMPLIANCE, SECURITY_PRIVACY, OPERATIONAL
// Case states: OPEN → TRIAGED → IN_PROGRESS → WAITING_INTERNAL / WAITING_EXTERNAL → RESOLVED / CLOSED / REOPENED
// Terminal states: RESOLVED, CLOSED
// Confidentiality: INTERNAL, RESTRICTED, PRIVILEGED
// Severity: LOW, MEDIUM, HIGH, CRITICAL

type CaseFamily string

const (
	CaseFamilyFinancial       CaseFamily = "FINANCIAL"
	CaseFamilyPayrollHR       CaseFamily = "PAYROLL_HR"
	CaseFamilyLegalCompliance CaseFamily = "LEGAL_COMPLIANCE"
	CaseFamilySecurityPrivacy CaseFamily = "SECURITY_PRIVACY"
	CaseFamilyOperational     CaseFamily = "OPERATIONAL"
)

type CaseState string

const (
	CaseStateOpen            CaseState = "OPEN"
	CaseStateTriaged         CaseState = "TRIAGED"
	CaseStateInProgress      CaseState = "IN_PROGRESS"
	CaseStateWaitingInternal CaseState = "WAITING_INTERNAL"
	CaseStateWaitingExternal CaseState = "WAITING_EXTERNAL"
	CaseStateResolved        CaseState = "RESOLVED"
	CaseStateClosed          CaseState = "CLOSED"
	CaseStateReopened        CaseState = "REOPENED"
)

var CaseStates = map[CaseState]bool{
	CaseStateOpen: true, CaseStateTriaged: true, CaseStateInProgress: true,
	CaseStateWaitingInternal: true, CaseStateWaitingExternal: true,
	CaseStateResolved: true, CaseStateClosed: true, CaseStateReopened: true,
}

type CaseConfidentiality string

const (
	CaseConfidentialityInternal   CaseConfidentiality = "INTERNAL"
	CaseConfidentialityRestricted CaseConfidentiality = "RESTRICTED"
	CaseConfidentialityPrivileged CaseConfidentiality = "PRIVILEGED"
)

type CaseSeverity string

const (
	CaseSeverityLow      CaseSeverity = "LOW"
	CaseSeverityMedium   CaseSeverity = "MEDIUM"
	CaseSeverityHigh     CaseSeverity = "HIGH"
	CaseSeverityCritical CaseSeverity = "CRITICAL"
)

type Case struct {
	CaseID               string              `json:"case_id"`
	TenantID             string              `json:"tenant_id"`
	LegalEntityID        string              `json:"legal_entity_id"`
	WorkflowInstanceID   *string             `json:"workflow_instance_id,omitempty"` // optional link
	CaseFamily           CaseFamily          `json:"case_family"`
	CaseType             string              `json:"case_type"` // INVESTIGATION, DISPUTE, EXCEPTION, etc.
	CaseState            CaseState           `json:"case_state"`
	Severity             CaseSeverity        `json:"severity"`
	Confidentiality      CaseConfidentiality `json:"confidentiality"`
	Title                string              `json:"title"`
	Description          string              `json:"description"`
	AssignedToPrincipal  *string             `json:"assigned_to_principal,omitempty"`
	CreatedByPrincipalID string              `json:"created_by_principal_id"`
	CanonicalCaseID      *string             `json:"canonical_case_id,omitempty"`  // for merge lineage
	SplitFromCaseID      *string             `json:"split_from_case_id,omitempty"` // for split lineage
	OpenedAt             time.Time           `json:"opened_at"`
	TriagedAt            *time.Time          `json:"triaged_at,omitempty"`
	ResolvedAt           *time.Time          `json:"resolved_at,omitempty"`
	ClosedAt             *time.Time          `json:"closed_at,omitempty"`
	CreatedAt            time.Time           `json:"created_at"`
	UpdatedAt            time.Time           `json:"updated_at"`
	RowVersion           int                 `json:"row_version"`
}

type CreateCaseParams struct {
	TenantID             string
	LegalEntityID        string
	WorkflowInstanceID   *string
	CaseFamily           CaseFamily
	CaseType             string
	Title                string
	Description          string
	AssignedToPrincipal  *string
	CreatedByPrincipalID string
	CorrelationID        string
}

type TransitionCaseParams struct {
	CaseID           string
	NewState         CaseState
	ExpectedVersion  int
	ActorPrincipalID string
	CorrelationID    string
	Reason           *string // for transitions
}

type ListCasesFilter struct {
	TenantID            string
	LegalEntityID       string
	WorkflowInstanceID  *string
	CaseFamily          CaseFamily
	CaseType            string
	CaseState           CaseState
	Confidentiality     CaseConfidentiality
	AssignedToPrincipal *string
	Limit               int
	Offset              int
	AfterCreatedAt      *time.Time
	AfterID             string
}

var ErrCaseNotFound = errorString("case not found")
var ErrInvalidCaseTransition = errorString("invalid case state transition")

// Case state transitions per R-001 §6.3
var CaseStateTransitions = map[CaseState][]CaseState{
	CaseStateOpen:            {CaseStateTriaged, CaseStateClosed},
	CaseStateTriaged:         {CaseStateInProgress, CaseStateWaitingInternal, CaseStateWaitingExternal, CaseStateClosed},
	CaseStateInProgress:      {CaseStateWaitingInternal, CaseStateWaitingExternal, CaseStateResolved, CaseStateClosed},
	CaseStateWaitingInternal: {CaseStateInProgress, CaseStateResolved, CaseStateClosed},
	CaseStateWaitingExternal: {CaseStateInProgress, CaseStateResolved, CaseStateClosed},
	CaseStateResolved:        {CaseStateClosed, CaseStateReopened},
	CaseStateClosed:          {CaseStateReopened},
	CaseStateReopened:        {CaseStateTriaged, CaseStateInProgress, CaseStateWaitingInternal, CaseStateWaitingExternal, CaseStateClosed},
}

// CaseMember represents a member of a case with a role
type CaseMember struct {
	CaseMemberID         string     `json:"case_member_id"`
	CaseID               string     `json:"case_id"`
	PrincipalID          string     `json:"principal_id"`
	Role                 string     `json:"role"` // OWNER, ASSIGNEE, OBSERVER
	AddedAt              time.Time  `json:"added_at"`
	AddedByPrincipalID   string     `json:"added_by_principal_id"`
	RemovedAt            *time.Time `json:"removed_at,omitempty"`
	RemovedByPrincipalID *string    `json:"removed_by_principal_id,omitempty"`
}

// CaseNote represents an append-only versioned note on a case
type CaseNote struct {
	CaseNoteID     string    `json:"case_note_id"`
	CaseID         string    `json:"case_id"`
	PrincipalID    string    `json:"principal_id"`
	Content        string    `json:"content"`
	Visibility     string    `json:"visibility"` // INTERNAL, RESTRICTED, PRIVILEGED
	EvidenceRefs   []string  `json:"evidence_refs"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Version        int       `json:"version"`
	PreviousNoteID *string   `json:"previous_note_id,omitempty"`
}

// CaseLink represents a read-only link to a domain object
type CaseLink struct {
	CaseLinkID          string    `json:"case_link_id"`
	CaseID              string    `json:"case_id"`
	DomainObjectType    string    `json:"domain_object_type"`
	DomainObjectID      string    `json:"domain_object_id"`
	LinkedAt            time.Time `json:"linked_at"`
	LinkedByPrincipalID string    `json:"linked_by_principal_id"`
}

// CaseTransition represents an append-only transition log entry
type CaseTransition struct {
	CaseTransitionID string     `json:"case_transition_id"`
	CaseID           string     `json:"case_id"`
	FromState        *CaseState `json:"from_state,omitempty"`
	ToState          CaseState  `json:"to_state"`
	ActorPrincipalID string     `json:"actor_principal_id"`
	Reason           *string    `json:"reason,omitempty"`
	TriggeredBy      string     `json:"triggered_by"` // TRIAGE, START, WAIT_INTERNAL, WAIT_EXTERNAL, RESOLVE, CLOSE, REOPEN, MERGE, SPLIT
	CorrelationID    *string    `json:"correlation_id,omitempty"`
	CausationID      *string    `json:"causation_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

// CaseAlias represents a merge alias (source case merged into canonical)
type CaseAlias struct {
	AliasID             string    `json:"alias_id"`
	CanonicalCaseID     string    `json:"canonical_case_id"`
	AliasCaseID         string    `json:"alias_case_id"`
	MergedAt            time.Time `json:"merged_at"`
	MergedByPrincipalID string    `json:"merged_by_principal_id"`
}

// =============================================================================
// ST-1/2/3: State Machine Registry
// =============================================================================

// StateMachineRegistry holds the allowed states and transitions for each
// workflow type. This is loaded from workflow definitions.
type StateMachineRegistry struct {
	// Maps workflow_type -> state machine definition
	definitions map[string]*StateMachineDef
}

type StateMachineDef struct {
	WorkflowType   string
	States         []string
	Transitions    map[string][]string // from_state -> []to_state
	InitialState   string
	TerminalStates []string
	RequiresHuman  bool // whether approvals require human (not service account)
}

// GetStateMachine returns the state machine for a workflow type.
// Returns nil if not found.
func (r *StateMachineRegistry) GetStateMachine(workflowType string) *StateMachineDef {
	return r.definitions[workflowType]
}

// RegisterStateMachine registers a state machine for a workflow type.
func (r *StateMachineRegistry) RegisterStateMachine(def *StateMachineDef) {
	if r.definitions == nil {
		r.definitions = make(map[string]*StateMachineDef)
	}
	r.definitions[def.WorkflowType] = def
}

// IsValidTransition checks if a transition is allowed for a workflow type.
func (r *StateMachineRegistry) IsValidTransition(workflowType, fromState, toState string) bool {
	def := r.GetStateMachine(workflowType)
	if def == nil {
		return false
	}
	allowed, ok := def.Transitions[fromState]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == toState {
			return true
		}
	}
	return false
}

// AvailableActions returns the actions available for a principal at a given stage.
// This is the ST-2 endpoint: GET /v1/workflows/{id}/available-actions
type AvailableActions struct {
	WorkflowInstanceID string   `json:"workflow_instance_id"`
	CurrentStage       int      `json:"current_stage"`
	CurrentStatus      string   `json:"current_status"`
	Actions            []string `json:"actions"` // APPROVE, REJECT, REQUEST_CHANGES, ESCALATE, CANCEL, INVALIDATE, etc.
	PrincipalID        string   `json:"principal_id"`
	CanAct             bool     `json:"can_act"`
	Reason             string   `json:"reason,omitempty"` // why can_act is false
}

// TransitionHistoryEntry is one entry in the transition history.
// This is the ST-3 endpoint: GET /v1/workflows/{id}/transition-history
type TransitionHistoryEntry struct {
	TransitionID       string    `json:"transition_id"`
	WorkflowInstanceID string    `json:"workflow_instance_id"`
	FromStage          int       `json:"from_stage"`
	ToStage            int       `json:"to_stage"`
	FromStatus         string    `json:"from_status"`
	ToStatus           string    `json:"to_status"`
	ActorPrincipalID   string    `json:"actor_principal_id"`
	ActionType         string    `json:"action_type"` // APPROVE, REJECT, REQUEST_CHANGES, ESCALATE, etc.
	Rationale          *string   `json:"rationale,omitempty"`
	DecisionID         *string   `json:"decision_id,omitempty"`
	Quorum             any       `json:"quorum,omitempty"`
	OccurredAt         time.Time `json:"occurred_at"`
}

// ErrNoStateMachine is returned when no state machine is registered for a workflow type.
var ErrNoStateMachine = errorString("no state machine registered for workflow type")
