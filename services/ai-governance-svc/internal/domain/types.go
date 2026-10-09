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

import (
	"encoding/json"
	"time"
)

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

// ─────────────────────────────────────────────────────────────────────────────
// ZS-SVC-X-001 Wave 1: AIG-01 (AI Use-Case, Risk & Impact Registry) and
// AIG-02 (Model, Provider & Capability Registry).
//
// Additive to the doc7-based entities above, which are unchanged. No AI
// capability reaches production until its business use, affected
// outcome and control class are explicitly registered and approved
// (§4 SECTION CONTROL), and models are deployable artifacts with
// changing behavior and contractual conditions, never interchangeable
// strings in application configuration (§5 SECTION CONTROL).
// ─────────────────────────────────────────────────────────────────────────────

// OperationalClass is §4's A0-A4 business-impact classification,
// independent of commercial packaging.
type OperationalClass string

const (
	OperationalClassA0 OperationalClass = "A0" // No AI
	OperationalClassA1 OperationalClass = "A1" // Low-impact assistive
	OperationalClassA2 OperationalClass = "A2" // Controlled assistive
	OperationalClassA3 OperationalClass = "A3" // High-impact human-gated
	OperationalClassA4 OperationalClass = "A4" // Prohibited-disabled
)

// IsPotentiallyHighImpact reports whether this class requires a
// resolved (non-INDETERMINATE) legal classification before activation
// — §4.5's "fail closed for potentially high-impact functions."
func (c OperationalClass) IsPotentiallyHighImpact() bool {
	return c == OperationalClassA2 || c == OperationalClassA3 || c == OperationalClassA4
}

type AutomationLevel string

const (
	AutomationLevelDraft                  AutomationLevel = "DRAFT"
	AutomationLevelRecommendation         AutomationLevel = "RECOMMENDATION"
	AutomationLevelExtraction             AutomationLevel = "EXTRACTION"
	AutomationLevelClassification         AutomationLevel = "CLASSIFICATION"
	AutomationLevelRanking                AutomationLevel = "RANKING"
	AutomationLevelAutonomousToolPlanning AutomationLevel = "AUTONOMOUS_TOOL_PLANNING"
	AutomationLevelProhibited             AutomationLevel = "PROHIBITED"
)

// UseCaseLifecycleState is §4.3's forward-only state machine: DRAFT ->
// ASSESSING -> APPROVED -> ACTIVE/LIMITED, with SUSPENDED and
// re-assessment branches, terminal at REJECTED/RETIRED.
type UseCaseLifecycleState string

const (
	UseCaseDraft     UseCaseLifecycleState = "DRAFT"
	UseCaseAssessing UseCaseLifecycleState = "ASSESSING"
	UseCaseApproved  UseCaseLifecycleState = "APPROVED"
	UseCaseActive    UseCaseLifecycleState = "ACTIVE"
	UseCaseLimited   UseCaseLifecycleState = "LIMITED"
	UseCaseSuspended UseCaseLifecycleState = "SUSPENDED"
	UseCaseRejected  UseCaseLifecycleState = "REJECTED"
	UseCaseRetired   UseCaseLifecycleState = "RETIRED"
)

// HumanRole is §4.2's "who is accountable, who reviews, and whether the
// human can meaningfully reject/override."
type HumanRole struct {
	AccountablePrincipalID string `json:"accountable_principal_id"`
	ReviewerPrincipalID    string `json:"reviewer_principal_id,omitempty"`
	CanReject              bool   `json:"can_reject"`
}

// AIUseCase is §4.2's registration contract. legal_classification_ref
// is a caller-supplied PDC reference — no live PDC integration exists
// yet, so an empty ref is treated identically to INDETERMINATE per
// §4.5 ("Legal classification INDETERMINATE -> fail closed for
// potentially high-impact functions").
type AIUseCase struct {
	UseCaseID              string                 `json:"use_case_id"`
	TenantID               string                 `json:"tenant_id"`
	Domain                 string                 `json:"domain"`
	Purpose                string                 `json:"purpose"`
	OutcomeType            string                 `json:"outcome_type"`
	OperationalClass       OperationalClass       `json:"operational_class"`
	LegalClassificationRef string                 `json:"legal_classification_ref,omitempty"`
	OwnerPrincipalID       string                 `json:"owner_principal_id"`
	BusinessOutcome        string                 `json:"business_outcome"`
	AffectedDecisions      []string               `json:"affected_decisions,omitempty"`
	DataProfile            map[string]interface{} `json:"data_profile,omitempty"`
	AutomationLevel        AutomationLevel        `json:"automation_level"`
	HumanRole              HumanRole              `json:"human_role"`
	Fallback               string                 `json:"fallback,omitempty"`
	SuccessMeasures        string                 `json:"success_measures,omitempty"`
	ProhibitedBoundary     string                 `json:"prohibited_boundary,omitempty"`
	RetirementCriteria     string                 `json:"retirement_criteria,omitempty"`
	LifecycleState         UseCaseLifecycleState  `json:"lifecycle_state"`
	CreatedAt              time.Time              `json:"created_at"`
	CreatedByPrincipalID   string                 `json:"created_by_principal_id"`
	UpdatedAt              time.Time              `json:"updated_at"`
}

type CreateUseCaseRequest struct {
	Domain                 string                 `json:"domain"`
	Purpose                string                 `json:"purpose"`
	OutcomeType            string                 `json:"outcome_type"`
	OperationalClass       string                 `json:"operational_class"`
	LegalClassificationRef string                 `json:"legal_classification_ref,omitempty"`
	OwnerPrincipalID       string                 `json:"owner_principal_id"`
	BusinessOutcome        string                 `json:"business_outcome"`
	AffectedDecisions      []string               `json:"affected_decisions,omitempty"`
	DataProfile            map[string]interface{} `json:"data_profile,omitempty"`
	AutomationLevel        string                 `json:"automation_level"`
	HumanRole              HumanRole              `json:"human_role"`
	Fallback               string                 `json:"fallback,omitempty"`
	SuccessMeasures        string                 `json:"success_measures,omitempty"`
	ProhibitedBoundary     string                 `json:"prohibited_boundary,omitempty"`
	RetirementCriteria     string                 `json:"retirement_criteria,omitempty"`
	ClientRequestID        string                 `json:"client_request_id"`
	CorrelationID          string                 `json:"correlation_id,omitempty"`
}

// AssessmentDecision is AIImpactAssessment's own PENDING/APPROVED/
// REJECTED dimension.
type AssessmentDecision string

const (
	AssessmentPending  AssessmentDecision = "PENDING"
	AssessmentApproved AssessmentDecision = "APPROVED"
	AssessmentRejected AssessmentDecision = "REJECTED"
)

// AIImpactAssessment is §4's required impact assessment for A2/A3 use
// cases — versioned and evidence-linked; immutable once decided.
type AIImpactAssessment struct {
	AssessmentID         string             `json:"assessment_id"`
	UseCaseID            string             `json:"use_case_id"`
	TenantID             string             `json:"tenant_id"`
	Version              int                `json:"version"`
	AffectedGroups       []string           `json:"affected_groups,omitempty"`
	RightsImpact         string             `json:"rights_impact,omitempty"`
	FinancialImpact      string             `json:"financial_impact,omitempty"`
	EmploymentImpact     string             `json:"employment_impact,omitempty"`
	Mitigations          string             `json:"mitigations,omitempty"`
	Approvers            []string           `json:"approvers,omitempty"`
	Decision             AssessmentDecision `json:"decision"`
	DecidedByPrincipalID *string            `json:"decided_by_principal_id,omitempty"`
	DecisionReason       *string            `json:"decision_reason,omitempty"`
	DecidedAt            *time.Time         `json:"decided_at,omitempty"`
	ExpiresAt            *time.Time         `json:"expires_at,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	CreatedByPrincipalID string             `json:"created_by_principal_id"`
}

type StartAssessmentRequest struct {
	AffectedGroups   []string   `json:"affected_groups,omitempty"`
	RightsImpact     string     `json:"rights_impact,omitempty"`
	FinancialImpact  string     `json:"financial_impact,omitempty"`
	EmploymentImpact string     `json:"employment_impact,omitempty"`
	Mitigations      string     `json:"mitigations,omitempty"`
	Approvers        []string   `json:"approvers,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	CorrelationID    string     `json:"correlation_id,omitempty"`
}

type DecideAssessmentRequest struct {
	Decision string `json:"decision"` // APPROVED | REJECTED
	Reason   string `json:"reason,omitempty"`
}

type ActivateUseCaseRequest struct {
	Limited       bool   `json:"limited,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type SuspendUseCaseRequest struct {
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type RequestReassessmentRequest struct {
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type RetireUseCaseRequest struct {
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// EffectiveUseCaseControl is §4.4's GET .../effective response — the
// exact effective control snapshot for a use case right now.
type EffectiveUseCaseControl struct {
	UseCase          AIUseCase           `json:"use_case"`
	LatestAssessment *AIImpactAssessment `json:"latest_assessment,omitempty"`
}

// ── AIG-02: Model, Provider & Capability Registry ───────────────────────────

// TrainingUse mirrors TrainingUsePosture's three values under the
// AIG-02 name the spec uses.
type TrainingUse string

const (
	TrainingUseNoTraining   TrainingUse = "NO_TRAINING"
	TrainingUseOptOutAvail  TrainingUse = "OPT_OUT_AVAILABLE"
	TrainingUseAllowedValue TrainingUse = "ALLOWED"
)

// ReleaseState is §5.2's forward-only release-state machine. Provider
// alias movement or silent behavior change always produces a NEW
// release row, never a mutation of an existing one (§5.3).
type ReleaseState string

const (
	ReleaseDiscovered   ReleaseState = "DISCOVERED"
	ReleaseDueDiligence ReleaseState = "DUE_DILIGENCE"
	ReleaseEvaluating   ReleaseState = "EVALUATING"
	ReleaseApproved     ReleaseState = "APPROVED"
	ReleaseActive       ReleaseState = "ACTIVE"
	ReleaseRestricted   ReleaseState = "RESTRICTED"
	ReleaseQuarantined  ReleaseState = "QUARANTINED"
	ReleaseRejected     ReleaseState = "REJECTED"
	ReleaseBlocked      ReleaseState = "BLOCKED"
	ReleaseRetired      ReleaseState = "RETIRED"
)

// AIModelRelease is §3.1/§5.1's exact release identity — immutable
// once registered (identity fields never change; only release_state,
// status_reason and control_evidence move as it progresses through
// due diligence, evaluation and approval).
type AIModelRelease struct {
	ModelReleaseID       string                 `json:"model_release_id"`
	Provider             string                 `json:"provider"`
	ProviderModelID      string                 `json:"provider_model_id"`
	DeploymentRegion     string                 `json:"deployment_region"`
	CapabilitySet        []string               `json:"capability_set,omitempty"`
	ContextLimit         *int                   `json:"context_limit,omitempty"`
	TrainingUse          TrainingUse            `json:"training_use"`
	Retention            string                 `json:"retention,omitempty"`
	ApprovedScopes       []string               `json:"approved_scopes,omitempty"`
	ControlEvidence      map[string]interface{} `json:"control_evidence,omitempty"`
	ReleaseState         ReleaseState           `json:"release_state"`
	StatusReason         *string                `json:"status_reason,omitempty"`
	CreatedAt            time.Time              `json:"created_at"`
	CreatedByPrincipalID string                 `json:"created_by_principal_id"`
	UpdatedAt            time.Time              `json:"updated_at"`
}

type RegisterModelReleaseRequest struct {
	Provider         string                 `json:"provider"`
	ProviderModelID  string                 `json:"provider_model_id"`
	DeploymentRegion string                 `json:"deployment_region"`
	CapabilitySet    []string               `json:"capability_set,omitempty"`
	ContextLimit     *int                   `json:"context_limit,omitempty"`
	TrainingUse      string                 `json:"training_use,omitempty"`
	Retention        string                 `json:"retention,omitempty"`
	ApprovedScopes   []string               `json:"approved_scopes,omitempty"`
	ControlEvidence  map[string]interface{} `json:"control_evidence,omitempty"`
	ClientRequestID  string                 `json:"client_request_id"`
	CorrelationID    string                 `json:"correlation_id,omitempty"`
}

// AdvanceReleaseRequest carries evidence for a single forward hop in
// the release-state machine, plus an optional reason for a
// terminal/restrictive hop (reject/block/restrict/quarantine/retire).
type AdvanceReleaseRequest struct {
	ControlEvidence map[string]interface{} `json:"control_evidence,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
	CorrelationID   string                 `json:"correlation_id,omitempty"`
}

// ApproveReleaseRequest is §5.4's procurement/enablement gate,
// attested by the caller since no live PDC/PRV/security-posture
// integration exists yet — the same honest-scoping pattern used for
// the AI-01/02/04/05 governance gaps elsewhere in this platform.
type ApproveReleaseRequest struct {
	PrivacyContractCleared bool                   `json:"privacy_contract_cleared"`
	ResidencyCleared       bool                   `json:"residency_cleared"`
	SecurityCleared        bool                   `json:"security_cleared"`
	EvaluationCleared      bool                   `json:"evaluation_cleared"`
	ExplainabilityCleared  bool                   `json:"explainability_cleared"`
	ContinuityCleared      bool                   `json:"continuity_cleared"`
	LegalCleared           bool                   `json:"legal_cleared"`
	ControlEvidence        map[string]interface{} `json:"control_evidence,omitempty"`
	CorrelationID          string                 `json:"correlation_id,omitempty"`
}

func (r ApproveReleaseRequest) AllGatesCleared() bool {
	return r.PrivacyContractCleared && r.ResidencyCleared && r.SecurityCleared &&
		r.EvaluationCleared && r.ExplainabilityCleared && r.ContinuityCleared && r.LegalCleared
}

type CreateExecutionRequest struct {
	UseCaseID      string          `json:"use_case_id"`
	ModelReleaseID string          `json:"model_release_id"`
	PackageID      string          `json:"package_id"`
	PackageVersion string          `json:"package_version"`
	Input          json.RawMessage `json:"input"`
}

type AIExecution struct {
	ExecutionID          string    `json:"execution_id"`
	TenantID             string    `json:"tenant_id"`
	UseCaseID            string    `json:"use_case_id"`
	ModelReleaseID       string    `json:"model_release_id"`
	PackageID            string    `json:"package_id"`
	PackageVersion       string    `json:"package_version"`
	RequestSHA256        string    `json:"-"`
	Status               string    `json:"status"`
	BlockReason          string    `json:"block_reason"`
	BlockedBy            []string  `json:"blocked_by"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

type CreateAIIncidentRequest struct {
	Severity           string   `json:"severity"`
	ModelReleaseID     string   `json:"model_release_id"`
	Description        string   `json:"description"`
	EvidenceReferences []string `json:"evidence_references,omitempty"`
}

type AIIncident struct {
	IncidentID           string    `json:"incident_id"`
	TenantID             string    `json:"tenant_id"`
	Severity             string    `json:"severity"`
	ModelReleaseID       string    `json:"model_release_id"`
	Description          string    `json:"description"`
	EvidenceReferences   []string  `json:"evidence_references"`
	Status               string    `json:"status"`
	RequestSHA256        string    `json:"-"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
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

	// AIG-01
	ErrUseCaseNotFound                  = errorString("ai use case not found") // AIG-001 USE_CASE_NOT_REGISTERED
	ErrUseCaseNotActive                 = errorString("ai use case is not ACTIVE")
	ErrAssessmentNotFound               = errorString("ai impact assessment not found")
	ErrAssessmentExpired                = errorString("ai impact assessment is expired") // AIG-003 ASSESSMENT_EXPIRED
	ErrAssessmentNotPending             = errorString("ai impact assessment is not PENDING")
	ErrAssessmentNotApproved            = errorString("use case has no approved impact assessment")
	ErrLegalClassificationIndeterminate = errorString("legal classification is indeterminate: fail closed for potentially high-impact use") // AIG-004
	ErrUseCaseNotAssessable             = errorString("ai use case is not in a state that can start assessment")
	ErrUseCaseNotApproved               = errorString("ai use case is not APPROVED")
	ErrUseCaseNotActivatable            = errorString("ai use case cannot be activated")
	ErrUseCaseNotSuspendable            = errorString("ai use case is not ACTIVE or LIMITED")
	ErrUseCaseNotReassessable           = errorString("ai use case is not in a state that allows reassessment")
	ErrUseCaseNotRetirable              = errorString("ai use case is not in a state that can be retired")
	ErrOperationalClassProhibited       = errorString("operational class A4 is prohibited-disabled and cannot be activated")

	// AIG-02
	ErrModelReleaseNotFound     = errorString("ai model release not found")
	ErrModelReleaseNotApproved  = errorString("ai model release is not APPROVED") // AIG-005 MODEL_RELEASE_NOT_APPROVED
	ErrModelReleaseQuarantined  = errorString("ai model release is QUARANTINED")  // AIG-006 MODEL_RELEASE_QUARANTINED
	ErrReleaseGatesNotCleared   = errorString("release cannot be approved: not every procurement/enablement gate is cleared")
	ErrInvalidReleaseTransition = errorString("invalid model release state transition")
	ErrIdempotencyConflict      = errorString("idempotency key was already used with a different request")
	ErrExecutionNotFound        = errorString("ai execution not found")
	ErrAIIncidentNotFound       = errorString("ai incident not found")

	// AIG-04 — ZS-SVC-X-001 §7. AIG-04 determines the oversight requirement
	// and records the disposition decision; WFC (reviewer assignment,
	// delegation, deadlines, escalation) is explicitly out of scope here —
	// no approved integration contract with it exists in this repository.
	ErrAIRunAlreadyHasDisposition = errorString("this ai run already has an output disposition")
	ErrOversightClassProhibited   = errorString("oversight class O4 is AI-prohibited: human/deterministic process only, no disposition may be created")
	ErrDispositionNotFound        = errorString("ai output disposition not found")
	ErrDispositionNotReviewable   = errorString("ai output disposition is not in REVIEW_REQUIRED state")
)

// OversightClass is ZS-SVC-X-001 §7.1's oversight taxonomy.
type OversightClass string

const (
	OversightNone      OversightClass = "O0" // no material consequence; automated monitoring only
	OversightUserReview OversightClass = "O1" // identified user sees AI status, can reject/edit
	OversightQualified  OversightClass = "O2" // qualified reviewer, explicit accept/reject
	OversightDual       OversightClass = "O3" // maker-checker dual/control review
	OversightProhibited OversightClass = "O4" // AI prohibited; technical block
)

func (c OversightClass) Valid() bool {
	switch c {
	case OversightNone, OversightUserReview, OversightQualified, OversightDual, OversightProhibited:
		return true
	default:
		return false
	}
}

// DispositionStatus is ZS-SVC-X-001 §7.3's output state machine. This
// service creates a disposition only in DRAFT_ASSISTIVE or REVIEW_REQUIRED
// (BLOCKED belongs to AIG-03's upstream validation step, before a
// disposition would ever be created here; SUPERSEDED requires a
// replacement-output linking workflow not implemented in this pass).
type DispositionStatus string

const (
	DispositionDraftAssistive DispositionStatus = "DRAFT_ASSISTIVE"
	DispositionReviewRequired DispositionStatus = "REVIEW_REQUIRED"
	DispositionAccepted       DispositionStatus = "ACCEPTED"
	DispositionRejected       DispositionStatus = "REJECTED"
)

type CreateOutputDispositionRequest struct {
	AIRunID        string `json:"ai_run_id"`
	OversightClass string `json:"oversight_class"`
}

// AIOutputDisposition is ZS-SVC-X-001 §7's disposition record: every
// AI-generated output that is not purely O0 passes through here before any
// downstream authority may rely on it. ACCEPTED does not itself execute a
// business action — it only records that a competent reviewer, distinct
// from whoever produced the output, examined and accepted it.
type AIOutputDisposition struct {
	DispositionID        string     `json:"disposition_id"`
	TenantID              string     `json:"tenant_id"`
	AIRunID               string     `json:"ai_run_id"`
	OversightClass         string     `json:"oversight_class"`
	Status                 string     `json:"status"`
	Reason                 *string    `json:"reason,omitempty"`
	RequestSHA256          string     `json:"-"`
	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	DecidedAt              *time.Time `json:"decided_at,omitempty"`
	DecidedByPrincipalID   *string    `json:"decided_by_principal_id,omitempty"`
}

type DecideOutputDispositionRequest struct {
	Decision string `json:"decision"` // ACCEPTED or REJECTED
	Reason   string `json:"reason,omitempty"`
}
