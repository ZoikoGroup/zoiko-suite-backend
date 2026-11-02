package domain

import (
	"math"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Rule-engine surface (/v1/anomalies)
//
// The original Phase 6 surface: tenant-scoped z-score rules and anomaly
// records computed from observed vs expected values. It is advisory in the
// same way as the governed layer: an anomaly record is evidence for review
// and nothing in this service acts on it.
// ─────────────────────────────────────────────────────────────────────────────

// AnomalyStatus is the rule-engine record lifecycle.
type AnomalyStatus string

const (
	StatusOpen               AnomalyStatus = "OPEN"
	StatusUnderInvestigation AnomalyStatus = "UNDER_INVESTIGATION"
	StatusConfirmedAnomaly   AnomalyStatus = "CONFIRMED_ANOMALY"
	StatusFalsePositive      AnomalyStatus = "FALSE_POSITIVE"
	StatusResolved           AnomalyStatus = "RESOLVED"
)

// IsValid reports whether s is a known record status.
func (s AnomalyStatus) IsValid() bool {
	_, known := allowedRecordTransitions[s]
	return known
}

// allowedRecordTransitions is the rule-engine record state machine:
//
//	OPEN ──► UNDER_INVESTIGATION ──► CONFIRMED_ANOMALY ──► RESOLVED
//	  │                │
//	  └────────────────┴──────────► FALSE_POSITIVE
//
// FALSE_POSITIVE and RESOLVED are terminal.
var allowedRecordTransitions = map[AnomalyStatus][]AnomalyStatus{
	StatusOpen:               {StatusUnderInvestigation, StatusFalsePositive},
	StatusUnderInvestigation: {StatusConfirmedAnomaly, StatusFalsePositive},
	StatusConfirmedAnomaly:   {StatusResolved},
	StatusFalsePositive:      {},
	StatusResolved:           {},
}

// CanTransitionRecord reports whether a record may move from -> to.
func CanTransitionRecord(from, to AnomalyStatus) bool {
	for _, allowed := range allowedRecordTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsDispositionStatus reports whether reaching s is a review decision (and so
// emits AI.AnomalyDispositioned and is subject to segregation of duties).
func IsDispositionStatus(s AnomalyStatus) bool {
	return s == StatusConfirmedAnomaly || s == StatusFalsePositive || s == StatusResolved
}

// AnomalyRule defines a configurable threshold/z-score rule.
type AnomalyRule struct {
	RuleID         string    `json:"rule_id"`
	TenantID       string    `json:"tenant_id"`
	RuleName       string    `json:"rule_name"`
	DomainName     string    `json:"domain_name"`
	MetricType     string    `json:"metric_type"`
	ThresholdValue float64   `json:"threshold_value"`
	ZScoreCutoff   float64   `json:"z_score_cutoff"`
	IsActive       bool      `json:"is_active"`
	Version        int       `json:"version"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// AnomalyRecord is one detected variance.
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
	AssuranceState  string        `json:"assurance_state"`
	DetectedBy      string        `json:"detected_by"`
	InvestigatedBy  string        `json:"investigated_by,omitempty"`
	InvestigatedAt  *time.Time    `json:"investigated_at,omitempty"`
	ResolutionNotes string        `json:"resolution_notes,omitempty"`
	Version         int           `json:"version"`
	DetectedAt      time.Time     `json:"detected_at"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// maxMetricMagnitude keeps observed/expected values inside the NUMERIC(18,4)
// columns, so an absurd value is a 422 rather than a database overflow (500).
const maxMetricMagnitude = 1e13

// MaxAnomalyScore caps the z-score so a tiny std_deviation cannot overflow the
// NUMERIC(14,2) anomaly_score column. Anything this large is CRITICAL anyway.
const MaxAnomalyScore = 999999999.0

// CreateRuleRequest is the body of POST /v1/anomalies/rules.
type CreateRuleRequest struct {
	BodyScope
	RuleName       string  `json:"rule_name"`
	DomainName     string  `json:"domain_name"`
	MetricType     string  `json:"metric_type"`
	ThresholdValue float64 `json:"threshold_value"`
	ZScoreCutoff   float64 `json:"z_score_cutoff"`
}

// Validate checks required fields and numeric ranges. A zero z_score_cutoff
// means "use the default of 3.00".
func (r *CreateRuleRequest) Validate() error {
	if strings.TrimSpace(r.RuleName) == "" {
		return NewValidationError("rule_name", "rule_name is required")
	}
	if len(r.RuleName) > 128 {
		return NewValidationError("rule_name", "rule_name must be at most 128 characters")
	}
	if strings.TrimSpace(r.DomainName) == "" {
		return NewValidationError("domain_name", "domain_name is required")
	}
	if len(r.DomainName) > 64 {
		return NewValidationError("domain_name", "domain_name must be at most 64 characters")
	}
	if strings.TrimSpace(r.MetricType) == "" {
		return NewValidationError("metric_type", "metric_type is required")
	}
	if len(r.MetricType) > 64 {
		return NewValidationError("metric_type", "metric_type must be at most 64 characters")
	}
	if math.Abs(r.ThresholdValue) > maxMetricMagnitude {
		return NewValidationError("threshold_value", "threshold_value is out of range")
	}
	if r.ZScoreCutoff < 0 || r.ZScoreCutoff > 100 {
		return NewValidationError("z_score_cutoff", "z_score_cutoff must be between 0 and 100 (0 means default 3.00)")
	}
	return nil
}

// DetectAnomalyRequest is the body of POST /v1/anomalies/detect.
type DetectAnomalyRequest struct {
	BodyScope
	LegalEntityID  string  `json:"legal_entity_id,omitempty"`
	DomainName     string  `json:"domain_name"`
	SourceEntityID string  `json:"source_entity_id"`
	RuleID         string  `json:"rule_id,omitempty"`
	MetricType     string  `json:"metric_type"`
	ObservedValue  float64 `json:"observed_value"`
	ExpectedValue  float64 `json:"expected_value"`
	StdDeviation   float64 `json:"std_deviation,omitempty"`
	Description    string  `json:"description,omitempty"`
}

// Validate checks required fields and numeric ranges. legal_entity_id is
// resolved by the handler from X-Legal-Entity-Id (the body may only agree).
func (r *DetectAnomalyRequest) Validate() error {
	if strings.TrimSpace(r.DomainName) == "" {
		return NewValidationError("domain_name", "domain_name is required")
	}
	if len(r.DomainName) > 64 {
		return NewValidationError("domain_name", "domain_name must be at most 64 characters")
	}
	if strings.TrimSpace(r.SourceEntityID) == "" {
		return NewValidationError("source_entity_id", "source_entity_id is required")
	}
	if len(r.SourceEntityID) > 128 {
		return NewValidationError("source_entity_id", "source_entity_id must be at most 128 characters")
	}
	if r.RuleID != "" {
		if err := ValidateID(PrefixAnomalyRule, r.RuleID); err != nil {
			return NewValidationError("rule_id", "rule_id must be arule-<uuid>")
		}
	}
	if math.Abs(r.ObservedValue) > maxMetricMagnitude {
		return NewValidationError("observed_value", "observed_value is out of range")
	}
	if math.Abs(r.ExpectedValue) > maxMetricMagnitude {
		return NewValidationError("expected_value", "expected_value is out of range")
	}
	if r.StdDeviation < 0 {
		return NewValidationError("std_deviation", "std_deviation cannot be negative")
	}
	return nil
}

// UpdateStatusRequest is the body of POST /v1/anomalies/{id}/status.
type UpdateStatusRequest struct {
	BodyScope
	Status AnomalyStatus `json:"status"`
	// InvestigatedBy is optional and only accepted when it equals the
	// authenticated principal; the recorded investigator is always the
	// X-Principal-Id.
	InvestigatedBy  string `json:"investigated_by,omitempty"`
	ResolutionNotes string `json:"resolution_notes,omitempty"`
}

// Validate checks the target status is known and that a closing status
// carries resolution notes (the reason for the disposition).
func (r *UpdateStatusRequest) Validate() error {
	if r.Status == "" {
		return NewValidationError("status", "status is required")
	}
	if !r.Status.IsValid() {
		return NewValidationError("status", "status must be one of OPEN, UNDER_INVESTIGATION, CONFIRMED_ANOMALY, FALSE_POSITIVE, RESOLVED")
	}
	needsNotes := r.Status == StatusFalsePositive || r.Status == StatusResolved || r.Status == StatusConfirmedAnomaly
	if needsNotes && strings.TrimSpace(r.ResolutionNotes) == "" {
		return NewValidationError("resolution_notes", "resolution_notes are required when dispositioning a record")
	}
	return nil
}

// RuleFilter narrows GET /v1/anomalies/rules.
type RuleFilter struct {
	DomainName string
}

// AnomalyFilter narrows GET /v1/anomalies.
type AnomalyFilter struct {
	LegalEntityID string
	DomainName    string
	Severity      Severity
	Status        AnomalyStatus
}

// CalculateAnomalyScore returns the absolute z-score (rounded to 2dp) and the
// severity bucket for observed vs expected. A missing or zero standard
// deviation is treated as 1.
func CalculateAnomalyScore(observed, expected, stdDev float64) (float64, Severity) {
	if stdDev <= 0 {
		stdDev = 1.0
	}
	difference := math.Abs(observed - expected)
	zScore := difference / stdDev
	if zScore > MaxAnomalyScore {
		zScore = MaxAnomalyScore
	}
	score := math.Round(zScore*100.0) / 100.0

	switch {
	case score >= 4.0:
		return score, SeverityCritical
	case score >= 3.0:
		return score, SeverityHigh
	case score >= 2.0:
		return score, SeverityMedium
	default:
		return score, SeverityLow
	}
}
