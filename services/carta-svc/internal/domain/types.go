package domain

import (
	"fmt"
	"math"
	"time"
)

type Decision string
type RiskLevel string

const (
	DecisionAllow     Decision = "ALLOW"
	DecisionStepUpMFA Decision = "STEP_UP_MFA"
	DecisionIsolate   Decision = "ISOLATE"
	DecisionDeny      Decision = "DENY"
)

const (
	RiskLow      RiskLevel = "LOW"
	RiskMedium   RiskLevel = "MEDIUM"
	RiskHigh     RiskLevel = "HIGH"
	RiskCritical RiskLevel = "CRITICAL"
)

type AccessContext struct {
	SubjectID           string `json:"subject_id"`
	SubjectType         string `json:"subject_type"` // USER, SERVICE_ACCOUNT, BOT
	DeviceTrustLevel    int    `json:"device_trust_level"` // 0 to 100
	IPAddress           string `json:"ip_address"`
	IsKnownLocation     bool   `json:"is_known_location"`
	ResourceSensitivity string `json:"resource_sensitivity"` // LOW, MEDIUM, HIGH, RESTRICTED
	ActionRequested     string `json:"action_requested"`
	TimeOfDayHour       int    `json:"time_of_day_hour"`
}

type CartaAssessment struct {
	ID                  string        `json:"id"`
	TenantID            string        `json:"tenant_id"`
	LegalEntityID       string        `json:"legal_entity_id"`
	SubjectID           string        `json:"subject_id"`
	Context             AccessContext `json:"context"`
	TrustScore          float64       `json:"trust_score"`
	RiskLevel           RiskLevel     `json:"risk_level"`
	Decision            Decision      `json:"decision"`
	RiskFactors         []string      `json:"risk_factors"`
	AssessmentTimestamp time.Time     `json:"assessment_timestamp"`
}

type EvaluateRequest struct {
	LegalEntityID string        `json:"legal_entity_id"`
	Context       AccessContext `json:"context"`
}

func (r *EvaluateRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.Context.SubjectID == "" {
		return fmt.Errorf("context.subject_id is required")
	}
	if r.Context.ActionRequested == "" {
		return fmt.Errorf("context.action_requested is required")
	}
	return nil
}

// EvaluateAccess runs CARTA evaluation algorithm
func EvaluateAccess(req *EvaluateRequest, tenantID string) *CartaAssessment {
	ctx := req.Context
	score := 100.0
	var riskFactors []string

	// 1. Device Trust Factor (weight: 30%)
	if ctx.DeviceTrustLevel < 50 {
		deduction := float64(50-ctx.DeviceTrustLevel) * 0.6
		score -= deduction
		riskFactors = append(riskFactors, fmt.Sprintf("Untrusted device (trust level %d)", ctx.DeviceTrustLevel))
	}

	// 2. Location Factor (weight: 25%)
	if !ctx.IsKnownLocation {
		score -= 25.0
		riskFactors = append(riskFactors, "Access request from unknown IP/location")
	}

	// 3. Resource Sensitivity Factor (weight: 25%)
	switch ctx.ResourceSensitivity {
	case "RESTRICTED":
		score -= 20.0
		riskFactors = append(riskFactors, "Access to RESTRICTED data asset")
	case "HIGH":
		score -= 10.0
	}

	// 4. Off-Hours Access (weight: 20%)
	if ctx.TimeOfDayHour < 6 || ctx.TimeOfDayHour > 22 {
		score -= 15.0
		riskFactors = append(riskFactors, "Off-hours access attempt")
	}

	if score < 0 {
		score = 0
	}
	score = math.Round(score*100) / 100

	var riskLevel RiskLevel
	var decision Decision

	switch {
	case score >= 80:
		riskLevel = RiskLow
		decision = DecisionAllow
	case score >= 60:
		riskLevel = RiskMedium
		decision = DecisionStepUpMFA
	case score >= 40:
		riskLevel = RiskHigh
		decision = DecisionIsolate
	default:
		riskLevel = RiskCritical
		decision = DecisionDeny
	}

	return &CartaAssessment{
		TenantID:            tenantID,
		LegalEntityID:       req.LegalEntityID,
		SubjectID:           ctx.SubjectID,
		Context:             ctx,
		TrustScore:          score,
		RiskLevel:           riskLevel,
		Decision:            decision,
		RiskFactors:         riskFactors,
		AssessmentTimestamp: time.Now(),
	}
}
