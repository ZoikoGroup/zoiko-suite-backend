package tax

import (
	"errors"
	"time"

	"zoiko.io/contract/types"
)

// TaxType defines standard tax regimes.
type TaxType string

const (
	TaxTypeVAT         TaxType = "VAT"
	TaxTypeGST         TaxType = "GST"
	TaxTypeSalesTax    TaxType = "SALES_TAX"
	TaxTypeIncomeTax   TaxType = "INCOME_TAX"
	TaxTypeWithholding TaxType = "WITHHOLDING"
)

// PackApprovalStatus represents governance lifecycle of a tax rule pack.
type PackApprovalStatus string

const (
	PackStatusDraft     PackApprovalStatus = "DRAFT"
	PackStatusValidated PackApprovalStatus = "VALIDATED"
	PackStatusApproved  PackApprovalStatus = "APPROVED"
	PackStatusRetired   PackApprovalStatus = "RETIRED"
)

// TaxRegistration represents registration of an entity or establishment in a tax jurisdiction (TAX-REG).
type TaxRegistration struct {
	TaxRegistrationID    types.UUID `json:"tax_registration_id"`
	TenantID             types.UUID `json:"tenant_id"`
	LegalEntityID        types.UUID `json:"legal_entity_id"`
	EstablishmentID      *types.UUID `json:"establishment_id,omitempty"`
	JurisdictionID       types.UUID `json:"jurisdiction_id"`
	TaxType              TaxType    `json:"tax_type"`
	RegistrationNumberRef string    `json:"registration_number_ref"` // Vault reference or encrypted value
	MaskedNumber         string     `json:"masked_number,omitempty"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	FilingFrequency      string     `json:"filing_frequency,omitempty"` // "MONTHLY", "QUARTERLY"
	Status               string     `json:"status"` // "ACTIVE", "PENDING", "CANCELLED"
}

// TaxRulePack represents an approved version bundle for one jurisdiction & tax regime (TAX-PACK).
type TaxRulePack struct {
	TaxRulePackID      types.UUID         `json:"tax_rule_pack_id"`
	JurisdictionID     types.UUID         `json:"jurisdiction_id"`
	TaxType            TaxType            `json:"tax_type"`
	PackVersion        string             `json:"pack_version"` // SemVer (e.g. "2026.1.0")
	EffectiveFrom      time.Time          `json:"effective_from"`
	EffectiveTo        *time.Time         `json:"effective_to,omitempty"`
	SourceAuthorityRef string             `json:"source_authority_ref"`
	ApprovalStatus     PackApprovalStatus `json:"approval_status"`
	ContentHash        string             `json:"content_hash"`
	Rules              []TaxRule          `json:"rules,omitempty"`
}

// TaxRule represents an individual executable rate or exemption rule within an approved pack (TAX-RULE).
type TaxRule struct {
	TaxRuleID       types.UUID        `json:"tax_rule_id"`
	TaxRulePackID   types.UUID        `json:"tax_rule_pack_id"`
	RuleCode        string            `json:"rule_code"`
	RuleType        string            `json:"rule_type"` // "STANDARD_RATE", "REDUCED_RATE", "EXEMPTION", "ZERO_RATED"
	Rate            types.RateDecimal `json:"rate"` // NUMERIC(38,18)
	EffectiveFrom   time.Time         `json:"effective_from"`
	EffectiveTo     *time.Time        `json:"effective_to,omitempty"`
}

// IsEffectiveAt checks whether the rule pack was active on the target tax point date.
func (p TaxRulePack) IsEffectiveAt(taxPointDate time.Time) bool {
	t := taxPointDate.UTC()
	if t.Before(p.EffectiveFrom) {
		return false
	}
	if p.EffectiveTo != nil && !t.Before(*p.EffectiveTo) {
		return false
	}
	return true
}

// Validate checks tax rule pack invariants.
func (p TaxRulePack) Validate() error {
	if p.TaxRulePackID.IsNil() || p.JurisdictionID.IsNil() {
		return errors.New("tax_rule_pack requires tax_rule_pack_id and jurisdiction_id")
	}
	if p.PackVersion == "" {
		return errors.New("tax_rule_pack requires pack_version")
	}
	if p.EffectiveFrom.IsZero() {
		return errors.New("tax_rule_pack requires effective_from")
	}
	return nil
}
