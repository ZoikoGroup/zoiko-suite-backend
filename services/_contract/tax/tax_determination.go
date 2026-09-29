package tax

import (
	"errors"
	"time"

	"zoiko.io/contract/types"
)

// DeterminationStatus represents the calculation state of a tax decision.
type DeterminationStatus string

const (
	DeterminationStatusCalculated   DeterminationStatus = "CALCULATED"
	DeterminationStatusExempt       DeterminationStatus = "EXEMPT"
	DeterminationStatusOutOfScope   DeterminationStatus = "OUT_OF_SCOPE"
	DeterminationStatusManualReview DeterminationStatus = "MANUAL_REVIEW"
	DeterminationStatusOverridden   DeterminationStatus = "OVERRIDDEN"
)

// TaxDetermination represents an immutable, reconstructable tax decision for a taxable document or line (TAX-DET).
// In accordance with ZS-DATA-001 Section 14 (Tax rule doctrine):
// "A tax result is not complete unless the platform can reproduce the input facts,
// applicable registration/jurisdiction context, rule-pack version, individual rules, rates and rounding."
type TaxDetermination struct {
	TaxDeterminationID     types.UUID          `json:"tax_determination_id"`
	TenantID               types.UUID          `json:"tenant_id"`
	LegalEntityID          types.UUID          `json:"legal_entity_id"`
	SourceObjectTable      string              `json:"source_object_table"` // e.g. "sales_invoices", "supplier_invoices"
	SourceObjectID         types.UUID          `json:"source_object_id"`
	RulePackVersionID      types.UUID          `json:"rule_pack_version_id"` // Specific version frozen at calculation time
	TaxPointDate           time.Time           `json:"tax_point_date"`
	TaxableBasis           types.MoneyDecimal  `json:"taxable_basis"`
	CurrencyCode           string              `json:"currency_code"`
	ResultStatus           DeterminationStatus `json:"result_status"`
	InputFactsHash         string              `json:"input_facts_hash"` // Cryptographic hash of input parameters
	OverrideRef            *types.UUID         `json:"override_ref,omitempty"`
	CreatedAt              time.Time           `json:"created_at"`
	Components             []TaxComponent      `json:"components,omitempty"`
}

// TaxComponent represents an individual tax amount or rate outcome within a determination (TAX-COMP).
type TaxComponent struct {
	TaxComponentID     types.UUID         `json:"tax_component_id"`
	TaxDeterminationID types.UUID         `json:"tax_determination_id"`
	TaxType            TaxType            `json:"tax_type"`
	JurisdictionID     types.UUID         `json:"jurisdiction_id"`
	Rate               types.RateDecimal  `json:"rate"` // Exact NUMERIC(38,18)
	TaxableAmount      types.MoneyDecimal `json:"taxable_amount"`
	TaxAmount          types.MoneyDecimal `json:"tax_amount"` // Exact rounded result
	TaxCode            string             `json:"tax_code,omitempty"`
	RuleID             types.UUID         `json:"rule_id"` // Lineage back to specific tax rule
}

// Validate checks tax determination requirements.
func (d TaxDetermination) Validate() error {
	if d.TaxDeterminationID.IsNil() || d.TenantID.IsNil() || d.LegalEntityID.IsNil() {
		return errors.New("tax_determination requires non-nil IDs")
	}
	if d.RulePackVersionID.IsNil() {
		return errors.New("tax_determination requires frozen rule_pack_version_id")
	}
	if d.TaxPointDate.IsZero() {
		return errors.New("tax_determination requires tax_point_date")
	}
	if d.InputFactsHash == "" {
		return errors.New("tax_determination requires input_facts_hash for audit reproducibility")
	}
	return nil
}

// TotalTaxAmount calculates the sum of all tax components in the determination.
func (d TaxDetermination) TotalTaxAmount() types.MoneyDecimal {
	total := types.ZeroMoney()
	for _, c := range d.Components {
		total = total.Add(c.TaxAmount)
	}
	return total
}
