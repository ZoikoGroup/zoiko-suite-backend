package domain

import (
	"errors"
	"time"
)

var (
	ErrObligationNotFound  = errors.New("withholding tax obligation not found")
	ErrAlreadyRemitted     = errors.New("withholding tax obligation is already remitted")
	ErrAlreadyCancelled    = errors.New("withholding tax obligation is already cancelled")
	ErrInvalidPaymentAmount = errors.New("payment amount must be greater than zero")
)

type ObligationStatus string

const (
	StatusDraft             ObligationStatus = "DRAFT"
	StatusCalculated        ObligationStatus = "CALCULATED"
	StatusPendingRemittance ObligationStatus = "PENDING_REMITTANCE"
	StatusRemitted          ObligationStatus = "REMITTED"
	StatusCancelled         ObligationStatus = "CANCELLED"
)

// WithholdingTaxObligation represents a tax obligation withheld at source on payment transactions.
// All jurisdiction rules, tax rates, and treaty exemptions are consumed dynamically at runtime (per doctrine.md).
type WithholdingTaxObligation struct {
	ObligationID            string           `json:"obligation_id"`
	TenantID                string           `json:"tenant_id"`
	LegalEntityID           string           `json:"legal_entity_id"`
	JurisdictionID          string           `json:"jurisdiction_id"`
	CounterpartyID          string           `json:"counterparty_id"`
	PaymentReference        string           `json:"payment_reference"`
	PaymentType             string           `json:"payment_type"`
	GrossPaymentAmount      float64          `json:"gross_payment_amount"`
	TaxableBaseAmount       float64          `json:"taxable_base_amount"`
	WithholdingRatePercent  float64          `json:"withholding_rate_percent"`
	WithheldAmount          float64          `json:"withheld_amount"`
	Currency                string           `json:"currency"`
	TaxRuleID               string           `json:"tax_rule_id,omitempty"`
	TaxTreatyExemption      bool             `json:"tax_treaty_exemption"`
	ExemptionCertificateRef string           `json:"exemption_certificate_ref,omitempty"`
	Status                  ObligationStatus `json:"status"`
	RemittanceReference     *string          `json:"remittance_reference,omitempty"`
	RemittedAt              *time.Time       `json:"remitted_at,omitempty"`
	RemittedBy              *string          `json:"remitted_by,omitempty"`
	Notes                   string           `json:"notes,omitempty"`
	EffectiveFrom           string           `json:"effective_from"`
	EffectiveTo             *string          `json:"effective_to,omitempty"`
	CreatedBy               string           `json:"created_by"`
	CreatedAt               time.Time        `json:"created_at"`
	UpdatedAt               time.Time        `json:"updated_at"`
}

// CalculateWithholdingRequest is the input structure for previewing withholding tax on a payment flow.
type CalculateWithholdingRequest struct {
	LegalEntityID           string  `json:"legal_entity_id"`
	JurisdictionID          string  `json:"jurisdiction_id"`
	CounterpartyID          string  `json:"counterparty_id"`
	PaymentReference        string  `json:"payment_reference"`
	PaymentType             string  `json:"payment_type"`
	GrossPaymentAmount      float64 `json:"gross_payment_amount"`
	TaxableBaseAmount       float64 `json:"taxable_base_amount,omitempty"`
	WithholdingRatePercent  float64 `json:"withholding_rate_percent"`
	Currency                string  `json:"currency"`
	TaxRuleID               string  `json:"tax_rule_id,omitempty"`
	TaxTreatyExemption      bool    `json:"tax_treaty_exemption"`
	ExemptionCertificateRef string  `json:"exemption_certificate_ref,omitempty"`
}

// CalculateWithholdingResponse is the output structure for withholding calculation.
type CalculateWithholdingResponse struct {
	GrossPaymentAmount     float64 `json:"gross_payment_amount"`
	TaxableBaseAmount      float64 `json:"taxable_base_amount"`
	WithholdingRatePercent float64 `json:"withholding_rate_percent"`
	WithheldAmount         float64 `json:"withheld_amount"`
	Currency               string  `json:"currency"`
	TaxTreatyApplied       bool    `json:"tax_treaty_applied"`
}

// CreateObligationRequest is the inbound payload for creating a withholding tax obligation.
type CreateObligationRequest struct {
	LegalEntityID           string  `json:"legal_entity_id"`
	JurisdictionID          string  `json:"jurisdiction_id"`
	CounterpartyID          string  `json:"counterparty_id"`
	PaymentReference        string  `json:"payment_reference"`
	PaymentType             string  `json:"payment_type"`
	GrossPaymentAmount      float64 `json:"gross_payment_amount"`
	TaxableBaseAmount       float64 `json:"taxable_base_amount,omitempty"`
	WithholdingRatePercent  float64 `json:"withholding_rate_percent"`
	Currency                string  `json:"currency"`
	TaxRuleID               string  `json:"tax_rule_id,omitempty"`
	TaxTreatyExemption      bool    `json:"tax_treaty_exemption"`
	ExemptionCertificateRef string  `json:"exemption_certificate_ref,omitempty"`
	Notes                   string  `json:"notes,omitempty"`
	EffectiveFrom           string  `json:"effective_from"`
	CreatedBy               string  `json:"created_by"`
}

// RemitObligationRequest is the inbound payload to record remittance to tax authority.
type RemitObligationRequest struct {
	RemittanceReference string `json:"remittance_reference"`
	RemittedBy          string `json:"remitted_by"`
	Notes               string `json:"notes,omitempty"`
}

// CancelObligationRequest is the inbound payload to cancel an obligation.
type CancelObligationRequest struct {
	Reason      string `json:"reason"`
	CancelledBy string `json:"cancelled_by"`
}

// Compute calculates the taxable base and withheld amount for the obligation.
func (o *WithholdingTaxObligation) Compute() {
	if o.TaxableBaseAmount <= 0 {
		o.TaxableBaseAmount = o.GrossPaymentAmount
	}
	if o.TaxTreatyExemption && o.ExemptionCertificateRef != "" {
		// Treaty exemption applies; rate is zeroed or adjusted by rule context
		o.WithheldAmount = 0
	} else {
		o.WithheldAmount = o.TaxableBaseAmount * (o.WithholdingRatePercent / 100.0)
	}
	if o.WithheldAmount < 0 {
		o.WithheldAmount = 0
	}
}
