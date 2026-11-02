package domain

import (
	"math"
	"time"
)

// ProjectionInput is everything ComputeProjections needs. Keeping it separate
// from the HTTP request types lets generate, recalculate and reforecast all
// share one deterministic calculation.
type ProjectionInput struct {
	HistoricalData       []float64
	HistoricalStartDate  string // YYYY-MM-DD
	HorizonPeriods       int
	AlgorithmType        AlgorithmType
	ScenarioType         ScenarioType
	GrowthRateAdjustment float64 // e.g. 0.05 lifts every projection by 5%
}

// HistoricalEndDate returns the last day covered by the historical data,
// assuming one data point per month starting at HistoricalStartDate.
func HistoricalEndDate(historicalStartDate string, dataPoints int) string {
	start, err := time.Parse("2006-01-02", historicalStartDate)
	if err != nil {
		return historicalStartDate
	}
	return start.AddDate(0, dataPoints, -1).Format("2006-01-02")
}

// ComputeProjections is a pure, deterministic function: the same input always
// produces the same projections (spec: "reproducible recalculation").
//
// Steps:
//  1. Compute a base value and a per-period slope with the chosen algorithm.
//  2. Apply the scenario multiplier and variance margin.
//  3. Apply the growth adjustment.
//  4. Emit one projection per horizon period with a confidence band.
func ComputeProjections(input ProjectionInput) []ForecastProjection {
	baseValue, slope := computeTrend(input.HistoricalData, input.AlgorithmType)
	scenarioMultiplier, varianceMarginPercent := scenarioParameters(input.ScenarioType)
	growthMultiplier := 1.0 + input.GrowthRateAdjustment
	firstProjectedMonth := firstProjectedPeriodStart(input.HistoricalStartDate, len(input.HistoricalData))

	projections := make([]ForecastProjection, 0, input.HorizonPeriods)
	for period := 1; period <= input.HorizonPeriods; period++ {
		trendValue := baseValue + slope*float64(period)
		projectedAmount := trendValue * scenarioMultiplier * growthMultiplier
		if projectedAmount < 0 {
			projectedAmount = 0 // financial projections are floored at zero
		}

		marginAmount := projectedAmount * (varianceMarginPercent / 100.0)
		confidenceLow := math.Max(0, projectedAmount-marginAmount)
		confidenceHigh := projectedAmount + marginAmount

		periodStart := firstProjectedMonth.AddDate(0, period-1, 0)
		periodEnd := firstProjectedMonth.AddDate(0, period, -1)

		projections = append(projections, ForecastProjection{
			PeriodIndex:     period,
			PeriodStartDate: periodStart.Format("2006-01-02"),
			PeriodEndDate:   periodEnd.Format("2006-01-02"),
			ProjectedAmount: roundToCents(projectedAmount),
			ConfidenceLow:   roundToCents(confidenceLow),
			ConfidenceHigh:  roundToCents(confidenceHigh),
			VarianceMargin:  varianceMarginPercent,
		})
	}
	return projections
}

// computeTrend returns the base value (level at the last observed period) and
// the slope per period for the chosen algorithm.
func computeTrend(data []float64, algorithm AlgorithmType) (baseValue float64, slope float64) {
	count := float64(len(data))
	switch algorithm {
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
		slope = (data[len(data)-1] - data[0]) / count

	case AlgorithmExponentialSmoothing:
		const alpha = 0.3
		smoothed := data[0]
		for i := 1; i < len(data); i++ {
			smoothed = alpha*data[i] + (1-alpha)*smoothed
		}
		baseValue = smoothed
		slope = (data[len(data)-1] - smoothed) / (count / 2.0)

	default: // LINEAR_TREND and SEASONAL_ADJUSTED use an ordinary least-squares line
		var sumX, sumY, sumXY, sumXX float64
		for i, y := range data {
			x := float64(i + 1)
			sumX += x
			sumY += y
			sumXY += x * y
			sumXX += x * x
		}
		denominator := count*sumXX - sumX*sumX
		if denominator != 0 {
			slope = (count*sumXY - sumX*sumY) / denominator
		}
		intercept := (sumY - slope*sumX) / count
		baseValue = intercept + slope*count
	}
	return baseValue, slope
}

// scenarioParameters returns the projection multiplier and the variance margin
// (in percent) for a scenario.
func scenarioParameters(scenario ScenarioType) (multiplier float64, varianceMarginPercent float64) {
	switch scenario {
	case ScenarioOptimistic:
		return 1.15, 7.5
	case ScenarioPessimistic:
		return 0.85, 10.0
	default:
		return 1.0, 5.0
	}
}

// firstProjectedPeriodStart is the month right after the historical data ends.
func firstProjectedPeriodStart(historicalStartDate string, dataPoints int) time.Time {
	start, err := time.Parse("2006-01-02", historicalStartDate)
	if err != nil {
		// Validation guarantees a parseable date; fall back to the epoch so the
		// function stays pure (no time.Now()).
		start = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return start.AddDate(0, dataPoints, 0)
}

func roundToCents(value float64) float64 {
	return math.Round(value*100) / 100
}
