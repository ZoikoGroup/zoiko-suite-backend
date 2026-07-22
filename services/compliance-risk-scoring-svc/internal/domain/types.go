package domain

import (
	"fmt"
	"math"
	"time"
)

type RiskCategory string
type RiskTier string

const (
	CategoryRegulatoryObligations RiskCategory = "REGULATORY_OBLIGATIONS"
	CategoryPolicyViolations      RiskCategory = "POLICY_VIOLATIONS"
	CategoryAuditExceptions       RiskCategory = "AUDIT_EXCEPTIONS"
	CategoryDataPrivacy           RiskCategory = "DATA_PRIVACY"
	CategoryTaxCompliance         RiskCategory = "TAX_COMPLIANCE"
)

const (
	TierLow      RiskTier = "LOW"      // 0 - 25
	TierModerate RiskTier = "MODERATE" // 26 - 50
	TierHigh     RiskTier = "HIGH"     // 51 - 75
	TierCritical RiskTier = "CRITICAL" // 76 - 100
)

type RiskScoreAssessment struct {
	ID                    string                `json:"id"`
	TenantID              string                `json:"tenant_id"`
	LegalEntityID         string                `json:"legal_entity_id"`
	AssessmentName        string                `json:"assessment_name"`
	CompositeRiskScore    float64               `json:"composite_risk_score"` // 0.00 to 100.00
	RiskTier              RiskTier              `json:"risk_tier"`
	OpenObligationsCount  int                   `json:"open_obligations_count"`
	PolicyViolationsCount int                   `json:"policy_violations_count"`
	AuditExceptionsCount  int                   `json:"audit_exceptions_count"`
	PrivacyIncidentsCount int                   `json:"privacy_incidents_count"`
	TaxPenaltiesCount     int                   `json:"tax_penalties_count"`
	Status                string                `json:"status"` // ACTIVE, ARCHIVED
	EvaluatedAt           time.Time             `json:"evaluated_at"`
	CreatedAt             time.Time             `json:"created_at"`
	FactorBreakdowns      []RiskFactorBreakdown `json:"factor_breakdowns,omitempty"`
}

type RiskFactorBreakdown struct {
	ID                string       `json:"id,omitempty"`
	TenantID          string       `json:"tenant_id"`
	AssessmentID      string       `json:"assessment_id"`
	RiskCategory      RiskCategory `json:"risk_category"`
	CategoryWeight    float64      `json:"category_weight"` // Percentage e.g. 0.30
	RawScore          float64      `json:"raw_score"`       // 0.00 to 100.00
	WeightedScore     float64      `json:"weighted_score"`  // RawScore * Weight
	RiskDriverSummary string       `json:"risk_driver_summary"`
	CreatedAt         time.Time    `json:"created_at"`
}

type RiskThresholdRule struct {
	ID                  string       `json:"id"`
	TenantID            string       `json:"tenant_id"`
	RuleName            string       `json:"rule_name"`
	RiskCategory        RiskCategory `json:"risk_category"`
	HighThreshold       float64      `json:"high_threshold"`
	CriticalThreshold   float64      `json:"critical_threshold"`
	NotificationChannel string       `json:"notification_channel"`
	IsActive            bool         `json:"is_active"`
	CreatedAt           time.Time    `json:"created_at"`
}

type CalculateRiskScoreRequest struct {
	LegalEntityID         string `json:"legal_entity_id"`
	AssessmentName        string `json:"assessment_name"`
	OpenObligationsCount  int    `json:"open_obligations_count"`
	PolicyViolationsCount int    `json:"policy_violations_count"`
	AuditExceptionsCount  int    `json:"audit_exceptions_count"`
	PrivacyIncidentsCount int    `json:"privacy_incidents_count"`
	TaxPenaltiesCount     int    `json:"tax_penalties_count"`
}

func (r *CalculateRiskScoreRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.AssessmentName == "" {
		return fmt.Errorf("assessment_name is required")
	}
	return nil
}

// ComputeRiskScore executes standard multi-factor risk weighting algorithms
func ComputeRiskScore(req *CalculateRiskScoreRequest, assessmentID, tenantID string) (float64, RiskTier, []RiskFactorBreakdown) {
	// Category Weights (Sum = 1.00)
	// 1. Regulatory Obligations: 30%
	// 2. Policy Violations: 25%
	// 3. Audit Exceptions: 20%
	// 4. Data Privacy Incidents: 15%
	// 5. Tax Penalties: 10%

	rawObligations := math.Min(100, float64(req.OpenObligationsCount)*12.5) // 8 open = 100%
	rawViolations := math.Min(100, float64(req.PolicyViolationsCount)*20.0) // 5 violations = 100%
	rawAudit := math.Min(100, float64(req.AuditExceptionsCount)*25.0)       // 4 audit exceptions = 100%
	rawPrivacy := math.Min(100, float64(req.PrivacyIncidentsCount)*33.3)    // 3 incidents = 100%
	rawTax := math.Min(100, float64(req.TaxPenaltiesCount)*50.0)            // 2 penalties = 100%

	breakdowns := []RiskFactorBreakdown{
		{
			TenantID:          tenantID,
			AssessmentID:      assessmentID,
			RiskCategory:      CategoryRegulatoryObligations,
			CategoryWeight:    0.30,
			RawScore:          math.Round(rawObligations*100) / 100,
			WeightedScore:     math.Round(rawObligations*0.30*100) / 100,
			RiskDriverSummary: fmt.Sprintf("%d open regulatory obligations pending resolution", req.OpenObligationsCount),
			CreatedAt:         time.Now(),
		},
		{
			TenantID:          tenantID,
			AssessmentID:      assessmentID,
			RiskCategory:      CategoryPolicyViolations,
			CategoryWeight:    0.25,
			RawScore:          math.Round(rawViolations*100) / 100,
			WeightedScore:     math.Round(rawViolations*0.25*100) / 100,
			RiskDriverSummary: fmt.Sprintf("%d active policy non-compliance breaches logged", req.PolicyViolationsCount),
			CreatedAt:         time.Now(),
		},
		{
			TenantID:          tenantID,
			AssessmentID:      assessmentID,
			RiskCategory:      CategoryAuditExceptions,
			CategoryWeight:    0.20,
			RawScore:          math.Round(rawAudit*100) / 100,
			WeightedScore:     math.Round(rawAudit*0.20*100) / 100,
			RiskDriverSummary: fmt.Sprintf("%d unresolved internal/external audit findings", req.AuditExceptionsCount),
			CreatedAt:         time.Now(),
		},
		{
			TenantID:          tenantID,
			AssessmentID:      assessmentID,
			RiskCategory:      CategoryDataPrivacy,
			CategoryWeight:    0.15,
			RawScore:          math.Round(rawPrivacy*100) / 100,
			WeightedScore:     math.Round(rawPrivacy*0.15*100) / 100,
			RiskDriverSummary: fmt.Sprintf("%d data privacy/GDPR compliance alerts", req.PrivacyIncidentsCount),
			CreatedAt:         time.Now(),
		},
		{
			TenantID:          tenantID,
			AssessmentID:      assessmentID,
			RiskCategory:      CategoryTaxCompliance,
			CategoryWeight:    0.10,
			RawScore:          math.Round(rawTax*100) / 100,
			WeightedScore:     math.Round(rawTax*0.10*100) / 100,
			RiskDriverSummary: fmt.Sprintf("%d statutory tax penalties/late filing flags", req.TaxPenaltiesCount),
			CreatedAt:         time.Now(),
		},
	}

	compositeScore := 0.0
	for _, b := range breakdowns {
		compositeScore += b.WeightedScore
	}
	compositeScore = math.Round(compositeScore*100) / 100

	var tier RiskTier
	switch {
	case compositeScore >= 76.0:
		tier = TierCritical
	case compositeScore >= 51.0:
		tier = TierHigh
	case compositeScore >= 26.0:
		tier = TierModerate
	default:
		tier = TierLow
	}

	return compositeScore, tier, breakdowns
}
