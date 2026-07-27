package domain

import (
	"errors"
	"time"
)

var (
	ErrTaxReturnNotFound = errors.New("corporate tax return not found")
	ErrAlreadySubmitted  = errors.New("corporate tax return is already submitted")
	ErrInvalidTaxYear    = errors.New("invalid tax year")
)

// FilingStatus represents the lifecycle of a corporate tax return.
type FilingStatus string

const (
	StatusDraft     FilingStatus = "DRAFT"
	StatusSubmitted FilingStatus = "SUBMITTED"
	StatusAssessed  FilingStatus = "ASSESSED"
	StatusSettled   FilingStatus = "SETTLED"
	StatusDisputed  FilingStatus = "DISPUTED"
)

// TaxReturn represents a corporate income tax return for a given legal entity and fiscal year.
// All jurisdiction-specific rates and rules are consumed at runtime from Tax Rules Service —
// never hardcoded here (per doctrine.md).
type TaxReturn struct {
	ReturnID                string       `json:"return_id"`
	TenantID                string       `json:"tenant_id"`
	LegalEntityID           string       `json:"legal_entity_id"`
	JurisdictionID          string       `json:"jurisdiction_id"`
	TaxRegistrationNumber   string       `json:"tax_registration_number"`
	FiscalYear              int          `json:"fiscal_year"`
	AccountingPeriodStart   string       `json:"accounting_period_start"`
	AccountingPeriodEnd     string       `json:"accounting_period_end"`
	GrossRevenue            float64      `json:"gross_revenue"`
	AllowableDeductions     float64      `json:"allowable_deductions"`
	TaxableIncome           float64      `json:"taxable_income"`
	TaxRatePercent          float64      `json:"tax_rate_percent"`
	GrossTaxLiability       float64      `json:"gross_tax_liability"`
	TaxCredits              float64      `json:"tax_credits"`
	NetTaxPayable           float64      `json:"net_tax_payable"`
	TaxAlreadyPaid          float64      `json:"tax_already_paid"`
	BalanceDue              float64      `json:"balance_due"`
	Currency                string       `json:"currency"`
	Status                  FilingStatus `json:"status"`
	SubmittedAt             *time.Time   `json:"submitted_at,omitempty"`
	SubmittedBy             *string      `json:"submitted_by,omitempty"`
	AssessedTaxAmount       *float64     `json:"assessed_tax_amount,omitempty"`
	AssessmentReference     *string      `json:"assessment_reference,omitempty"`
	Notes                   string       `json:"notes,omitempty"`
	EffectiveFrom           string       `json:"effective_from"`
	EffectiveTo             *string      `json:"effective_to,omitempty"`
	CreatedBy               string       `json:"created_by"`
	CreatedAt               time.Time    `json:"created_at"`
	UpdatedAt               time.Time    `json:"updated_at"`
}

// CreateTaxReturnRequest is the inbound payload for creating a new draft return.
type CreateTaxReturnRequest struct {
	LegalEntityID         string  `json:"legal_entity_id"`
	JurisdictionID        string  `json:"jurisdiction_id"`
	TaxRegistrationNumber string  `json:"tax_registration_number"`
	FiscalYear            int     `json:"fiscal_year"`
	AccountingPeriodStart string  `json:"accounting_period_start"`
	AccountingPeriodEnd   string  `json:"accounting_period_end"`
	GrossRevenue          float64 `json:"gross_revenue"`
	AllowableDeductions   float64 `json:"allowable_deductions"`
	TaxRatePercent        float64 `json:"tax_rate_percent"`
	TaxCredits            float64 `json:"tax_credits"`
	TaxAlreadyPaid        float64 `json:"tax_already_paid"`
	Currency              string  `json:"currency"`
	Notes                 string  `json:"notes,omitempty"`
	EffectiveFrom         string  `json:"effective_from"`
	CreatedBy             string  `json:"created_by"`
}

// SubmitTaxReturnRequest is the inbound payload to submit a draft to the tax authority.
type SubmitTaxReturnRequest struct {
	SubmittedBy string `json:"submitted_by"`
}

// AssessTaxReturnRequest is the inbound payload to record an authority assessment.
type AssessTaxReturnRequest struct {
	AssessedTaxAmount   float64 `json:"assessed_tax_amount"`
	AssessmentReference string  `json:"assessment_reference"`
	Notes               string  `json:"notes,omitempty"`
}

// Compute derives taxable income and tax amounts from the input fields.
func (r *TaxReturn) Compute() {
	r.TaxableIncome = r.GrossRevenue - r.AllowableDeductions
	if r.TaxableIncome < 0 {
		r.TaxableIncome = 0
	}
	r.GrossTaxLiability = r.TaxableIncome * (r.TaxRatePercent / 100.0)
	r.NetTaxPayable = r.GrossTaxLiability - r.TaxCredits
	if r.NetTaxPayable < 0 {
		r.NetTaxPayable = 0
	}
	r.BalanceDue = r.NetTaxPayable - r.TaxAlreadyPaid
}
