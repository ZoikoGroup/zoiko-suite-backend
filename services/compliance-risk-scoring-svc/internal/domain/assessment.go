package domain

import (
	"strings"
	"time"
)

// SubjectType is what a score is about. A score always belongs to a legal
// entity; it may additionally target one obligation, filing or control
// (03-microservices.md §15.3: "Scores obligations, filings, and control
// exposures"; 04-data-model.md ComplianceRiskScore.obligation_id nullable).
type SubjectType string

const (
	SubjectLegalEntity SubjectType = "LEGAL_ENTITY"
	SubjectObligation  SubjectType = "OBLIGATION"
	SubjectFiling      SubjectType = "FILING"
	SubjectControl     SubjectType = "CONTROL"
)

// IsValid reports whether s is a known subject type.
func (s SubjectType) IsValid() bool {
	switch s {
	case SubjectLegalEntity, SubjectObligation, SubjectFiling, SubjectControl:
		return true
	}
	return false
}

// ScoreSubject identifies the thing being scored.
type ScoreSubject struct {
	LegalEntityID string
	Type          SubjectType
	ID            string
}

// ScoreStatus is the lifecycle of a stored score.
//
//	ACTIVE --(newer score for same subject)--> SUPERSEDED
//	ACTIVE / SUPERSEDED --(archive)--> ARCHIVED   (terminal)
//
// Scores are never deleted.
type ScoreStatus string

const (
	ScoreActive     ScoreStatus = "ACTIVE"
	ScoreSuperseded ScoreStatus = "SUPERSEDED"
	ScoreArchived   ScoreStatus = "ARCHIVED"
)

// IsValid reports whether s is a known score status.
func (s ScoreStatus) IsValid() bool {
	switch s {
	case ScoreActive, ScoreSuperseded, ScoreArchived:
		return true
	}
	return false
}

// ReviewStatus is the human-review lifecycle of an advisory score.
//
//	NOT_REQUIRED                       (LOW / MODERATE; no review step)
//	PENDING_REVIEW --> CONFIRMED | DISMISSED | ESCALATED
//	ESCALATED      --> CONFIRMED | DISMISSED
type ReviewStatus string

const (
	ReviewNotRequired ReviewStatus = "NOT_REQUIRED"
	ReviewPending     ReviewStatus = "PENDING_REVIEW"
	ReviewConfirmed   ReviewStatus = "CONFIRMED"
	ReviewDismissed   ReviewStatus = "DISMISSED"
	ReviewEscalated   ReviewStatus = "ESCALATED"
)

// ReviewDisposition is what a reviewer decides.
type ReviewDisposition string

const (
	DispositionConfirmed ReviewDisposition = "CONFIRMED"
	DispositionDismissed ReviewDisposition = "DISMISSED"
	DispositionEscalated ReviewDisposition = "ESCALATED"
)

// IsValid reports whether d is a known disposition.
func (d ReviewDisposition) IsValid() bool {
	switch d {
	case DispositionConfirmed, DispositionDismissed, DispositionEscalated:
		return true
	}
	return false
}

// AssuranceAdvisory labels every score: it is a derived, advisory output,
// never a certified fact (AI spec §3 "Derived assurance").
const AssuranceAdvisory = "ADVISORY"

// FactorBreakdown is the stored, explainable contribution of one category.
type FactorBreakdown struct {
	ID                    string       `json:"id"`
	RiskCategory          RiskCategory `json:"risk_category"`
	InputCount            int64        `json:"input_count"`
	SaturationCount       int64        `json:"saturation_count"`
	CategoryWeight        Hundredths   `json:"category_weight_percent"`
	RawScore              Hundredths   `json:"raw_score"`
	WeightedScore         Hundredths   `json:"weighted_score"`
	CategoryThresholdBand *RiskTier    `json:"category_threshold_band"`
	RiskDriverSummary     string       `json:"risk_driver_summary"`
	CreatedAt             time.Time    `json:"created_at"`
}

// RiskScoreAssessment is one stored score with its full explanation.
type RiskScoreAssessment struct {
	ID             string      `json:"id"`
	TenantID       string      `json:"tenant_id"`
	LegalEntityID  string      `json:"legal_entity_id"`
	SubjectType    SubjectType `json:"subject_type"`
	SubjectID      string      `json:"subject_id"`
	AssessmentName string      `json:"assessment_name"`

	OpenObligationsCount  int64 `json:"open_obligations_count"`
	PolicyViolationsCount int64 `json:"policy_violations_count"`
	AuditExceptionsCount  int64 `json:"audit_exceptions_count"`
	PrivacyIncidentsCount int64 `json:"privacy_incidents_count"`
	TaxPenaltiesCount     int64 `json:"tax_penalties_count"`

	CompositeRiskScore Hundredths        `json:"composite_risk_score"`
	RiskTier           RiskTier          `json:"risk_tier"`
	ModelVersion       string            `json:"model_version"`
	InputHash          string            `json:"input_hash"`
	ThresholdsApplied  AppliedThresholds `json:"thresholds_applied"`
	BandRationale      string            `json:"band_rationale"`
	AssuranceState     string            `json:"assurance_state"`

	RequiresHumanReview bool         `json:"requires_human_review"`
	ReviewStatus        ReviewStatus `json:"review_status"`
	ReviewedBy          *string      `json:"reviewed_by"`
	ReviewedAt          *time.Time   `json:"reviewed_at"`
	ReviewNote          *string      `json:"review_note"`

	Status           ScoreStatus `json:"status"`
	PreviousScoreID  *string     `json:"previous_score_id"`
	ScoreDelta       *Hundredths `json:"score_delta"`
	SupersededByID   *string     `json:"superseded_by_id"`
	SourceReferences []string    `json:"source_references"`

	CalculatedBy  string     `json:"calculated_by"`
	Version       int        `json:"version"`
	EvaluatedAt   time.Time  `json:"evaluated_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ArchivedAt    *time.Time `json:"archived_at"`
	ArchivedBy    *string    `json:"archived_by"`
	ArchiveReason *string    `json:"archive_reason"`

	FactorBreakdowns []FactorBreakdown `json:"factor_breakdowns"`
}

// CanArchive checks the archive transition and its business guard: an
// unreviewed HIGH/CRITICAL score cannot be archived, because archiving would
// hide a risk signal nobody has looked at.
func (a *RiskScoreAssessment) CanArchive() *Error {
	if a.Status == ScoreArchived {
		return InvalidTransition("risk score", string(a.Status), "archived")
	}
	if a.ReviewStatus == ReviewPending || a.ReviewStatus == ReviewEscalated {
		return Conflict("review_outstanding",
			"risk score has review_status "+string(a.ReviewStatus)+"; record a review disposition before archiving")
	}
	return nil
}

// CanReview checks the review transition for disposition by reviewer.
func (a *RiskScoreAssessment) CanReview(disposition ReviewDisposition, reviewerID string) *Error {
	if a.Status == ScoreArchived {
		return InvalidTransition("risk score", string(a.Status), "reviewed")
	}

	switch a.ReviewStatus {
	case ReviewPending:
		// any disposition allowed
	case ReviewEscalated:
		if disposition == DispositionEscalated {
			return InvalidTransition("risk score review", string(a.ReviewStatus), "escalated again")
		}
	default:
		return InvalidTransition("risk score review", string(a.ReviewStatus), "reviewed")
	}

	// Segregation of duties: whoever ran the calculation cannot also be the
	// person who confirms or dismisses it.
	if reviewerID == a.CalculatedBy {
		return Forbidden("segregation_of_duties", "the principal who calculated a score cannot review it")
	}
	return nil
}

// CalculateRiskScoreRequest is the POST /v1/risk-scores/calculate body.
type CalculateRiskScoreRequest struct {
	TenantID              string   `json:"tenant_id,omitempty"`
	LegalEntityID         string   `json:"legal_entity_id"`
	SubjectType           string   `json:"subject_type,omitempty"`
	SubjectID             string   `json:"subject_id,omitempty"`
	AssessmentName        string   `json:"assessment_name"`
	OpenObligationsCount  *int64   `json:"open_obligations_count"`
	PolicyViolationsCount *int64   `json:"policy_violations_count"`
	AuditExceptionsCount  *int64   `json:"audit_exceptions_count"`
	PrivacyIncidentsCount *int64   `json:"privacy_incidents_count"`
	TaxPenaltiesCount     *int64   `json:"tax_penalties_count"`
	SourceReferences      []string `json:"source_references,omitempty"`
}

// Input limits.
const (
	MaxIdentifierLength     = 64
	MaxSubjectIDLength      = 128
	MaxNameLength           = 128
	MaxCountValue           = 1_000_000
	MaxSourceReferences     = 50
	MaxSourceReferenceChars = 256
	MaxReviewNoteChars      = 2000
)

// Normalize trims strings and applies defaults: subject_type defaults to
// LEGAL_ENTITY, and for LEGAL_ENTITY the subject_id is the legal entity.
func (r *CalculateRiskScoreRequest) Normalize() {
	r.LegalEntityID = strings.TrimSpace(r.LegalEntityID)
	r.AssessmentName = strings.TrimSpace(r.AssessmentName)
	r.SubjectType = strings.ToUpper(strings.TrimSpace(r.SubjectType))
	r.SubjectID = strings.TrimSpace(r.SubjectID)

	if r.SubjectType == "" {
		r.SubjectType = string(SubjectLegalEntity)
	}
	if r.SubjectType == string(SubjectLegalEntity) && r.SubjectID == "" {
		r.SubjectID = r.LegalEntityID
	}
}

// Validate checks every field and reports all problems at once.
// Call Normalize first.
func (r *CalculateRiskScoreRequest) Validate() []FieldError {
	var problems []FieldError
	add := func(field, detail string) { problems = append(problems, FieldError{Field: field, Detail: detail}) }

	if r.LegalEntityID == "" {
		add("legal_entity_id", "is required")
	} else if len(r.LegalEntityID) > MaxIdentifierLength {
		add("legal_entity_id", "must be at most 64 characters")
	}

	if r.AssessmentName == "" {
		add("assessment_name", "is required")
	} else if len(r.AssessmentName) > MaxNameLength {
		add("assessment_name", "must be at most 128 characters")
	}

	subjectType := SubjectType(r.SubjectType)
	if !subjectType.IsValid() {
		add("subject_type", "must be one of LEGAL_ENTITY, OBLIGATION, FILING, CONTROL")
	} else if subjectType == SubjectLegalEntity && r.SubjectID != r.LegalEntityID {
		add("subject_id", "for subject_type LEGAL_ENTITY must equal legal_entity_id (or be omitted)")
	}
	if r.SubjectID == "" {
		add("subject_id", "is required when subject_type is not LEGAL_ENTITY")
	} else if len(r.SubjectID) > MaxSubjectIDLength {
		add("subject_id", "must be at most 128 characters")
	}

	checkCount := func(field string, value *int64) {
		if value == nil {
			add(field, "is required (use 0 when there are none)")
			return
		}
		if *value < 0 || *value > MaxCountValue {
			add(field, "must be between 0 and 1000000")
		}
	}
	checkCount("open_obligations_count", r.OpenObligationsCount)
	checkCount("policy_violations_count", r.PolicyViolationsCount)
	checkCount("audit_exceptions_count", r.AuditExceptionsCount)
	checkCount("privacy_incidents_count", r.PrivacyIncidentsCount)
	checkCount("tax_penalties_count", r.TaxPenaltiesCount)

	if len(r.SourceReferences) > MaxSourceReferences {
		add("source_references", "at most 50 references")
	}
	for _, reference := range r.SourceReferences {
		if strings.TrimSpace(reference) == "" || len(reference) > MaxSourceReferenceChars {
			add("source_references", "each reference must be non-empty and at most 256 characters")
			break
		}
	}

	return problems
}

// Inputs converts the validated request into scoring inputs.
func (r *CalculateRiskScoreRequest) Inputs() ScoringInputs {
	return ScoringInputs{
		OpenObligationsCount:  *r.OpenObligationsCount,
		PolicyViolationsCount: *r.PolicyViolationsCount,
		AuditExceptionsCount:  *r.AuditExceptionsCount,
		PrivacyIncidentsCount: *r.PrivacyIncidentsCount,
		TaxPenaltiesCount:     *r.TaxPenaltiesCount,
	}
}

// Subject returns the validated subject.
func (r *CalculateRiskScoreRequest) Subject() ScoreSubject {
	return ScoreSubject{LegalEntityID: r.LegalEntityID, Type: SubjectType(r.SubjectType), ID: r.SubjectID}
}

// ReviewRiskScoreRequest is the POST /v1/risk-scores/{id}/review body.
type ReviewRiskScoreRequest struct {
	TenantID    string `json:"tenant_id,omitempty"`
	Disposition string `json:"disposition"`
	Note        string `json:"note"`
}

// Validate checks the review body.
func (r *ReviewRiskScoreRequest) Validate() []FieldError {
	var problems []FieldError
	r.Disposition = strings.ToUpper(strings.TrimSpace(r.Disposition))
	r.Note = strings.TrimSpace(r.Note)

	if !ReviewDisposition(r.Disposition).IsValid() {
		problems = append(problems, FieldError{Field: "disposition", Detail: "must be one of CONFIRMED, DISMISSED, ESCALATED"})
	}
	if r.Note == "" {
		problems = append(problems, FieldError{Field: "note", Detail: "is required: a review must record its rationale"})
	} else if len(r.Note) > MaxReviewNoteChars {
		problems = append(problems, FieldError{Field: "note", Detail: "must be at most 2000 characters"})
	}
	return problems
}

// ArchiveRiskScoreRequest is the optional POST /v1/risk-scores/{id}/archive body.
type ArchiveRiskScoreRequest struct {
	TenantID string `json:"tenant_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// MaxArchiveReasonChars bounds the archive reason.
const MaxArchiveReasonChars = 500
