package domain

import "time"

// Group 2 completion pass (21 Oct 2026): ROLE stages, the authority snapshot,
// command idempotency, review cycles and timers.

// StageTypeRole is a stage a workflow definition routes to a role rather than
// to a principal the initiator named: any principal authorization-svc
// permits to approve this workflow type at this entity (with the stage's
// approver_role passed as an attribute) may decide it, except the initiator
// and anyone who already decided another stage of the instance.
const StageTypeRole = "ROLE"

// ActionAuthority is the authority snapshot an approval decision is taken
// under (R-001 evidence lineage): the authorization decision, and the
// delegation when a delegate acted.
type ActionAuthority struct {
	AccessDecisionID string `json:"access_decision_id,omitempty"`
	Basis            string `json:"authority_basis,omitempty"`
	DelegationID     string `json:"delegation_id,omitempty"`
}

// CommandMeta carries what every state-changing command records with its
// transition: who asked, under which trace, with which Idempotency-Key and
// request hash (a retried command with the same key replays; a different
// body is ErrIdempotencyMismatch), and the governance-ledger decision id the
// transition is recorded under.
type CommandMeta struct {
	IdempotencyKey string
	RequestHash    string
	CorrelationID  string
	DecisionID     string
	Jurisdiction   string
}

// LifecycleCommand is the optional detail of an escalate, cancel, expire or
// request-changes command: why, under which authority, and its command meta
// (ledger decision id, Idempotency-Key).
type LifecycleCommand struct {
	Rationale  *string
	ReasonCode string
	Authority  ActionAuthority
	Meta       CommandMeta
}

// DefinitionStage is one entry of a workflow definition's stages_json.
type DefinitionStage struct {
	Order               int      `json:"order"`
	StageType           string   `json:"stage_type,omitempty"`
	ApproverRole        string   `json:"approver_role,omitempty"`
	ApproverPrincipalID string   `json:"approver_principal_id,omitempty"`
	RequiredApprovals   int      `json:"required_approvals,omitempty"`
	QuorumApprovers     []string `json:"quorum_approvers,omitempty"`
	DueAtHours          int      `json:"due_at_hours,omitempty"`
}

// TimerSweepResult reports one timer pass.
type TimerSweepResult struct {
	Expired   int
	Escalated int
	Rechecked int
}

// DueInstance is an instance a timer pass must act on, with its tenant.
type DueInstance struct {
	TenantID           string
	WorkflowInstanceID string
	WorkflowStatus     string
	DueAt              *time.Time
	NextActionAt       *time.Time
	RecheckAt          *time.Time
	CurrentStage       int
	RowVersion         int
	EscalationTarget   *string
	LegalEntityID      string
	InitiatedBy        string
	WorkflowType       string
}

// ApproverCheck is one current-stage approver a re-check must re-authorize.
type ApproverCheck struct {
	WorkflowInstanceID  string
	StageOrder          int
	StageType           string
	ApproverPrincipalID string
	ApproverRole        string
}

var (
	ErrRationaleRequired  = errorString("a rationale is required for this decision")
	ErrWorkflowExpired    = errorString("workflow has expired")
	ErrApproverNotHuman   = errorString("a service account or workload cannot decide an approval")
	ErrAlreadyDecided     = errorString("principal already decided another stage of this workflow")
	ErrNotEscalationOwner = errorString("only the escalation target may act on an escalated stage it was routed to")
	// ErrNoCurrentStage: the workflow exists but is terminal, so no stage
	// awaits a decision. It used to answer 404 workflow_not_found, the same
	// as an id that does not exist.
	ErrNoCurrentStage = errorString("workflow has no stage awaiting a decision")
	// ErrLegalEntityNotFound: tenant-entity-registry-svc does not know the
	// legal entity in the caller's tenant.
	ErrLegalEntityNotFound = errorString("legal entity not found")
	// ErrRegistryUnavailable: tenant-entity-registry-svc could not answer, so
	// the entity fact is unknown and the request fails closed.
	ErrRegistryUnavailable = errorString("tenant-entity-registry-svc unavailable")
)
