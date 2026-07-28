package domain

import (
	"errors"
	"time"
)

var (
	ErrTaxRuleNotFound   = errors.New("tax rule not found")
	ErrDuplicateRuleCode = errors.New("tax rule with code already exists")
)

type TaxCategory string

const (
	CategoryVAT             TaxCategory = "VAT"
	CategoryGST             TaxCategory = "GST"
	CategorySalesTax        TaxCategory = "SALES_TAX"
	CategoryCorporateIncome TaxCategory = "CORPORATE_INCOME"
	CategoryWithholding     TaxCategory = "WITHHOLDING"
	CategoryExcise          TaxCategory = "EXCISE"
	CategoryCustoms         TaxCategory = "CUSTOMS"
)

type RuleStatus string

const (
	StatusDraft      RuleStatus = "DRAFT"
	StatusActive     RuleStatus = "ACTIVE"
	StatusDeprecated RuleStatus = "DEPRECATED"
)

type TaxRule struct {
	RuleID            string      `json:"rule_id"`
	TenantID          string      `json:"tenant_id"`
	JurisdictionID    string      `json:"jurisdiction_id"`
	RuleCode          string      `json:"rule_code"`
	Name              string      `json:"name"`
	Category          TaxCategory `json:"category"`
	TaxRatePercentage float64     `json:"tax_rate_percentage"`
	StandardDeductions float64    `json:"standard_deductions,omitempty"`
	ExemptionsJSON    string      `json:"exemptions_json,omitempty"`
	Status            RuleStatus  `json:"status"`
	Version           int         `json:"version"`
	EffectiveFrom     string      `json:"effective_from"`
	EffectiveTo       *string     `json:"effective_to,omitempty"`
	CreatedBy         string      `json:"created_by"`
	CreatedAt         time.Time   `json:"created_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
}

type CreateTaxRuleRequest struct {
	JurisdictionID    string      `json:"jurisdiction_id"`
	RuleCode          string      `json:"rule_code"`
	Name              string      `json:"name"`
	Category          TaxCategory `json:"category"`
	TaxRatePercentage float64     `json:"tax_rate_percentage"`
	StandardDeductions float64    `json:"standard_deductions,omitempty"`
	ExemptionsJSON    string      `json:"exemptions_json,omitempty"`
	EffectiveFrom     string      `json:"effective_from"`
	EffectiveTo       *string     `json:"effective_to,omitempty"`
	CreatedBy         string      `json:"created_by"`
}

type UpdateTaxRuleRequest struct {
	Name              string      `json:"name,omitempty"`
	Category          TaxCategory `json:"category,omitempty"`
	TaxRatePercentage *float64    `json:"tax_rate_percentage,omitempty"`
	StandardDeductions *float64   `json:"standard_deductions,omitempty"`
	ExemptionsJSON    string      `json:"exemptions_json,omitempty"`
	Status            RuleStatus  `json:"status,omitempty"`
	EffectiveTo       *string     `json:"effective_to,omitempty"`
	UpdatedBy         string      `json:"updated_by"`
}
