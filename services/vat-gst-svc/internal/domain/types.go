package domain

import (
	"errors"
	"time"
)

var (
	ErrVATReturnNotFound = errors.New("vat return not found")
	ErrAlreadyFiled      = errors.New("vat return is already filed")
)

type FilingStatus string

const (
	StatusDraft    FilingStatus = "DRAFT"
	StatusFiled    FilingStatus = "FILED"
	StatusAccepted FilingStatus = "ACCEPTED"
	StatusRejected FilingStatus = "REJECTED"
)

type VATReturn struct {
	ReturnID              string       `json:"return_id"`
	TenantID              string       `json:"tenant_id"`
	LegalEntityID         string       `json:"legal_entity_id"`
	JurisdictionID        string       `json:"jurisdiction_id"`
	TaxRegistrationNumber string       `json:"tax_registration_number"`
	TaxPeriod             string       `json:"tax_period"` // e.g. "2026-Q1", "2026-03"
	TotalSalesAmount      float64      `json:"total_sales_amount"`
	TotalPurchaseAmount   float64      `json:"total_purchase_amount"`
	OutputTaxAmount       float64      `json:"output_tax_amount"`
	InputTaxAmount        float64      `json:"input_tax_amount"`
	NetTaxPayable         float64      `json:"net_tax_payable"`
	Currency              string       `json:"currency"`
	Status                FilingStatus `json:"status"`
	FiledAt               *time.Time   `json:"filed_at,omitempty"`
	FiledBy               *string      `json:"filed_by,omitempty"`
	EffectiveFrom         string       `json:"effective_from"`
	EffectiveTo           *string      `json:"effective_to,omitempty"`
	CreatedBy             string       `json:"created_by"`
	CreatedAt             time.Time    `json:"created_at"`
	UpdatedAt             time.Time    `json:"updated_at"`
}

type CreateVATReturnRequest struct {
	LegalEntityID         string  `json:"legal_entity_id"`
	JurisdictionID        string  `json:"jurisdiction_id"`
	TaxRegistrationNumber string  `json:"tax_registration_number"`
	TaxPeriod             string  `json:"tax_period"`
	TotalSalesAmount      float64 `json:"total_sales_amount"`
	TotalPurchaseAmount   float64 `json:"total_purchase_amount"`
	OutputTaxAmount       float64 `json:"output_tax_amount"`
	InputTaxAmount        float64 `json:"input_tax_amount"`
	Currency              string  `json:"currency"`
	EffectiveFrom         string  `json:"effective_from"`
	CreatedBy             string  `json:"created_by"`
}

type FileVATReturnRequest struct {
	FiledBy string `json:"filed_by"`
}
