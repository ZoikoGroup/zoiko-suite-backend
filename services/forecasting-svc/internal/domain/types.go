package domain

import (
	"fmt"
	"math"
	"time"
)

type ForecastDomain string
type ScenarioType string
type AlgorithmType string
type Granularity string

const (
	DomainFinancial    ForecastDomain = "FINANCIAL"
	DomainPayroll      ForecastDomain = "PAYROLL"
	DomainCashFlow     ForecastDomain = "CASH_FLOW"
	DomainWorkforce    ForecastDomain = "WORKFORCE"
	DomainTaxLiability ForecastDomain = "TAX_LIABILITY"
)

const (
	ScenarioBaseline    ScenarioType = "BASELINE"
	ScenarioOptimistic  ScenarioType = "OPTIMISTIC"
	ScenarioPessimistic ScenarioType = "PESSIMISTIC"
)

const (
	AlgorithmLinearTrend          AlgorithmType = "LINEAR_TREND"
	AlgorithmExponentialSmoothing AlgorithmType = "EXPONENTIAL_SMOOTHING"
	AlgorithmMovingAverage        AlgorithmType = "MOVING_AVERAGE"
	AlgorithmSeasonalAdjusted     AlgorithmType = "SEASONAL_ADJUSTED"
)

const (
	GranularityDaily     Granularity = "DAILY"
	GranularityWeekly    Granularity = "WEEKLY"
	GranularityMonthly   Granularity = "MONTHLY"
	GranularityQuarterly Granularity = "QUARTERLY"
	GranularityAnnual    Granularity = "ANNUAL"
)

type ForecastModel struct {
	ID                  string                 `json:"id"`
	TenantID            string                 `json:"tenant_id"`
	LegalEntityID       string                 `json:"legal_entity_id"`
	ModelName           string                 `json:"model_name"`
	Domain              ForecastDomain         `json:"domain"`
	ScenarioType        ScenarioType           `json:"scenario_type"`
	AlgorithmType       AlgorithmType          `json:"algorithm_type"`
	Granularity         Granularity            `json:"granularity"`
	HorizonPeriods      int                    `json:"horizon_periods"`
	HistoricalStartDate string                 `json:"historical_start_date"`
	HistoricalEndDate   string                 `json:"historical_end_date"`
	Status              string                 `json:"status"` // ACTIVE, ARCHIVED
	ConfidenceLevel     float64                `json:"confidence_level"`
	Metadata            map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	UpdatedAt           time.Time              `json:"updated_at"`
	Projections         []ForecastProjection   `json:"projections,omitempty"`
}

type ForecastProjection struct {
	ID              string    `json:"id,omitempty"`
	TenantID        string    `json:"tenant_id"`
	ForecastModelID string    `json:"forecast_model_id"`
	PeriodIndex     int       `json:"period_index"`
	PeriodStartDate string    `json:"period_start_date"`
	PeriodEndDate   string    `json:"period_end_date"`
	ProjectedAmount float64   `json:"projected_amount"`
	ConfidenceLow   float64   `json:"confidence_low"`
	ConfidenceHigh  float64   `json:"confidence_high"`
	VarianceMargin  float64   `json:"variance_margin"`
	CreatedAt       time.Time `json:"created_at"`
}

type GenerateForecastRequest struct {
	LegalEntityID       string                 `json:"legal_entity_id"`
	ModelName           string                 `json:"model_name"`
	Domain              ForecastDomain         `json:"domain"`
	ScenarioType        ScenarioType           `json:"scenario_type"`
	AlgorithmType       AlgorithmType          `json:"algorithm_type"`
	Granularity         Granularity            `json:"granularity"`
	HorizonPeriods      int                    `json:"horizon_periods"`
	HistoricalData      []float64              `json:"historical_data"`
	HistoricalStartDate string                 `json:"historical_start_date"`
	Metadata            map[string]interface{} `json:"metadata,omitempty"`
}

type RecalculateRequest struct {
	GrowthRateAdjustment float64      `json:"growth_rate_adjustment"` // e.g. 0.05 for +5%
	ScenarioType         ScenarioType `json:"scenario_type,omitempty"`
}

func (r *GenerateForecastRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.ModelName == "" {
		return fmt.Errorf("model_name is required")
	}
	if r.Domain == "" {
		return fmt.Errorf("domain is required")
	}
	if r.HorizonPeriods <= 0 {
		r.HorizonPeriods = 12
	}
	if len(r.HistoricalData) < 2 {
		return fmt.Errorf("at least 2 historical data points are required for forecasting")
	}
	if r.ScenarioType == "" {
		r.ScenarioType = ScenarioBaseline
	}
	if r.AlgorithmType == "" {
		r.AlgorithmType = AlgorithmLinearTrend
	}
	if r.Granularity == "" {
		r.Granularity = GranularityMonthly
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// AI-05 governed advisory layer (ZS-SVC-N-001 §4/§13 Wave 8)
//
// This section is additive to the Phase 6 forecasting-engine types
// above and does not alter them. It assists planners with driver,
// range and narrative suggestions while preserving explicit planning
// assumptions — it never owns approved forecast values, accounting
// actuals, or direct scenario promotion. No command in this layer ever
// writes to ForecastModel/ForecastProjection or any accounting table:
// "accepted suggestions create normal FIN candidates, never accounting
// entries" holds structurally, not by convention.
// ─────────────────────────────────────────────────────────────────────────────

const (
	PrefixForecastModelRelease = "fmr_"
	PrefixForecastAssistJob    = "faj_"
	PrefixSuggestedDriver      = "sdr_"
	PrefixSuggestedRange       = "srg_"
	PrefixEvidenceReference    = "evr_"
	PrefixPlannerDecision      = "pld_"
)

type assistErrorString string

func (e assistErrorString) Error() string { return string(e) }

type AssistIdempotentReplayError struct {
	ResourceID string
}

func (e *AssistIdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

type AssistIdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

const AssistSellerScope = "seller"

// ForecastModelRelease is the seller-managed model/provider
// registration for one (domain, provider, version) combination.
// Registering IS the evaluation-passed signal — SuggestDrivers/
// SuggestRange/GenerateNarrative all refuse outright against an
// unregistered combination.
type ForecastModelRelease struct {
	ReleaseID     string    `json:"release_id"`
	TenantID      string    `json:"tenant_id"`
	DomainName    string    `json:"domain_name"`
	ModelProvider string    `json:"model_provider"`
	ModelVersion  string    `json:"model_version"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by"`
}

type RegisterForecastModelReleaseRequest struct {
	DomainName    string `json:"domain_name"`
	ModelProvider string `json:"model_provider"`
	ModelVersion  string `json:"model_version"`
}

func (r RegisterForecastModelReleaseRequest) Validate() error {
	if r.DomainName == "" || r.ModelProvider == "" || r.ModelVersion == "" {
		return fmt.Errorf("domain_name, model_provider and model_version are required")
	}
	return nil
}

// ForecastAssistJobStatus is the job's forward-only lifecycle: Queued
// -> Generated -> PlannerReview -> (Accepted|Rejected), then terminal.
type ForecastAssistJobStatus string

const (
	AssistJobQueued        ForecastAssistJobStatus = "Queued"
	AssistJobGenerated     ForecastAssistJobStatus = "Generated"
	AssistJobPlannerReview ForecastAssistJobStatus = "PlannerReview"
	AssistJobAccepted      ForecastAssistJobStatus = "Accepted"
	AssistJobRejected      ForecastAssistJobStatus = "Rejected"
)

// ForecastAssistJob is one planning-assist session. model_provider/
// model_version are frozen at creation against a registered release;
// narrative is write-once.
type ForecastAssistJob struct {
	JobID           string                  `json:"job_id"`
	TenantID        string                  `json:"tenant_id"`
	DomainName      string                  `json:"domain_name"`
	ModelProvider   string                  `json:"model_provider"`
	ModelVersion    string                  `json:"model_version"`
	PlanVersion     string                  `json:"plan_version"`
	PlanningPurpose string                  `json:"planning_purpose"`
	Narrative       string                  `json:"narrative,omitempty"`
	Status          ForecastAssistJobStatus `json:"status"`
	CreatedAt       time.Time               `json:"created_at"`
	CreatedBy       string                  `json:"created_by"`
}

type DriverInput struct {
	DriverName     string  `json:"driver_name"`
	SuggestedValue float64 `json:"suggested_value"`
	Rationale      string  `json:"rationale"`
}

type EvidenceInput struct {
	SourceRef   string `json:"source_ref"`
	Description string `json:"description"`
}

// SuggestDriversRequest either starts a new job (JobID empty) or
// appends to an existing one (JobID set) — both paths validate the
// model/provider/version against the registered release.
type SuggestDriversRequest struct {
	JobID           string          `json:"job_id,omitempty"`
	DomainName      string          `json:"domain_name"`
	ModelProvider   string          `json:"model_provider"`
	ModelVersion    string          `json:"model_version"`
	PlanVersion     string          `json:"plan_version"`
	PlanningPurpose string          `json:"planning_purpose"`
	Drivers         []DriverInput   `json:"drivers"`
	Evidence        []EvidenceInput `json:"evidence"`
}

func (r SuggestDriversRequest) Validate() error {
	if r.JobID == "" {
		if r.DomainName == "" || r.ModelProvider == "" || r.ModelVersion == "" || r.PlanVersion == "" || r.PlanningPurpose == "" {
			return fmt.Errorf("domain_name, model_provider, model_version, plan_version and planning_purpose are required to start a job")
		}
	}
	if len(r.Drivers) == 0 {
		return fmt.Errorf("at least one driver is required")
	}
	for _, d := range r.Drivers {
		if d.DriverName == "" || d.Rationale == "" {
			return fmt.Errorf("driver_name and rationale are required for every driver")
		}
	}
	return nil
}

type RangeInput struct {
	PeriodLabel    string  `json:"period_label"`
	LowValue       float64 `json:"low_value"`
	HighValue      float64 `json:"high_value"`
	ConfidenceNote string  `json:"confidence_note"`
}

type SuggestRangeRequest struct {
	JobID           string          `json:"job_id,omitempty"`
	DomainName      string          `json:"domain_name"`
	ModelProvider   string          `json:"model_provider"`
	ModelVersion    string          `json:"model_version"`
	PlanVersion     string          `json:"plan_version"`
	PlanningPurpose string          `json:"planning_purpose"`
	Ranges          []RangeInput    `json:"ranges"`
	Evidence        []EvidenceInput `json:"evidence"`
}

func (r SuggestRangeRequest) Validate() error {
	if r.JobID == "" {
		if r.DomainName == "" || r.ModelProvider == "" || r.ModelVersion == "" || r.PlanVersion == "" || r.PlanningPurpose == "" {
			return fmt.Errorf("domain_name, model_provider, model_version, plan_version and planning_purpose are required to start a job")
		}
	}
	if len(r.Ranges) == 0 {
		return fmt.Errorf("at least one range is required")
	}
	for _, rg := range r.Ranges {
		if rg.PeriodLabel == "" {
			return fmt.Errorf("period_label is required for every range")
		}
		if rg.HighValue < rg.LowValue {
			return fmt.Errorf("high_value must be >= low_value")
		}
	}
	return nil
}

type GenerateNarrativeRequest struct {
	JobID           string          `json:"job_id,omitempty"`
	DomainName      string          `json:"domain_name"`
	ModelProvider   string          `json:"model_provider"`
	ModelVersion    string          `json:"model_version"`
	PlanVersion     string          `json:"plan_version"`
	PlanningPurpose string          `json:"planning_purpose"`
	NarrativeText   string          `json:"narrative_text"`
	Evidence        []EvidenceInput `json:"evidence"`
}

func (r GenerateNarrativeRequest) Validate() error {
	if r.JobID == "" {
		if r.DomainName == "" || r.ModelProvider == "" || r.ModelVersion == "" || r.PlanVersion == "" || r.PlanningPurpose == "" {
			return fmt.Errorf("domain_name, model_provider, model_version, plan_version and planning_purpose are required to start a job")
		}
	}
	if r.NarrativeText == "" {
		return fmt.Errorf("narrative_text is required")
	}
	return nil
}

// SuggestedDriver/SuggestedRange/EvidenceReference are immutable once
// written.
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

// PlannerDecision is the append-only, exactly-once terminal decision
// for a job.
type PlannerDecisionType string

const (
	PlannerDecisionAccepted PlannerDecisionType = "Accepted"
	PlannerDecisionRejected PlannerDecisionType = "Rejected"
)

type PlannerDecision struct {
	DecisionID string              `json:"decision_id"`
	TenantID   string              `json:"tenant_id"`
	JobID      string              `json:"job_id"`
	Decision   PlannerDecisionType `json:"decision"`
	Reason     string              `json:"reason,omitempty"`
	Actor      string              `json:"actor"`
	DecidedAt  time.Time           `json:"decided_at"`
}

type RejectForecastSuggestionRequest struct {
	Reason string `json:"reason"`
}

func (r RejectForecastSuggestionRequest) Validate() error {
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

var (
	ErrForecastModelReleaseNotFound = assistErrorString("forecast model/provider/version is not registered — evaluation required before material use")
	ErrForecastAssistJobNotFound    = assistErrorString("forecast assist job not found")
	ErrForecastJobNotGenerated      = assistErrorString("forecast assist job is not in Generated status")
	ErrForecastJobNotInReview       = assistErrorString("forecast assist job is not in PlannerReview status")
	ErrForecastJobNotOpen           = assistErrorString("forecast assist job is not open for new suggestions")
	ErrNarrativeAlreadySet          = assistErrorString("forecast assist job narrative has already been generated")
	ErrAssistIdempotencyKeyReused   = assistErrorString("idempotency key was already used for a different request")
)

// ComputeProjections applies mathematical forecasting algorithms and scenario multipliers
func ComputeProjections(req *GenerateForecastRequest, modelID, tenantID string) []ForecastProjection {
	data := req.HistoricalData
	n := float64(len(data))

	// 1. Calculate Base Trend / Moving Average
	var baseValue float64
	var slope float64

	switch req.AlgorithmType {
	case AlgorithmMovingAverage:
		window := 3
		if len(data) < window {
			window = len(data)
		}
		sum := 0.0
		for i := len(data) - window; i < len(data); i++ {
			sum += data[i]
		}
		baseValue = sum / float64(window)
		slope = (data[len(data)-1] - data[0]) / n

	case AlgorithmExponentialSmoothing:
		alpha := 0.3
		s := data[0]
		for i := 1; i < len(data); i++ {
			s = alpha*data[i] + (1-alpha)*s
		}
		baseValue = s
		slope = (data[len(data)-1] - s) / (n / 2.0)

	default: // Linear Trend
		var sumX, sumY, sumXY, sumXX float64
		for i, y := range data {
			x := float64(i + 1)
			sumX += x
			sumY += y
			sumXY += x * y
			sumXX += x * x
		}
		slope = (n*sumXY - sumX*sumY) / (n*sumXX - sumX*sumX)
		intercept := (sumY - slope*sumX) / n
		baseValue = intercept + slope*n
	}

	// 2. Scenario Multipliers
	scenarioMultiplier := 1.0
	varianceMargin := 5.0
	switch req.ScenarioType {
	case ScenarioOptimistic:
		scenarioMultiplier = 1.15 // +15% projection
		varianceMargin = 7.5
	case ScenarioPessimistic:
		scenarioMultiplier = 0.85 // -15% projection
		varianceMargin = 10.0
	default: // BASELINE
		scenarioMultiplier = 1.0
		varianceMargin = 5.0
	}

	// 3. Generate Multi-period Projections
	var projections []ForecastProjection
	startDate := time.Now()
	if req.HistoricalStartDate != "" {
		if parsed, err := time.Parse("2006-01-02", req.HistoricalStartDate); err == nil {
			startDate = parsed.AddDate(0, len(data), 0)
		}
	}

	for period := 1; period <= req.HorizonPeriods; period++ {
		// Projected value with trend + scenario multiplier
		rawProjected := (baseValue + slope*float64(period)) * scenarioMultiplier
		if rawProjected < 0 {
			rawProjected = 0 // Prevent negative financial projections unless allowed
		}

		// Confidence Interval (+/- variance margin %)
		marginAmount := rawProjected * (varianceMargin / 100.0)
		confLow := math.Max(0, rawProjected-marginAmount)
		confHigh := rawProjected + marginAmount

		pStart := startDate.AddDate(0, period-1, 0).Format("2006-01-02")
		pEnd := startDate.AddDate(0, period, -1).Format("2006-01-02")

		projections = append(projections, ForecastProjection{
			TenantID:        tenantID,
			ForecastModelID: modelID,
			PeriodIndex:     period,
			PeriodStartDate: pStart,
			PeriodEndDate:   pEnd,
			ProjectedAmount: math.Round(rawProjected*100) / 100,
			ConfidenceLow:   math.Round(confLow*100) / 100,
			ConfidenceHigh:  math.Round(confHigh*100) / 100,
			VarianceMargin:  varianceMargin,
			CreatedAt:       time.Now(),
		})
	}

	return projections
}
