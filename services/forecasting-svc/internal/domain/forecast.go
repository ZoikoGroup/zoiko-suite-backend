package domain

import (
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// FIN-02 Forecast (Planning & Forecasting spec §3.2).
//
// A forecast is one versioned projection computed from historical data. Its
// lifecycle follows the spec's state model:
//
//	CALCULATED --submit--> REVIEWED --approve--> APPROVED --publish--> PUBLISHED --(newer reforecast published)--> SUPERSEDED
//	CALCULATED / REVIEWED --archive--> ARCHIVED
//
// Only a CALCULATED forecast may be recalculated. APPROVED and later states are
// immutable: a change requires a reforecast, which creates a new forecast that
// goes through approval again (spec: "Approved budgets and forecasts are
// immutable versions").
// ─────────────────────────────────────────────────────────────────────────────

type ForecastDomain string
type ScenarioType string
type AlgorithmType string
type Granularity string
type ForecastStatus string

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

const (
	ForecastCalculated ForecastStatus = "CALCULATED"
	ForecastReviewed   ForecastStatus = "REVIEWED"
	ForecastApproved   ForecastStatus = "APPROVED"
	ForecastPublished  ForecastStatus = "PUBLISHED"
	ForecastSuperseded ForecastStatus = "SUPERSEDED"
	ForecastArchived   ForecastStatus = "ARCHIVED"
)

var validForecastDomains = map[ForecastDomain]bool{
	DomainFinancial: true, DomainPayroll: true, DomainCashFlow: true, DomainWorkforce: true, DomainTaxLiability: true,
}
var validScenarios = map[ScenarioType]bool{ScenarioBaseline: true, ScenarioOptimistic: true, ScenarioPessimistic: true}
var validAlgorithms = map[AlgorithmType]bool{
	AlgorithmLinearTrend: true, AlgorithmExponentialSmoothing: true, AlgorithmMovingAverage: true, AlgorithmSeasonalAdjusted: true,
}
var validGranularities = map[Granularity]bool{
	GranularityDaily: true, GranularityWeekly: true, GranularityMonthly: true, GranularityQuarterly: true, GranularityAnnual: true,
}
var validForecastStatuses = map[ForecastStatus]bool{
	ForecastCalculated: true, ForecastReviewed: true, ForecastApproved: true,
	ForecastPublished: true, ForecastSuperseded: true, ForecastArchived: true,
}

func (d ForecastDomain) Valid() bool { return validForecastDomains[d] }
func (s ScenarioType) Valid() bool   { return validScenarios[s] }
func (a AlgorithmType) Valid() bool  { return validAlgorithms[a] }
func (g Granularity) Valid() bool    { return validGranularities[g] }
func (s ForecastStatus) Valid() bool { return validForecastStatuses[s] }

// IsImmutable reports whether a forecast in this status may no longer have its
// numbers changed. Approved, published and superseded versions are frozen
// records; archived forecasts are retired.
func (s ForecastStatus) IsImmutable() bool {
	return s == ForecastApproved || s == ForecastPublished || s == ForecastSuperseded || s == ForecastArchived
}

// Limits on forecast inputs. They bound the model runtime (spec NFR: "bounded
// model runtime") and keep a single request from producing an unbounded row set.
const (
	DefaultHorizonPeriods   = 12
	MaxHorizonPeriods       = 120
	MinHistoricalDataPoints = 2
	MaxHistoricalDataPoints = 1000
	MinGrowthRateAdjustment = -0.99
	MaxGrowthRateAdjustment = 10.0
)

// Forecast is one forecast version with its projections.
type Forecast struct {
	ID                   string                 `json:"id"`
	TenantID             string                 `json:"tenant_id"`
	LegalEntityID        string                 `json:"legal_entity_id"`
	ModelName            string                 `json:"model_name"`
	Domain               ForecastDomain         `json:"domain"`
	ScenarioType         ScenarioType           `json:"scenario_type"`
	AlgorithmType        AlgorithmType          `json:"algorithm_type"`
	Granularity          Granularity            `json:"granularity"`
	HorizonPeriods       int                    `json:"horizon_periods"`
	HistoricalData       []float64              `json:"historical_data"`
	HistoricalStartDate  string                 `json:"historical_start_date"`
	HistoricalEndDate    string                 `json:"historical_end_date"`
	GrowthRateAdjustment float64                `json:"growth_rate_adjustment"`
	Status               ForecastStatus         `json:"status"`
	Version              int                    `json:"version"`
	ConfidenceLevel      float64                `json:"confidence_level"`
	PriorForecastID      string                 `json:"prior_forecast_id,omitempty"`
	Metadata             map[string]interface{} `json:"metadata"`
	CreatedBy            string                 `json:"created_by"`
	SubmittedBy          string                 `json:"submitted_by,omitempty"`
	ApprovedBy           string                 `json:"approved_by,omitempty"`
	ApprovedAt           *time.Time             `json:"approved_at,omitempty"`
	PublishedBy          string                 `json:"published_by,omitempty"`
	PublishedAt          *time.Time             `json:"published_at,omitempty"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
	Projections          []ForecastProjection   `json:"projections,omitempty"`
}

// ForecastProjection is one projected period of a forecast.
type ForecastProjection struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id"`
	ForecastID      string    `json:"forecast_model_id"`
	PeriodIndex     int       `json:"period_index"`
	PeriodStartDate string    `json:"period_start_date"`
	PeriodEndDate   string    `json:"period_end_date"`
	ProjectedAmount float64   `json:"projected_amount"`
	ConfidenceLow   float64   `json:"confidence_low"`
	ConfidenceHigh  float64   `json:"confidence_high"`
	VarianceMargin  float64   `json:"variance_margin"`
	CreatedAt       time.Time `json:"created_at"`
}

// GenerateForecastRequest is the body of POST /v1/forecasts/generate.
// LegalEntityID may be omitted: it then defaults to the X-Legal-Entity-Id
// header. When both are present they must agree.
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

// ApplyDefaults fills optional fields with their documented defaults.
func (r *GenerateForecastRequest) ApplyDefaults() {
	if r.HorizonPeriods == 0 {
		r.HorizonPeriods = DefaultHorizonPeriods
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
}

// Validate checks every field after ApplyDefaults has run.
func (r *GenerateForecastRequest) Validate() error {
	if strings.TrimSpace(r.LegalEntityID) == "" {
		return NewValidationError("legal_entity_id", "legal_entity_id is required (body or X-Legal-Entity-Id header)")
	}
	if strings.TrimSpace(r.ModelName) == "" {
		return NewValidationError("model_name", "model_name is required")
	}
	if len(r.ModelName) > 128 {
		return NewValidationError("model_name", "model_name must be at most 128 characters")
	}
	if !r.Domain.Valid() {
		return NewValidationError("domain", "domain must be one of FINANCIAL, PAYROLL, CASH_FLOW, WORKFORCE, TAX_LIABILITY")
	}
	if !r.ScenarioType.Valid() {
		return NewValidationError("scenario_type", "scenario_type must be one of BASELINE, OPTIMISTIC, PESSIMISTIC")
	}
	if !r.AlgorithmType.Valid() {
		return NewValidationError("algorithm_type", "algorithm_type must be one of LINEAR_TREND, EXPONENTIAL_SMOOTHING, MOVING_AVERAGE, SEASONAL_ADJUSTED")
	}
	if !r.Granularity.Valid() {
		return NewValidationError("granularity", "granularity must be one of DAILY, WEEKLY, MONTHLY, QUARTERLY, ANNUAL")
	}
	if r.HorizonPeriods < 1 || r.HorizonPeriods > MaxHorizonPeriods {
		return NewValidationError("horizon_periods", "horizon_periods must be between 1 and 120")
	}
	if len(r.HistoricalData) < MinHistoricalDataPoints {
		return NewValidationError("historical_data", "at least 2 historical data points are required")
	}
	if len(r.HistoricalData) > MaxHistoricalDataPoints {
		return NewValidationError("historical_data", "at most 1000 historical data points are allowed")
	}
	if r.HistoricalStartDate == "" {
		return NewValidationError("historical_start_date", "historical_start_date is required (YYYY-MM-DD)")
	}
	if _, err := time.Parse("2006-01-02", r.HistoricalStartDate); err != nil {
		return NewValidationError("historical_start_date", "historical_start_date must be a date in YYYY-MM-DD format")
	}
	return nil
}

// RecalculateForecastRequest is the body of POST /v1/forecasts/{id}/recalculate.
// Recalculation is reproducible: projections are recomputed from the stored
// historical data, the (optionally changed) scenario and the growth adjustment —
// never by multiplying the previous projections again.
type RecalculateForecastRequest struct {
	GrowthRateAdjustment *float64     `json:"growth_rate_adjustment"`
	ScenarioType         ScenarioType `json:"scenario_type,omitempty"`
}

func (r *RecalculateForecastRequest) Validate() error {
	if r.GrowthRateAdjustment == nil && r.ScenarioType == "" {
		return NewValidationError("growth_rate_adjustment", "provide growth_rate_adjustment and/or scenario_type")
	}
	if r.ScenarioType != "" && !r.ScenarioType.Valid() {
		return NewValidationError("scenario_type", "scenario_type must be one of BASELINE, OPTIMISTIC, PESSIMISTIC")
	}
	if r.GrowthRateAdjustment != nil {
		adjustment := *r.GrowthRateAdjustment
		if adjustment < MinGrowthRateAdjustment || adjustment > MaxGrowthRateAdjustment {
			return NewValidationError("growth_rate_adjustment", "growth_rate_adjustment must be between -0.99 and 10")
		}
	}
	return nil
}

// ReforecastRequest is the body of POST /v1/forecasts/{id}/reforecast. All
// fields are optional overrides of the source forecast's settings.
type ReforecastRequest struct {
	ModelName            string       `json:"model_name,omitempty"`
	ScenarioType         ScenarioType `json:"scenario_type,omitempty"`
	GrowthRateAdjustment *float64     `json:"growth_rate_adjustment,omitempty"`
	HistoricalData       []float64    `json:"historical_data,omitempty"`
}

func (r *ReforecastRequest) Validate() error {
	if r.ScenarioType != "" && !r.ScenarioType.Valid() {
		return NewValidationError("scenario_type", "scenario_type must be one of BASELINE, OPTIMISTIC, PESSIMISTIC")
	}
	if r.GrowthRateAdjustment != nil {
		adjustment := *r.GrowthRateAdjustment
		if adjustment < MinGrowthRateAdjustment || adjustment > MaxGrowthRateAdjustment {
			return NewValidationError("growth_rate_adjustment", "growth_rate_adjustment must be between -0.99 and 10")
		}
	}
	if r.HistoricalData != nil && len(r.HistoricalData) < MinHistoricalDataPoints {
		return NewValidationError("historical_data", "at least 2 historical data points are required")
	}
	if len(r.HistoricalData) > MaxHistoricalDataPoints {
		return NewValidationError("historical_data", "at most 1000 historical data points are allowed")
	}
	if len(r.ModelName) > 128 {
		return NewValidationError("model_name", "model_name must be at most 128 characters")
	}
	return nil
}

// ForecastListFilter holds the optional filters of GET /v1/forecasts.
type ForecastListFilter struct {
	LegalEntityID string
	Domain        ForecastDomain
	ScenarioType  ScenarioType
	Status        ForecastStatus
	Page          PageRequest
}
