// Package domain defines the authoritative domain types for
// ai-governance-svc.
//
// Per docs/original_doc/zoiko_suite_doc7.txt §11 ("AI, Agentic Automation &
// Human Authority"), the doctrine is explicit: "the platform must never
// confuse model capability with organizational authority. The deterministic
// policy layer, source/evidence state, actor permissions, approved tool
// registry, tenant automation policy and required human approvals outrank
// model preference." This service is the record-keeping and gate-checking
// layer for that doctrine — it does not run models or execute automations
// itself.
package domain

import "time"

// AIRunType is doc7 §G1's own enumerated list of what AI may do by default —
// quoted verbatim, not a locally invented taxonomy: "Classify, summarize,
// extract, compare, draft, prioritize, recommend and explain within approved
// data/source scope."
type AIRunType string

const (
	AIRunClassify   AIRunType = "CLASSIFY"
	AIRunSummarize  AIRunType = "SUMMARIZE"
	AIRunExtract    AIRunType = "EXTRACT"
	AIRunCompare    AIRunType = "COMPARE"
	AIRunDraft      AIRunType = "DRAFT"
	AIRunPrioritize AIRunType = "PRIORITIZE"
	AIRunRecommend  AIRunType = "RECOMMEND"
	AIRunExplain    AIRunType = "EXPLAIN"
)

// UncertaintyState is doc7 §G5's required state set: "Use
// classification/risk states including UNCERTAIN/CONFLICT/NO_SOURCE where
// applicable." NONE means the run reported ordinary confidence.
type UncertaintyState string

const (
	UncertaintyNone      UncertaintyState = "NONE"
	UncertaintyUncertain UncertaintyState = "UNCERTAIN"
	UncertaintyConflict  UncertaintyState = "CONFLICT"
	UncertaintyNoSource  UncertaintyState = "NO_SOURCE"
)

// AIRun is doc7 §G1's AI run/recommendation object: "AI outputs carry
// model/prompt/tool versions, source/evidence refs, confidence/limitation
// metadata and audit IDs where material." This is the record of one such
// output — never itself an authority to act.
type AIRun struct {
	AIRunID              string           `json:"ai_run_id"`
	TenantID             string           `json:"tenant_id"`
	RunType              AIRunType        `json:"run_type"`
	ModelID              string           `json:"model_id"`
	PromptVersion        string           `json:"prompt_version"`
	ToolVersion          *string          `json:"tool_version,omitempty"`
	SourceRefs           []string         `json:"source_refs,omitempty"`
	EvidenceRefs         []string         `json:"evidence_refs,omitempty"`
	Confidence           *float64         `json:"confidence,omitempty"`
	Limitation           *string          `json:"limitation,omitempty"`
	UncertaintyState     UncertaintyState `json:"uncertainty_state"`
	RecommendedAction    *string          `json:"recommended_action,omitempty"`
	AuditID              string           `json:"audit_id"`
	CreatedAt            time.Time        `json:"created_at"`
	CreatedByPrincipalID string           `json:"created_by_principal_id"`
}

type CreateAIRunRequest struct {
	RunType           string   `json:"run_type"`
	ModelID           string   `json:"model_id"`
	PromptVersion     string   `json:"prompt_version"`
	ToolVersion       string   `json:"tool_version,omitempty"`
	SourceRefs        []string `json:"source_refs,omitempty"`
	EvidenceRefs      []string `json:"evidence_refs,omitempty"`
	Confidence        *float64 `json:"confidence,omitempty"`
	Limitation        string   `json:"limitation,omitempty"`
	UncertaintyState  string   `json:"uncertainty_state,omitempty"`
	RecommendedAction string   `json:"recommended_action,omitempty"`
	AuditID           string   `json:"audit_id"`
	CorrelationID     string   `json:"correlation_id"`
}

// ActionRiskClassification maps a business action type to the doc7 §G2 risk
// taxonomy: "Any action affecting money, employment, tax/filing, legal
// position, external certification, access/security, contractual
// commitment, record deletion, retention/legal hold or regulated reporting"
// requires heightened controls. ActionType is DATA, per the same doctrine
// applied elsewhere in this codebase — no handler in this service switches
// on it, only looks it up.
type RiskCategory string

const (
	RiskCategoryNone                  RiskCategory = "NONE"
	RiskCategoryMoney                 RiskCategory = "MONEY"
	RiskCategoryEmployment            RiskCategory = "EMPLOYMENT"
	RiskCategoryTaxFiling             RiskCategory = "TAX_FILING"
	RiskCategoryLegalPosition         RiskCategory = "LEGAL_POSITION"
	RiskCategoryExternalCertification RiskCategory = "EXTERNAL_CERTIFICATION"
	RiskCategoryAccessSecurity        RiskCategory = "ACCESS_SECURITY"
	RiskCategoryContractualCommitment RiskCategory = "CONTRACTUAL_COMMITMENT"
	RiskCategoryRecordDeletion        RiskCategory = "RECORD_DELETION"
	RiskCategoryRetentionLegalHold    RiskCategory = "RETENTION_LEGAL_HOLD"
	RiskCategoryRegulatedReporting    RiskCategory = "REGULATED_REPORTING"
)

type ActionRiskClassification struct {
	ActionType           string       `json:"action_type"`
	RiskCategory         RiskCategory `json:"risk_category"`
	HumanReviewTrigger   bool         `json:"human_review_trigger"`
	RequiresMakerChecker bool         `json:"requires_maker_checker"`
	CreatedAt            time.Time    `json:"created_at"`
	CreatedByPrincipalID string       `json:"created_by_principal_id"`
}

type SetActionRiskClassificationRequest struct {
	ActionType           string `json:"action_type"`
	RiskCategory         string `json:"risk_category"`
	HumanReviewTrigger   bool   `json:"human_review_trigger"`
	RequiresMakerChecker bool   `json:"requires_maker_checker"`
	CorrelationID        string `json:"correlation_id"`
}

// AutomationActionStatus is the object's own lifecycle, separate from its
// ApprovalStatus dimension — mirroring doc7 §H1's doctrine that work state,
// approval state, evidence state, etc. are independent dimensions composed
// into a user-facing status, never one collapsed field.
type AutomationActionStatus string

const (
	AutomationActionProposed   AutomationActionStatus = "PROPOSED"
	AutomationActionApproved   AutomationActionStatus = "APPROVED"
	AutomationActionExecuting  AutomationActionStatus = "EXECUTING"
	AutomationActionCompleted  AutomationActionStatus = "COMPLETED"
	AutomationActionFailed     AutomationActionStatus = "FAILED"
	AutomationActionRolledBack AutomationActionStatus = "ROLLED_BACK"
	AutomationActionRejected   AutomationActionStatus = "REJECTED"
)

type ApprovalStatus string

const (
	ApprovalNotRequired ApprovalStatus = "NOT_REQUIRED"
	ApprovalPending     ApprovalStatus = "PENDING"
	ApprovalApproved    ApprovalStatus = "APPROVED"
	ApprovalRejected    ApprovalStatus = "REJECTED"
)

// AutomationAction is doc7 §G2/§G7's required object: "Require action risk
// class, deterministic preconditions, explicit permission, maker-checker/
// human approval where defined, idempotency, postcondition verification and
// rollback/compensation plan." A row here is the record of one proposed or
// executed autonomous action, always checked against an AutomationPolicy
// allowlist before it may run.
type AutomationAction struct {
	AutomationActionID    string                 `json:"automation_action_id"`
	TenantID              string                 `json:"tenant_id"`
	ActionType            string                 `json:"action_type"`
	RiskCategory          RiskCategory           `json:"risk_category"`
	IdempotencyKey        string                 `json:"idempotency_key"`
	PreconditionsMet      bool                   `json:"preconditions_met"`
	ApprovalStatus        ApprovalStatus         `json:"approval_status"`
	PostconditionVerified bool                   `json:"postcondition_verified"`
	RollbackPlan          *string                `json:"rollback_plan,omitempty"`
	Status                AutomationActionStatus `json:"status"`
	ProposedByPrincipalID string                 `json:"proposed_by_principal_id"`
	ApprovedByPrincipalID *string                `json:"approved_by_principal_id,omitempty"`
	CreatedAt             time.Time              `json:"created_at"`
	UpdatedAt             time.Time              `json:"updated_at"`
}

type ProposeAutomationActionRequest struct {
	TenantID         string `json:"tenant_id"`
	ActionType       string `json:"action_type"`
	Role             string `json:"role"`
	Tool             string `json:"tool"`
	IdempotencyKey   string `json:"idempotency_key"`
	PreconditionsMet bool   `json:"preconditions_met"`
	RollbackPlan     string `json:"rollback_plan,omitempty"`
	CorrelationID    string `json:"correlation_id"`
}

type ApproveAutomationActionRequest struct {
	Decision string `json:"decision"` // APPROVED | REJECTED
	Reason   string `json:"reason,omitempty"`
}

// AutomationPolicy is doc7 §G7's automation_policy allowlist: "defines
// action, preconditions, max scope/amount, required approvals, dry-run/
// preview, idempotency, time window, rate/volume limits, kill switch and
// audit event set" — scoped per tenant, role, risk class and tool, per §G7's
// decision that autonomous actions are allowed "through explicit
// action-policy allowlists," never broad delegated authority.
type AutomationPolicy struct {
	AutomationPolicyID   string       `json:"automation_policy_id"`
	TenantID             string       `json:"tenant_id"`
	Role                 string       `json:"role"`
	RiskCategory         RiskCategory `json:"risk_category"`
	Tool                 string       `json:"tool"`
	ActionType           string       `json:"action_type"`
	MaxScopeAmount       *float64     `json:"max_scope_amount,omitempty"`
	RequiredApprovals    int          `json:"required_approvals"`
	DryRunRequired       bool         `json:"dry_run_required"`
	RateLimitPerDay      *int         `json:"rate_limit_per_day,omitempty"`
	KillSwitchEngaged    bool         `json:"kill_switch_engaged"`
	CreatedAt            time.Time    `json:"created_at"`
	CreatedByPrincipalID string       `json:"created_by_principal_id"`
}

type CreateAutomationPolicyRequest struct {
	TenantID          string   `json:"tenant_id"`
	Role              string   `json:"role"`
	RiskCategory      string   `json:"risk_category"`
	Tool              string   `json:"tool"`
	ActionType        string   `json:"action_type"`
	MaxScopeAmount    *float64 `json:"max_scope_amount,omitempty"`
	RequiredApprovals int      `json:"required_approvals"`
	DryRunRequired    bool     `json:"dry_run_required,omitempty"`
	RateLimitPerDay   *int     `json:"rate_limit_per_day,omitempty"`
	CorrelationID     string   `json:"correlation_id"`
}

// AutomationPolicyResolution is the answer to "may this tenant/role/tool
// autonomously perform this action right now" — the single check every
// autonomous-action caller must pass before proposing an AutomationAction.
type AutomationPolicyResolution struct {
	Allowed    bool   `json:"allowed"`
	ReasonCode string `json:"reason_code"` // ALLOWED | NOT_ALLOWLISTED | KILL_SWITCH_ENGAGED
	Detail     string `json:"detail,omitempty"`
}

// ModelProviderRegistration is doc7 §G6's provider/model registry: "must
// verify training-use posture, retention, region, DPA/subprocessors and
// approved data classes before production calls." TrainingUsePosture
// defaults to NO_TRAINING per §G6's decision: "No default training use is
// authorized."
type TrainingUsePosture string

const (
	TrainingUseNone    TrainingUsePosture = "NO_TRAINING"
	TrainingUseOptOut  TrainingUsePosture = "OPT_OUT_AVAILABLE"
	TrainingUseAllowed TrainingUsePosture = "ALLOWED"
)

type ModelProviderRegistration struct {
	ProviderRegistrationID string             `json:"provider_registration_id"`
	ProviderName           string             `json:"provider_name"`
	ModelName              string             `json:"model_name"`
	TrainingUsePosture     TrainingUsePosture `json:"training_use_posture"`
	RetentionPolicyRef     *string            `json:"retention_policy_ref,omitempty"`
	DataRegion             string             `json:"data_region"`
	DPAVerified            bool               `json:"dpa_verified"`
	ApprovedDataClasses    []string           `json:"approved_data_classes,omitempty"`
	ApprovedAt             *time.Time         `json:"approved_at,omitempty"`
	ApprovedByPrincipalID  *string            `json:"approved_by_principal_id,omitempty"`
	CreatedAt              time.Time          `json:"created_at"`
}

type RegisterModelProviderRequest struct {
	ProviderName        string   `json:"provider_name"`
	ModelName           string   `json:"model_name"`
	TrainingUsePosture  string   `json:"training_use_posture,omitempty"`
	RetentionPolicyRef  string   `json:"retention_policy_ref,omitempty"`
	DataRegion          string   `json:"data_region"`
	DPAVerified         bool     `json:"dpa_verified,omitempty"`
	ApprovedDataClasses []string `json:"approved_data_classes,omitempty"`
	CorrelationID       string   `json:"correlation_id"`
}

// ModelProviderVerification is the pre-production-call check §G6 requires.
type ModelProviderVerification struct {
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
}

// PolicyChangeApproval is doc7 §G3's maker-checker gate: "Agent may draft
// changes; publication requires versioned change request, impact analysis,
// authorized approval, tests and release record." Self-approval is blocked
// at decision time, per §H3: "Self-approval attempts are blocked and
// evented."
type PolicyChangeDecision string

const (
	PolicyChangePending  PolicyChangeDecision = "PENDING"
	PolicyChangeApproved PolicyChangeDecision = "APPROVED"
	PolicyChangeRejected PolicyChangeDecision = "REJECTED"
)

type PolicyChangeApproval struct {
	PolicyChangeApprovalID string               `json:"policy_change_approval_id"`
	TargetPolicyRef        string               `json:"target_policy_ref"`
	ProposedChange         string               `json:"proposed_change"`
	ProposedByPrincipalID  string               `json:"proposed_by_principal_id"`
	Decision               PolicyChangeDecision `json:"decision"`
	DecidedByPrincipalID   *string              `json:"decided_by_principal_id,omitempty"`
	DecisionReason         *string              `json:"decision_reason,omitempty"`
	DecidedAt              *time.Time           `json:"decided_at,omitempty"`
	CreatedAt              time.Time            `json:"created_at"`
}

type ProposePolicyChangeRequest struct {
	TargetPolicyRef string `json:"target_policy_ref"`
	ProposedChange  string `json:"proposed_change"`
	CorrelationID   string `json:"correlation_id"`
}

type DecidePolicyChangeRequest struct {
	Decision string `json:"decision"` // APPROVED | REJECTED
	Reason   string `json:"reason,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrAIRunNotFound                    = errorString("ai run not found")
	ErrActionRiskClassificationNotFound = errorString("action risk classification not found")
	ErrAutomationActionNotFound         = errorString("automation action not found")
	ErrAutomationPolicyNotFound         = errorString("automation policy not found")
	ErrModelProviderNotFound            = errorString("model provider registration not found")
	ErrPolicyChangeApprovalNotFound     = errorString("policy change approval not found")
	ErrConflict                         = errorString("conflict")
	// ErrSelfApprovalBlocked is doc7 §H3's mandatory self-approval block —
	// the same principal that proposed an action or policy change may never
	// be the one who approves it.
	ErrSelfApprovalBlocked = errorString("self-approval is blocked: approver must differ from proposer")
	// ErrActionNotAllowlisted means no AutomationPolicy grants this
	// tenant/role/risk-class/tool/action combination — doc7 §G7's allowlist
	// doctrine is fail-closed by default.
	ErrActionNotAllowlisted    = errorString("action is not allowlisted for this tenant/role/risk-class/tool")
	ErrDuplicateIdempotencyKey = errorString("automation action already proposed with this idempotency key")
	ErrInvalidDecision         = errorString("decision must be APPROVED or REJECTED")
)
