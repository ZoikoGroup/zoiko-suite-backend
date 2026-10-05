package domain

import "time"

// AIG-05 Evaluation, Monitoring, Incident & Change Governance Service
// (ZS-SVC-X-001 §8).
//
// AIEvaluation and AIIncident are both caller-attested evidence — no
// real eval-running pipeline or production telemetry exists in this
// codebase to generate them automatically (AIG-03, the execution
// gateway that would produce real signals, was deliberately not
// built), so an external evaluator/operator submits results and this
// service owns the governance consequences, not the measurement
// itself.

type EvaluationDimension string

const (
	DimensionTaskQuality     EvaluationDimension = "TASK_QUALITY"
	DimensionSafety          EvaluationDimension = "SAFETY"
	DimensionDomainIntegrity EvaluationDimension = "DOMAIN_INTEGRITY"
	DimensionFairnessImpact  EvaluationDimension = "FAIRNESS_IMPACT"
	DimensionRobustness      EvaluationDimension = "ROBUSTNESS"
	DimensionPrivacySecurity EvaluationDimension = "PRIVACY_SECURITY"
	DimensionOperations      EvaluationDimension = "OPERATIONS"
	DimensionHumanFactors    EvaluationDimension = "HUMAN_FACTORS"
)

func (d EvaluationDimension) Valid() bool {
	switch d {
	case DimensionTaskQuality, DimensionSafety, DimensionDomainIntegrity, DimensionFairnessImpact,
		DimensionRobustness, DimensionPrivacySecurity, DimensionOperations, DimensionHumanFactors:
		return true
	}
	return false
}

type EvaluationResult string

const (
	EvaluationPass EvaluationResult = "PASS"
	EvaluationFail EvaluationResult = "FAIL"
)

// AIEvaluation is one dimension's evaluation run against a model
// release — §8's "immutable release evidence." A release accumulates
// several of these as its mandatory suites are run.
type AIEvaluation struct {
	EvaluationID           string                 `json:"evaluation_id"`
	ModelReleaseID         string                 `json:"model_release_id"`
	UseCaseID              string                 `json:"use_case_id,omitempty"`
	Dimension              EvaluationDimension    `json:"dimension"`
	DatasetVersion         string                 `json:"dataset_version"`
	Metrics                map[string]interface{} `json:"metrics,omitempty"`
	Thresholds             map[string]interface{} `json:"thresholds,omitempty"`
	Result                 EvaluationResult       `json:"result"`
	Defects                []string               `json:"defects,omitempty"`
	EvaluatedByPrincipalID string                 `json:"evaluated_by_principal_id"`
	EvaluatedAt            time.Time              `json:"evaluated_at"`
}

type CreateEvaluationRequest struct {
	ModelReleaseID string                 `json:"model_release_id"`
	UseCaseID      string                 `json:"use_case_id,omitempty"`
	Dimension      string                 `json:"dimension"`
	DatasetVersion string                 `json:"dataset_version"`
	Metrics        map[string]interface{} `json:"metrics,omitempty"`
	Thresholds     map[string]interface{} `json:"thresholds,omitempty"`
	Result         string                 `json:"result"`
	Defects        []string               `json:"defects,omitempty"`
}

// IncidentSeverity is §8.4's AI-P0..P3 scale. AI-P0 triggers immediate
// quarantine of the affected release; AI-P1 restricts it — both
// enforced structurally by ReportIncident, not left to a follow-up
// manual step.
type IncidentSeverity string

const (
	SeverityAIP0 IncidentSeverity = "AI-P0"
	SeverityAIP1 IncidentSeverity = "AI-P1"
	SeverityAIP2 IncidentSeverity = "AI-P2"
	SeverityAIP3 IncidentSeverity = "AI-P3"
)

func (s IncidentSeverity) Valid() bool {
	switch s {
	case SeverityAIP0, SeverityAIP1, SeverityAIP2, SeverityAIP3:
		return true
	}
	return false
}

type IncidentState string

const (
	IncidentOpen      IncidentState = "OPEN"
	IncidentContained IncidentState = "CONTAINED"
	IncidentResolved  IncidentState = "RESOLVED"
	IncidentClosed    IncidentState = "CLOSED"
)

// AIIncident is §8.4/§8.6's incident record. exception_case_ref is a
// logical (non-FK) reference to an exception-escalation-svc
// ExceptionCase — services in this platform do not share a database,
// so cross-service links are always caller-attested identifiers.
type AIIncident struct {
	IncidentID            string           `json:"incident_id"`
	Severity              IncidentSeverity `json:"severity"`
	ModelReleaseID        string           `json:"model_release_id,omitempty"`
	UseCaseID             string           `json:"use_case_id,omitempty"`
	ExceptionCaseRef      string           `json:"exception_case_ref,omitempty"`
	IncidentState         IncidentState    `json:"incident_state"`
	Description           string           `json:"description"`
	ContainmentAction     string           `json:"containment_action,omitempty"`
	RootCause             string           `json:"root_cause,omitempty"`
	CorrectiveActions     string           `json:"corrective_actions,omitempty"`
	ClosureEvidence       string           `json:"closure_evidence,omitempty"`
	ReportedByPrincipalID string           `json:"reported_by_principal_id"`
	ReportedAt            time.Time        `json:"reported_at"`
	ContainedAt           *time.Time       `json:"contained_at,omitempty"`
	ResolvedAt            *time.Time       `json:"resolved_at,omitempty"`
	ClosedAt              *time.Time       `json:"closed_at,omitempty"`
	ClosedByPrincipalID   string           `json:"closed_by_principal_id,omitempty"`
}

// ReportIncidentRequest files a new incident. At least one of
// ModelReleaseID/UseCaseID is required. For AI-P0, the referenced
// model release (if any) is quarantined in the same transaction; for
// AI-P1, it is restricted — these are the only two severities the doc
// names an immediate release-state consequence for.
type ReportIncidentRequest struct {
	Severity         string `json:"severity"`
	ModelReleaseID   string `json:"model_release_id,omitempty"`
	UseCaseID        string `json:"use_case_id,omitempty"`
	ExceptionCaseRef string `json:"exception_case_ref,omitempty"`
	Description      string `json:"description"`
}

type ContainIncidentRequest struct {
	ContainmentAction string `json:"containment_action"`
}

type ResolveIncidentRequest struct {
	RootCause         string `json:"root_cause"`
	CorrectiveActions string `json:"corrective_actions"`
}

type CloseIncidentRequest struct {
	ClosureEvidence string `json:"closure_evidence"`
}

// ReactivateReleaseRequest is AIG-05's governed reactivation gate
// (§8.6): a QUARANTINED release may only return to ACTIVE once root
// cause is documented (via a RESOLVED/CLOSED incident referencing it)
// and a PASS evaluation has been recorded for it AFTER that incident
// was reported — proving the corrective change was actually
// re-evaluated, not just asserted fixed.
type ReactivateReleaseRequest struct {
	IncidentID string `json:"incident_id"`
	Reason     string `json:"reason"`
}

// ── errors ───────────────────────────────────────────────────────────────────

const (
	// AIG-05
	ErrInvalidEvaluationDimension = errorString("invalid dimension")
	ErrInvalidEvaluationResult    = errorString("result must be PASS or FAIL")
	ErrEvaluationNotFound         = errorString("ai evaluation not found")

	ErrInvalidIncidentSeverity = errorString("severity must be AI-P0, AI-P1, AI-P2 or AI-P3")
	ErrIncidentMissingScope    = errorString("at least one of model_release_id or use_case_id is required")
	ErrIncidentNotFound        = errorString("ai incident not found")
	ErrIncidentNotOpen         = errorString("ai incident is not OPEN")
	ErrIncidentNotContained    = errorString("ai incident is not CONTAINED")
	ErrIncidentNotResolved     = errorString("ai incident is not RESOLVED")

	ErrReactivationIncidentNotFound            = errorString("referenced incident not found")
	ErrReactivationIncidentNotClosedOrResolved = errorString("referenced incident must be RESOLVED or CLOSED with a documented root cause")
	ErrReactivationIncidentScopeMismatch       = errorString("referenced incident does not name this model release")
	ErrReactivationNoPassingReevaluation       = errorString("no PASS evaluation recorded for this release after the incident was reported")
)
