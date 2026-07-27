package domain

import "time"

type BenefitPlan struct {
	PlanID                      string    `json:"plan_id"`
	TenantID                    string    `json:"tenant_id"`
	LegalEntityID               string    `json:"legal_entity_id"`
	Name                        string    `json:"name"`
	PlanType                    string    `json:"plan_type"` // HEALTH_INSURANCE, DENTAL, VISION, RETIREMENT_401K, HSA, LIFE_INSURANCE
	ProviderName                string    `json:"provider_name"`
	DeductionTaxTreatment      string    `json:"deduction_tax_treatment"` // PRE_TAX, POST_TAX
	EmployerContributionPct    float64   `json:"employer_contribution_pct"`
	EmployeeContributionAmount float64   `json:"employee_contribution_amount"`
	Currency                    string    `json:"currency"`
	Status                      string    `json:"status"` // ACTIVE, INACTIVE
	CreatedAt                   time.Time `json:"created_at"`
	UpdatedAt                   time.Time `json:"updated_at"`
}

type BenefitElection struct {
	ElectionID                 string    `json:"election_id"`
	TenantID                   string    `json:"tenant_id"`
	EmployeeID                 string    `json:"employee_id"`
	PlanID                     string    `json:"plan_id"`
	CoverageLevel              string    `json:"coverage_level"` // EMPLOYEE_ONLY, EMPLOYEE_PLUS_SPOUSE, EMPLOYEE_PLUS_FAMILY
	EmployeeContributionAmount float64   `json:"employee_contribution_amount"`
	EmployerContributionAmount float64   `json:"employer_contribution_amount"`
	EffectiveFrom              string    `json:"effective_from"` // YYYY-MM-DD
	EffectiveTo                *string   `json:"effective_to,omitempty"` // YYYY-MM-DD
	Status                     string    `json:"status"` // ACTIVE, CANCELLED
	CreatedAt                  time.Time `json:"created_at"`
	UpdatedAt                  time.Time `json:"updated_at"`
}

type CreatePlanRequest struct {
	LegalEntityID              string   `json:"legal_entity_id"`
	Name                       string   `json:"name"`
	PlanType                   string   `json:"plan_type"`
	ProviderName               string   `json:"provider_name"`
	DeductionTaxTreatment      string   `json:"deduction_tax_treatment"`
	EmployerContributionPct    *float64 `json:"employer_contribution_pct,omitempty"`
	EmployeeContributionAmount *float64 `json:"employee_contribution_amount,omitempty"`
	Currency                   string   `json:"currency"`
}

type EnrollBenefitRequest struct {
	EmployeeID                 string   `json:"employee_id"`
	PlanID                     string   `json:"plan_id"`
	CoverageLevel              string   `json:"coverage_level"`
	EmployeeContributionAmount *float64 `json:"employee_contribution_amount,omitempty"`
	EffectiveFrom              string   `json:"effective_from"`
}

type UpdateElectionRequest struct {
	CoverageLevel              string   `json:"coverage_level,omitempty"`
	EmployeeContributionAmount *float64 `json:"employee_contribution_amount,omitempty"`
}

type DeductionSummary struct {
	EmployeeID                string  `json:"employee_id"`
	PreTaxDeductionTotal      float64 `json:"pre_tax_deduction_total"`
	PostTaxDeductionTotal     float64 `json:"post_tax_deduction_total"`
	EmployerContributionTotal float64 `json:"employer_contribution_total"`
	Currency                  string  `json:"currency"`
	ElectionsCount            int     `json:"elections_count"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrPlanNotFound            = errorString("benefit plan not found")
	ErrElectionNotFound        = errorString("benefit election not found")
	ErrElectionAlreadyCancelled= errorString("benefit election already cancelled")
	ErrEmployeeNotFound        = errorString("employee not found or inactive")
	ErrAuthorizationDenied     = errorString("authorization denied for benefits action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("benefits store unavailable")
)