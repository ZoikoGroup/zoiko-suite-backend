package domain

import "time"

type TaxJurisdictionProfile struct {
	ProfileID        string    `json:"profile_id"`
	TenantID         string    `json:"tenant_id"`
	LegalEntityID    string    `json:"legal_entity_id"`
	JurisdictionCode string    `json:"jurisdiction_code"`
	TaxEngineType    string    `json:"tax_engine_type"` // STANDARD_ENGINE, LOCAL_PROVIDER, EXTERNAL_SERVICE, GOVERNMENT_DIRECT
	ProviderEndpoint *string   `json:"provider_endpoint,omitempty"`
	Status           string    `json:"status"` // ACTIVE, INACTIVE
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type TaxComponent struct {
	TaxName   string  `json:"tax_name"`   // e.g. Income Tax, Social Insurance, Medicare
	TaxType   string  `json:"tax_type"`   // STATE, FEDERAL, LOCAL, SOCIAL
	RatePct   float64 `json:"rate_pct"`
	TaxAmount float64 `json:"tax_amount"`
}

type TaxCalculationRecord struct {
	CalculationID         string         `json:"calculation_id"`
	TenantID              string         `json:"tenant_id"`
	PayrollRunID          string         `json:"payroll_run_id"`
	EmployeeID            string         `json:"employee_id"`
	JurisdictionCode      string         `json:"jurisdiction_code"`
	GrossTaxableAmount    float64        `json:"gross_taxable_amount"`
	PreTaxDeductionAmount float64        `json:"pre_tax_deduction_amount"`
	TaxableBasis          float64        `json:"taxable_basis"`
	TotalTaxAmount        float64        `json:"total_tax_amount"`
	TaxBreakdown          []TaxComponent `json:"tax_breakdown"`
	EngineType            string         `json:"engine_type"`
	RuleVersionUsed       string         `json:"rule_version_used"`
	Status                string         `json:"status"` // CALCULATED, AUDITED, ADJUSTED
	CreatedAt             time.Time      `json:"created_at"`
}

type TaxBasisAudit struct {
	AuditID              string    `json:"audit_id"`
	TenantID             string    `json:"tenant_id"`
	CalculationID        string    `json:"calculation_id"`
	EmployeeID           string    `json:"employee_id"`
	RuleBasisJSON        string    `json:"rule_basis_json"`
	ProviderMetadataJSON string    `json:"provider_metadata_json"`
	AuditedAt            time.Time `json:"audited_at"`
}

type CreateProfileRequest struct {
	LegalEntityID    string  `json:"legal_entity_id"`
	JurisdictionCode string  `json:"jurisdiction_code"`
	TaxEngineType    string  `json:"tax_engine_type"`
	ProviderEndpoint *string `json:"provider_endpoint,omitempty"`
}

type CalculateTaxRequest struct {
	PayrollRunID          string  `json:"payroll_run_id"`
	EmployeeID            string  `json:"employee_id"`
	JurisdictionCode      string  `json:"jurisdiction_code"`
	GrossTaxableAmount    float64 `json:"gross_taxable_amount"`
	PreTaxDeductionAmount float64 `json:"pre_tax_deduction_amount"`
	Currency              string  `json:"currency"`
}

type AdjustTaxRequest struct {
	NewTaxBreakdown  []TaxComponent `json:"new_tax_breakdown"`
	NewTotalTaxAmount float64       `json:"new_total_tax_amount"`
	Reason           string         `json:"reason"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrProfileNotFound         = errorString("tax jurisdiction profile not found")
	ErrCalculationNotFound     = errorString("tax calculation record not found")
	ErrAuditNotFound           = errorString("tax basis audit not found")
	ErrEmployeeNotFound        = errorString("employee not found or inactive")
	ErrAuthorizationDenied     = errorString("authorization denied for payroll tax action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("payroll tax store unavailable")
)