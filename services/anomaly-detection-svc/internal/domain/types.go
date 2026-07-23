package domain

import (
	"errors"
	"math"
	"time"
)

var (
	ErrAnomalyRecordNotFound = errors.New("anomaly record not found")
	ErrAnomalyRuleNotFound   = errors.New("anomaly rule not found")
	ErrInvalidStatusTransition = errors.New("invalid status transition")
)

type Severity string

const (
	SeverityLow      Severity = "LOW"
	SeverityMedium   Severity = "MEDIUM"
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

type AnomalyStatus string

const (
	StatusOpen               AnomalyStatus = "OPEN"
	StatusUnderInvestigation AnomalyStatus = "UNDER_INVESTIGATION"
	StatusConfirmedAnomaly   AnomalyStatus = "CONFIRMED_ANOMALY"
	StatusFalsePositive      AnomalyStatus = "FALSE_POSITIVE"
	StatusResolved           AnomalyStatus = "RESOLVED"
)

// AnomalyRule defines configurable threshold/Z-score rules for anomaly detection.
type AnomalyRule struct {
	RuleID         string    `json:"rule_id"`
	TenantID       string    `json:"tenant_id"`
	RuleName       string    `json:"rule_name"`
	DomainName     string    `json:"domain_name"`
	MetricType     string    `json:"metric_type"`
	ThresholdValue float64   `json:"threshold_value"`
	ZScoreCutoff   float64   `json:"z_score_cutoff"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// AnomalyRecord represents a detected variance or anomaly event.
type AnomalyRecord struct {
	AnomalyID       string        `json:"anomaly_id"`
	TenantID        string        `json:"tenant_id"`
	LegalEntityID   string        `json:"legal_entity_id"`
	DomainName      string        `json:"domain_name"`
	SourceEntityID  string        `json:"source_entity_id"`
	RuleID          string        `json:"rule_id,omitempty"`
	Severity        Severity      `json:"severity"`
	AnomalyScore    float64       `json:"anomaly_score"`
	ObservedValue   float64       `json:"observed_value"`
	ExpectedValue   float64       `json:"expected_value"`
	Description     string        `json:"description"`
	Status          AnomalyStatus `json:"status"`
	InvestigatedBy  string        `json:"investigated_by,omitempty"`
	InvestigatedAt  *time.Time    `json:"investigated_at,omitempty"`
	ResolutionNotes string        `json:"resolution_notes,omitempty"`
	DetectedAt      time.Time     `json:"detected_at"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// DetectAnomalyRequest payload to analyze transaction metrics against baseline.
type DetectAnomalyRequest struct {
	LegalEntityID  string  `json:"legal_entity_id"`
	DomainName     string  `json:"domain_name"`
	SourceEntityID string  `json:"source_entity_id"`
	RuleID         string  `json:"rule_id,omitempty"`
	MetricType     string  `json:"metric_type"`
	ObservedValue  float64 `json:"observed_value"`
	ExpectedValue  float64 `json:"expected_value"`
	StdDeviation   float64 `json:"std_deviation,omitempty"`
	Description    string  `json:"description,omitempty"`
}

// UpdateStatusRequest payload to transition anomaly investigation state.
type UpdateStatusRequest struct {
	Status          AnomalyStatus `json:"status"`
	InvestigatedBy  string        `json:"investigated_by"`
	ResolutionNotes string        `json:"resolution_notes,omitempty"`
}

// CreateRuleRequest payload to register a new detection rule.
type CreateRuleRequest struct {
	RuleName       string  `json:"rule_name"`
	DomainName     string  `json:"domain_name"`
	MetricType     string  `json:"metric_type"`
	ThresholdValue float64 `json:"threshold_value"`
	ZScoreCutoff   float64 `json:"z_score_cutoff"`
}

// CalculateAnomalyScore determines Z-score & severity rating from observed vs expected values.
func CalculateAnomalyScore(observed, expected, stdDev float64) (float64, Severity) {
	if stdDev <= 0 {
		stdDev = 1.0
	}
	diff := math.Abs(observed - expected)
	zScore := diff / stdDev

	score := math.Round(zScore*100.0) / 100.0

	var severity Severity
	switch {
	case score >= 4.0:
		severity = SeverityCritical
	case score >= 3.0:
		severity = SeverityHigh
	case score >= 2.0:
		severity = SeverityMedium
	default:
		severity = SeverityLow
	}

	return score, severity
}
