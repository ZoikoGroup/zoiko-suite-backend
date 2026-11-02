package domain

import (
	"regexp"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// AI-05 Forecast Assist (Data/Search/Analytics/AI spec, AI-05).
//
// Forecast Assist proposes drivers, ranges and narrative for planners. It is
// strictly ADVISORY:
//
//   - It never writes to forecasts or projections (spec: "No direct write to
//     approved forecasts"; failure mode N-20 "Forecast Assist edits approved
//     forecast -> Reject; create planner-reviewed candidate").
//   - Accepting a suggestion records a PlannerDecision and emits
//     AI.ForecastSuggestionAccepted so the FIN-02 owner can create a normal
//     forecast candidate through its own governed commands.
//   - Every suggestion must cite at least one evidence source (spec: "explicit
//     uncertainty and source basis"; FP&A AI boundary: "suggestions require
//     source links").
//   - A model/provider/version may only be used after its release has been
//     registered AND its evaluation approved by a different principal (spec:
//     "Model/provider change requires evaluation before material use";
//     "publication separated from evaluation/release approval").
//
// Job lifecycle (spec: "Queued -> Generated -> PlannerReview -> Accepted /
// Rejected"):
//
//	Generated --start-review--> PlannerReview --accept--> Accepted
//	                                          --reject--> Rejected
//
// Suggestions are generated synchronously in the creating request, so a job is
// born Generated; Queued exists in the schema for a future asynchronous mode.
// ─────────────────────────────────────────────────────────────────────────────

// ID prefixes. Every assist identifier is "<prefix><uuid>".
const (
	PrefixForecastModelRelease = "fmr_"
	PrefixForecastAssistJob    = "faj_"
	PrefixSuggestedDriver      = "sdr_"
	PrefixSuggestedRange       = "srg_"
	PrefixEvidenceReference    = "evr_"
	PrefixPlannerDecision      = "pld_"
)

var prefixedIDPattern = regexp.MustCompile(`^[a-z]{3}_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsValidPrefixedID reports whether id looks like "<prefix><lower-case uuid>".
func IsValidPrefixedID(id, prefix string) bool {
	return strings.HasPrefix(id, prefix) && prefixedIDPattern.MatchString(id)
}

// ── Model releases ──────────────────────────────────────────────────────────

type ModelReleaseStatus string

const (
	ModelReleasePendingEvaluation ModelReleaseStatus = "PendingEvaluation"
	ModelReleaseApproved          ModelReleaseStatus = "Approved"
)

func (s ModelReleaseStatus) Valid() bool {
	return s == ModelReleasePendingEvaluation || s == ModelReleaseApproved
}

// ForecastModelRelease registers one (domain, provider, version) model. It is
// usable by suggestion commands only once Status is Approved.
type ForecastModelRelease struct {
	ReleaseID           string             `json:"release_id"`
	TenantID            string             `json:"tenant_id"`
	DomainName          string             `json:"domain_name"`
	ModelProvider       string             `json:"model_provider"`
	ModelVersion        string             `json:"model_version"`
	Status              ModelReleaseStatus `json:"status"`
	Version             int                `json:"version"`
	EvaluationReference string             `json:"evaluation_reference,omitempty"`
	CreatedBy           string             `json:"created_by"`
	CreatedAt           time.Time          `json:"created_at"`
	ApprovedBy          string             `json:"approved_by,omitempty"`
	ApprovedAt          *time.Time         `json:"approved_at,omitempty"`
}

type RegisterModelReleaseRequest struct {
	DomainName    string `json:"domain_name"`
	ModelProvider string `json:"model_provider"`
	ModelVersion  string `json:"model_version"`
}

func (r *RegisterModelReleaseRequest) Validate() error {
	if err := requireText("domain_name", r.DomainName, 128); err != nil {
		return err
	}
	if err := requireText("model_provider", r.ModelProvider, 128); err != nil {
		return err
	}
	return requireText("model_version", r.ModelVersion, 128)
}

type ApproveModelReleaseRequest struct {
	EvaluationReference string `json:"evaluation_reference"`
}

func (r *ApproveModelReleaseRequest) Validate() error {
	return requireText("evaluation_reference", r.EvaluationReference, 256)
}

type ModelReleaseListFilter struct {
	Status ModelReleaseStatus
	Page   PageRequest
}

// ── Jobs and suggestions ────────────────────────────────────────────────────

type AssistJobStatus string

const (
	AssistJobQueued        AssistJobStatus = "Queued"
	AssistJobGenerated     AssistJobStatus = "Generated"
	AssistJobPlannerReview AssistJobStatus = "PlannerReview"
	AssistJobAccepted      AssistJobStatus = "Accepted"
	AssistJobRejected      AssistJobStatus = "Rejected"
)

func (s AssistJobStatus) Valid() bool {
	switch s {
	case AssistJobQueued, AssistJobGenerated, AssistJobPlannerReview, AssistJobAccepted, AssistJobRejected:
		return true
	}
	return false
}

// IsOpenForSuggestions reports whether more drivers/ranges/narrative may be
// appended. Once a planner starts reviewing, the content is frozen.
func (s AssistJobStatus) IsOpenForSuggestions() bool {
	return s == AssistJobQueued || s == AssistJobGenerated
}

// ForecastAssistJob is one planning-assist session. The model context and plan
// version are frozen at creation; narrative is write-once.
type ForecastAssistJob struct {
	JobID           string          `json:"job_id"`
	TenantID        string          `json:"tenant_id"`
	LegalEntityID   string          `json:"legal_entity_id"`
	DomainName      string          `json:"domain_name"`
	ModelProvider   string          `json:"model_provider"`
	ModelVersion    string          `json:"model_version"`
	PlanVersion     string          `json:"plan_version"`
	PlanningPurpose string          `json:"planning_purpose"`
	Narrative       string          `json:"narrative,omitempty"`
	Status          AssistJobStatus `json:"status"`
	Version         int             `json:"version"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
	ReviewStartedBy string          `json:"review_started_by,omitempty"`
}

// DriverInput is one proposed driver value.
type DriverInput struct {
	DriverName     string  `json:"driver_name"`
	SuggestedValue float64 `json:"suggested_value"`
	Rationale      string  `json:"rationale"`
}

// RangeInput is one proposed uncertainty range for a period.
type RangeInput struct {
	PeriodLabel    string  `json:"period_label"`
	LowValue       float64 `json:"low_value"`
	HighValue      float64 `json:"high_value"`
	ConfidenceNote string  `json:"confidence_note"`
}

// EvidenceInput is one source the suggestion is based on.
type EvidenceInput struct {
	SourceRef   string `json:"source_ref"`
	Description string `json:"description"`
}

// JobContext is the part of every suggestion command that either starts a new
// job (JobID empty: all model/plan fields required) or appends to an existing
// open job (JobID set: the job's frozen context wins, other fields ignored).
type JobContext struct {
	JobID           string `json:"job_id,omitempty"`
	DomainName      string `json:"domain_name"`
	ModelProvider   string `json:"model_provider"`
	ModelVersion    string `json:"model_version"`
	PlanVersion     string `json:"plan_version"`
	PlanningPurpose string `json:"planning_purpose"`
}

// StartsNewJob reports whether the command creates a job.
func (c JobContext) StartsNewJob() bool { return c.JobID == "" }

func (c JobContext) validate() error {
	if !c.StartsNewJob() {
		if !IsValidPrefixedID(c.JobID, PrefixForecastAssistJob) {
			return NewValidationError("job_id", "job_id must look like faj_<uuid>")
		}
		return nil
	}
	if err := requireText("domain_name", c.DomainName, 128); err != nil {
		return err
	}
	if err := requireText("model_provider", c.ModelProvider, 128); err != nil {
		return err
	}
	if err := requireText("model_version", c.ModelVersion, 128); err != nil {
		return err
	}
	if err := requireText("plan_version", c.PlanVersion, 128); err != nil {
		return err
	}
	return requireText("planning_purpose", c.PlanningPurpose, 128)
}

func validateEvidence(evidence []EvidenceInput) error {
	if len(evidence) == 0 {
		return NewValidationError("evidence", "at least one evidence reference is required: AI suggestions must cite their source basis")
	}
	for _, item := range evidence {
		if err := requireText("evidence.source_ref", item.SourceRef, 256); err != nil {
			return err
		}
	}
	return nil
}

type SuggestDriversRequest struct {
	JobContext
	Drivers  []DriverInput   `json:"drivers"`
	Evidence []EvidenceInput `json:"evidence"`
}

func (r *SuggestDriversRequest) Validate() error {
	if err := r.JobContext.validate(); err != nil {
		return err
	}
	if len(r.Drivers) == 0 {
		return NewValidationError("drivers", "at least one driver is required")
	}
	for _, driver := range r.Drivers {
		if err := requireText("drivers.driver_name", driver.DriverName, 128); err != nil {
			return err
		}
		if err := requireText("drivers.rationale", driver.Rationale, 4000); err != nil {
			return err
		}
	}
	return validateEvidence(r.Evidence)
}

type SuggestRangeRequest struct {
	JobContext
	Ranges   []RangeInput    `json:"ranges"`
	Evidence []EvidenceInput `json:"evidence"`
}

func (r *SuggestRangeRequest) Validate() error {
	if err := r.JobContext.validate(); err != nil {
		return err
	}
	if len(r.Ranges) == 0 {
		return NewValidationError("ranges", "at least one range is required")
	}
	for _, suggestedRange := range r.Ranges {
		if err := requireText("ranges.period_label", suggestedRange.PeriodLabel, 64); err != nil {
			return err
		}
		if suggestedRange.HighValue < suggestedRange.LowValue {
			return NewValidationError("ranges.high_value", "high_value must be greater than or equal to low_value")
		}
	}
	return validateEvidence(r.Evidence)
}

type GenerateNarrativeRequest struct {
	JobContext
	NarrativeText string          `json:"narrative_text"`
	Evidence      []EvidenceInput `json:"evidence"`
}

func (r *GenerateNarrativeRequest) Validate() error {
	if err := r.JobContext.validate(); err != nil {
		return err
	}
	if err := requireText("narrative_text", r.NarrativeText, 20000); err != nil {
		return err
	}
	return validateEvidence(r.Evidence)
}

// SuggestedDriver, SuggestedRange and EvidenceReference are immutable once
// written (enforced by database triggers).
type SuggestedDriver struct {
	DriverID       string    `json:"driver_id"`
	TenantID       string    `json:"tenant_id"`
	JobID          string    `json:"job_id"`
	DriverName     string    `json:"driver_name"`
	SuggestedValue float64   `json:"suggested_value"`
	Rationale      string    `json:"rationale"`
	CreatedAt      time.Time `json:"created_at"`
}

type SuggestedRange struct {
	RangeID        string    `json:"range_id"`
	TenantID       string    `json:"tenant_id"`
	JobID          string    `json:"job_id"`
	PeriodLabel    string    `json:"period_label"`
	LowValue       float64   `json:"low_value"`
	HighValue      float64   `json:"high_value"`
	ConfidenceNote string    `json:"confidence_note"`
	CreatedAt      time.Time `json:"created_at"`
}

type EvidenceReference struct {
	EvidenceID  string    `json:"evidence_id"`
	TenantID    string    `json:"tenant_id"`
	JobID       string    `json:"job_id"`
	SourceRef   string    `json:"source_ref"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// ── Planner decision ────────────────────────────────────────────────────────

type PlannerDecisionType string

const (
	PlannerDecisionAccepted PlannerDecisionType = "Accepted"
	PlannerDecisionRejected PlannerDecisionType = "Rejected"
)

// PlannerDecision is the single, immutable terminal decision for a job.
//
// TargetForecastID / TargetForecastStatus / TargetForecastVersion are a
// provenance snapshot of the forecast the planner intends the suggestion for.
// The forecast itself is never modified by this service's assist layer.
type PlannerDecision struct {
	DecisionID            string              `json:"decision_id"`
	TenantID              string              `json:"tenant_id"`
	JobID                 string              `json:"job_id"`
	Decision              PlannerDecisionType `json:"decision"`
	Reason                string              `json:"reason,omitempty"`
	Note                  string              `json:"note,omitempty"`
	Actor                 string              `json:"actor"`
	TargetForecastID      string              `json:"target_forecast_id,omitempty"`
	TargetForecastStatus  string              `json:"target_forecast_status,omitempty"`
	TargetForecastVersion int                 `json:"target_forecast_version,omitempty"`
	DecidedAt             time.Time           `json:"decided_at"`
}

// AcceptSuggestionRequest is the optional body of :accept.
type AcceptSuggestionRequest struct {
	TargetForecastID string `json:"target_forecast_id,omitempty"`
	Note             string `json:"note,omitempty"`
}

func (r *AcceptSuggestionRequest) Validate() error {
	if r.TargetForecastID != "" && !IsValidUUID(r.TargetForecastID) {
		return NewValidationError("target_forecast_id", "target_forecast_id must be a UUID")
	}
	if len(r.Note) > 4000 {
		return NewValidationError("note", "note must be at most 4000 characters")
	}
	return nil
}

// RejectSuggestionRequest is the body of :reject. A reason is mandatory.
type RejectSuggestionRequest struct {
	Reason string `json:"reason"`
}

func (r *RejectSuggestionRequest) Validate() error {
	return requireText("reason", r.Reason, 4000)
}

// JobDecisionResult is the response of :accept and :reject.
type JobDecisionResult struct {
	Job      *ForecastAssistJob `json:"job"`
	Decision *PlannerDecision   `json:"decision"`
}

type AssistJobListFilter struct {
	Status AssistJobStatus
	Page   PageRequest
}

// ── helpers ─────────────────────────────────────────────────────────────────

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsValidUUID reports whether value is a canonical UUID string.
func IsValidUUID(value string) bool { return uuidPattern.MatchString(value) }

func requireText(field, value string, maxLength int) error {
	if strings.TrimSpace(value) == "" {
		return NewValidationError(field, field+" is required")
	}
	if len(value) > maxLength {
		return NewValidationError(field, field+" is too long")
	}
	return nil
}
