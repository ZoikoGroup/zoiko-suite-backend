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
	ScenarioBaseline   ScenarioType = "BASELINE"
	ScenarioOptimistic ScenarioType = "OPTIMISTIC"
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
	ID                   string               `json:"id"`
	TenantID             string               `json:"tenant_id"`
	LegalEntityID        string               `json:"legal_entity_id"`
	ModelName            string               `json:"model_name"`
	Domain               ForecastDomain       `json:"domain"`
	ScenarioType         ScenarioType         `json:"scenario_type"`
	AlgorithmType        AlgorithmType        `json:"algorithm_type"`
	Granularity          Granularity          `json:"granularity"`
	HorizonPeriods       int                  `json:"horizon_periods"`
	HistoricalStartDate  string               `json:"historical_start_date"`
	HistoricalEndDate    string               `json:"historical_end_date"`
	Status               string               `json:"status"` // ACTIVE, ARCHIVED
	ConfidenceLevel      float64              `json:"confidence_level"`
	Metadata             map[string]interface{}`json:"metadata,omitempty"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
	Projections          []ForecastProjection `json:"projections,omitempty"`
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
	GrowthRateAdjustment float64 `json:"growth_rate_adjustment"` // e.g. 0.05 for +5%
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
