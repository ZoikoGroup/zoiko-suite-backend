package domain

import (
	"errors"
	"time"
)

// AUD-08 Finding/Exception wraps ExceptionCase via a mandatory
// exception_case_id FK rather than adding audit-only columns directly
// onto it — ExceptionCase is the generic cross-domain exception primitive
// already used for non-audit escalations. This reuses the proven
// EscalationRecord/self-approval machinery for the communicate/escalate
// path while keeping engagement/materiality/finding-type fields out of
// the shared table.

type FindingType string

const (
	FindingTypeMisstatement      FindingType = "MISSTATEMENT"
	FindingTypeControlDeficiency FindingType = "CONTROL_DEFICIENCY"
	FindingTypeDeviation         FindingType = "DEVIATION"
	FindingTypeScopeLimitation   FindingType = "SCOPE_LIMITATION"
)

type FindingStatus string

const (
	FindingIdentified         FindingStatus = "IDENTIFIED"
	FindingEvaluating         FindingStatus = "EVALUATING"
	FindingCommunicated       FindingStatus = "COMMUNICATED"
	FindingManagementResponse FindingStatus = "MANAGEMENT_RESPONSE"
	FindingRemediationTesting FindingStatus = "REMEDIATION_TESTING"
	FindingClosed             FindingStatus = "CLOSED"
	FindingOpenAtReport       FindingStatus = "OPEN_AT_REPORT"
	FindingReopened           FindingStatus = "REOPENED"
)

// AuditFinding is AUD-08's own "AuditFinding" — a reject-mutation trigger
// allows only status/reopened_count/closed_at to change post-insert,
// which is the real, DB-enforced form of "original finding immutable."
type AuditFinding struct {
	FindingID                   string        `json:"finding_id"`
	ExceptionCaseID             string        `json:"exception_case_id"`
	TenantID                    string        `json:"tenant_id"`
	LegalEntityID               string        `json:"legal_entity_id"`
	EngagementID                string        `json:"engagement_id"`
	FindingType                 FindingType   `json:"finding_type"`
	RequiresRemediationEvidence bool          `json:"requires_remediation_evidence"`
	Status                      FindingStatus `json:"status"`
	ReopenedCount               int           `json:"reopened_count"`
	CreatedByPrincipalID        string        `json:"created_by_principal_id"`
	CreatedAt                   time.Time     `json:"created_at"`
	ClosedAt                    *time.Time    `json:"closed_at,omitempty"`
}

// MisstatementRecord: a correction is a NEW row with CorrectsMisstatementID
// set, never an UPDATE of the original — AUD-NEG-027's own mechanism.
type MisstatementRecord struct {
	MisstatementID         string    `json:"misstatement_id"`
	FindingID              string    `json:"finding_id"`
	Amount                 float64   `json:"amount"`
	CorrectsMisstatementID *string   `json:"corrects_misstatement_id,omitempty"`
	IsCorrected            bool      `json:"is_corrected"`
	RecordedByPrincipalID  string    `json:"recorded_by_principal_id"`
	RecordedAt             time.Time `json:"recorded_at"`
}

type MaterialityEvaluation struct {
	EvaluationID           string    `json:"evaluation_id"`
	FindingID              string    `json:"finding_id"`
	Version                int       `json:"version"`
	IsMaterial             bool      `json:"is_material"`
	QualitativeNotes       string    `json:"qualitative_notes"`
	EvaluatedByPrincipalID string    `json:"evaluated_by_principal_id"`
	EvaluatedAt            time.Time `json:"evaluated_at"`
}

type ControlDeficiencyRecord struct {
	DeficiencyID          string    `json:"deficiency_id"`
	FindingID             string    `json:"finding_id"`
	ControlDescription    string    `json:"control_description"`
	DeficiencySeverity    string    `json:"deficiency_severity"`
	RecordedByPrincipalID string    `json:"recorded_by_principal_id"`
	RecordedAt            time.Time `json:"recorded_at"`
}

// ScopeLimitation's StillImpactsReport stays true independent of the
// parent finding's own status — the AUD-NEG-029 mechanism: CloseFinding
// never touches this table, and the completion-gate query for open
// limitations reads still_impacts_report directly, so closing the
// finding cannot hide it.
type ScopeLimitation struct {
	LimitationID          string     `json:"limitation_id"`
	FindingID             string     `json:"finding_id"`
	Description           string     `json:"description"`
	StillImpactsReport    bool       `json:"still_impacts_report"`
	RecordedByPrincipalID string     `json:"recorded_by_principal_id"`
	RecordedAt            time.Time  `json:"recorded_at"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
}

type ManagementResponse struct {
	ResponseID             string    `json:"response_id"`
	FindingID              string    `json:"finding_id"`
	ResponseText           string    `json:"response_text"`
	RemediationPlan        string    `json:"remediation_plan"`
	RespondedByPrincipalID string    `json:"responded_by_principal_id"`
	RespondedAt            time.Time `json:"responded_at"`
}

type RemediationEvidence struct {
	RemediationID         string    `json:"remediation_id"`
	FindingID             string    `json:"finding_id"`
	EvidenceRef           string    `json:"evidence_ref"`
	Reperformed           bool      `json:"reperformed"`
	RecordedByPrincipalID string    `json:"recorded_by_principal_id"`
	RecordedAt            time.Time `json:"recorded_at"`
}

type FindingClosureAssessment struct {
	AssessmentID        string    `json:"assessment_id"`
	FindingID           string    `json:"finding_id"`
	ClosedByPrincipalID string    `json:"closed_by_principal_id"`
	ClosureNotes        string    `json:"closure_notes"`
	ClosedAt            time.Time `json:"closed_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateFindingParams struct {
	ExceptionCaseID, TenantID, LegalEntityID, EngagementID, CreatedByPrincipalID string
	FindingType                                                                  FindingType
	RequiresRemediationEvidence                                                  bool
}

type AccumulateMisstatementParams struct {
	FindingID, TenantID, RecordedByPrincipalID string
	Amount                                     float64
	CorrectsMisstatementID                     *string
}

type RecordMaterialityEvaluationParams struct {
	FindingID, TenantID, QualitativeNotes, EvaluatedByPrincipalID string
	IsMaterial                                                    bool
}

type CommunicateFindingParams struct {
	FindingID, TenantID string
}

type RecordManagementResponseParams struct {
	FindingID, TenantID, ResponseText, RemediationPlan, RespondedByPrincipalID string
}

type LinkRemediationParams struct {
	FindingID, TenantID, EvidenceRef, RecordedByPrincipalID string
	Reperformed                                             bool
}

type RecordScopeLimitationParams struct {
	FindingID, TenantID, Description, RecordedByPrincipalID string
}

type ResolveScopeLimitationParams struct {
	LimitationID, TenantID string
}

type CloseFindingParams struct {
	FindingID, TenantID, ClosedByPrincipalID, ClosureNotes string
}

type ReopenFindingParams struct {
	FindingID, TenantID, ActorPrincipalID, Reason string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrFindingNotFound         = errors.New("audit finding not found")
	ErrFindingInvalidState     = errors.New("audit finding is not in a state that permits this action")
	ErrClosureEvidenceRequired = errors.New("finding requires remediation evidence before it can be closed")
	ErrScopeLimitationNotFound = errors.New("scope limitation not found")
)
