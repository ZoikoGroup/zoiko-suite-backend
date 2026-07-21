package domain

import (
	"errors"
	"time"
)

var (
	ErrTaxDeterminationNotFound = errors.New("tax determination not found")
	ErrAlreadyOverridden        = errors.New("tax determination is already overridden")
)

type DeterminationStatus string

const (
	StatusCalculated DeterminationStatus = "CALCULATED"
	StatusApplied    DeterminationStatus = "APPLIED"
	StatusOverridden DeterminationStatus = "OVERRIDDEN"
	StatusReversed   DeterminationStatus = "REVERSED"
)

type TaxDetermination struct {
	DeterminationID     string              `json:"determination_id"`
	TenantID            string              `json:"tenant_id"`
	TransactionID       string              `json:"transaction_id"`
	SourceModule        string              `json:"source_module"` // INVOICE, PAYROLL, PURCHASE_ORDER, AP, AR
	LegalEntityID       string              `json:"legal_entity_id"`
	JurisdictionID      string              `json:"jurisdiction_id"`
	RuleID              string              `json:"rule_id,omitempty"`
	TaxCategory         string              `json:"tax_category"`
	GrossAmount         float64             `json:"gross_amount"`
	TaxableAmount       float64             `json:"taxable_amount"`
	TaxRatePercentage   float64             `json:"tax_rate_percentage"`
	CalculatedTaxAmount float64             `json:"calculated_tax_amount"`
	ExemptAmount        float64             `json:"exempt_amount"`
	Currency            string              `json:"currency"`
	Status              DeterminationStatus `json:"status"`
	EffectiveFrom       string              `json:"effective_from"`
	EffectiveTo         *string             `json:"effective_to,omitempty"`
	EvaluatedAt         time.Time           `json:"evaluated_at"`
	EvaluatedBy         string              `json:"evaluated_by"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
}

type DetermineTaxRequest struct {
	TransactionID  string  `json:"transaction_id"`
	SourceModule   string  `json:"source_module"`
	LegalEntityID  string  `json:"legal_entity_id"`
	JurisdictionID string  `json:"jurisdiction_id"`
	TaxCategory    string  `json:"tax_category"`
	GrossAmount    float64 `json:"gross_amount"`
	ExemptAmount   float64 `json:"exempt_amount,omitempty"`
	Currency       string  `json:"currency"`
	EffectiveFrom  string  `json:"effective_from"`
	EvaluatedBy    string  `json:"evaluated_by"`
}

type OverrideTaxRequest struct {
	OverriddenTaxAmount float64 `json:"overridden_tax_amount"`
	Reason              string  `json:"reason"`
	UpdatedBy           string  `json:"updated_by"`
}
