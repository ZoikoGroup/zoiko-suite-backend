package domain

import "time"

// AuditPlan is AUD-02's own "AuditPlan" — one per engagement at a time
// (enforced by a partial unique index on effective_to IS NULL), versioned
// and effective-dated the same way AuditEngagement itself is. Approval
// snapshots the engagement's own scope_version so a later AmendScope can
// detect it needs to demote this plan back to REVIEWED.
type AuditPlan struct {
	PlanID                 string     `json:"plan_id"`
	EngagementID           string     `json:"engagement_id"`
	TenantID               string     `json:"tenant_id"`
	Version                int        `json:"version"`
	Status                 string     `json:"status"`
	ScopeVersionAtApproval *int       `json:"scope_version_at_approval,omitempty"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	EffectiveFrom          time.Time  `json:"effective_from"`
	EffectiveTo            *time.Time `json:"effective_to,omitempty"`
}

const (
	AuditPlanDraft    = "DRAFT"
	AuditPlanPrepared = "PREPARED"
	AuditPlanReviewed = "REVIEWED"
	AuditPlanApproved = "APPROVED"
)

// MaterialityRecord is AUD-02's own "MaterialityRecord" — versioned,
// append-only (a new record is inserted, the old one end-dated, never
// mutated). RecordMateriality against an APPROVED plan is the concrete
// enforcement of "materiality changes trigger dependency analysis": see
// AuditPlanStore.RecordMateriality's own doc comment.
type MaterialityRecord struct {
	MaterialityID           string     `json:"materiality_id"`
	PlanID                  string     `json:"plan_id"`
	TenantID                string     `json:"tenant_id"`
	OverallMateriality      float64    `json:"overall_materiality"`
	PerformanceMateriality  float64    `json:"performance_materiality"`
	ClearlyTrivialThreshold *float64   `json:"clearly_trivial_threshold,omitempty"`
	Rationale               string     `json:"rationale"`
	CreatedByPrincipalID    string     `json:"created_by_principal_id"`
	CreatedAt               time.Time  `json:"created_at"`
	EffectiveFrom           time.Time  `json:"effective_from"`
	EffectiveTo             *time.Time `json:"effective_to,omitempty"`
}

// RiskAssessment is AUD-02's own "RiskAssessment". CoverageStatus tracks
// whether a HIGH risk's own response has actually been designed and
// executed — this is what AUD-01's own MarkFieldworkComplete gate reads
// ("unresolved high-risk coverage blocks completion").
type RiskAssessment struct {
	RiskID                string     `json:"risk_id"`
	PlanID                string     `json:"plan_id"`
	EngagementID          string     `json:"engagement_id"`
	TenantID              string     `json:"tenant_id"`
	Description           string     `json:"description"`
	RiskLevel             string     `json:"risk_level"`
	IsSignificant         bool       `json:"is_significant"`
	Status                string     `json:"status"`
	CoverageStatus        string     `json:"coverage_status"`
	AssessedByPrincipalID *string    `json:"assessed_by_principal_id,omitempty"`
	AssessedAt            *time.Time `json:"assessed_at,omitempty"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	CreatedAt             time.Time  `json:"created_at"`
	EffectiveFrom         time.Time  `json:"effective_from"`
	EffectiveTo           *time.Time `json:"effective_to,omitempty"`
}

const (
	RiskLevelLow      = "LOW"
	RiskLevelModerate = "MODERATE"
	RiskLevelHigh     = "HIGH"

	RiskStatusIdentified = "IDENTIFIED"
	RiskStatusAssessed   = "ASSESSED"

	RiskCoveragePending             = "PENDING"
	RiskCoverageCovered             = "COVERED"
	RiskCoveragePendingReassessment = "PENDING_REASSESSMENT"
)

// AssertionLink and PlannedProcedure are the two facts AssessRisk's own
// CAS predicate requires to exist before a risk can leave IDENTIFIED — the
// real, DB-enforced form of "every risk links to assertions/process and
// response."
type AssertionLink struct {
	AssertionLinkID string    `json:"assertion_link_id"`
	RiskID          string    `json:"risk_id"`
	TenantID        string    `json:"tenant_id"`
	AssertionCode   string    `json:"assertion_code"`
	CreatedAt       time.Time `json:"created_at"`
}

type PlannedProcedure struct {
	ProcedureID           string    `json:"procedure_id"`
	RiskID                string    `json:"risk_id"`
	TenantID              string    `json:"tenant_id"`
	Description           string    `json:"description"`
	DesignedByPrincipalID string    `json:"designed_by_principal_id"`
	CreatedAt             time.Time `json:"created_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateAuditPlanParams struct {
	EngagementID, TenantID, CreatedByPrincipalID, CorrelationID string
}

type RecordMaterialityParams struct {
	PlanID, TenantID, ActorPrincipalID, CorrelationID string
	OverallMateriality, PerformanceMateriality        float64
	ClearlyTrivialThreshold                           *float64
	Rationale                                         string
}

type IdentifyRiskParams struct {
	PlanID, EngagementID, TenantID, Description, RiskLevel, CreatedByPrincipalID, CorrelationID string
}

type AssessRiskParams struct {
	RiskID, TenantID, ActorPrincipalID, CorrelationID string
}

type MarkSignificantRiskParams struct {
	RiskID, TenantID, ActorPrincipalID, CorrelationID string
}

type LinkAssertionParams struct {
	RiskID, TenantID, AssertionCode string
}

type DesignAuditResponseParams struct {
	RiskID, TenantID, Description, DesignedByPrincipalID string
}

type ApprovePlanParams struct {
	PlanID, TenantID, ActorPrincipalID, CorrelationID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var ErrAuditPlanNotFound = errorString("audit plan not found")
var ErrAuditPlanInvalidState = errorString("audit plan is not in a state that permits this action")
var ErrAuditPlanAlreadyExists = errorString("engagement already has a live audit plan")
var ErrAuditRiskNotFound = errorString("risk assessment not found")
var ErrAuditRiskInvalidState = errorString("risk assessment is not in a state that permits this action")

// ErrAuditRiskRequiresAssertionAndResponse is AssessRisk's own CAS
// predicate failure — the DB-enforced form of "every risk links to
// assertions/process and response" (a risk with no linked assertion or no
// planned procedure cannot leave IDENTIFIED).
var ErrAuditRiskRequiresAssertionAndResponse = errorString("risk must have at least one linked assertion and one planned procedure before it can be assessed")

// ErrAuditPlanSelfApproval mirrors ErrAuditEngagementSelfAcceptance — the
// plan's own preparer may not approve it.
var ErrAuditPlanSelfApproval = errorString("plan preparer may not record the approval decision")
