package domain

import "time"

// SalaryComponent is one element of pay — an allowance, or a deduction — that a
// legal entity can compose into its compensation structures.
type SalaryComponent struct {
	ComponentID   string        `json:"component_id"`
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	Name          string        `json:"name"`
	Code          string        `json:"code"`
	ComponentType ComponentType `json:"component_type"`
	IsTaxable     bool          `json:"is_taxable"`
	DefaultAmount *float64      `json:"default_amount,omitempty"`
	Currency      string        `json:"currency"`
	Description   *string       `json:"description,omitempty"`
	Status        string        `json:"status"` // ACTIVE, INACTIVE
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	// Monolith-aligned fields
	StatutoryComponent *bool   `json:"statutory_component,omitempty"`
	TaxCode            *string `json:"tax_code,omitempty"`
	PFContributionType *string `json:"pf_contribution_type,omitempty"` // EMPLOYEE, EMPLOYER, BOTH
	ESICoverage        *bool   `json:"esi_coverage,omitempty"`
	PTApplicable       *bool   `json:"pt_applicable,omitempty"`
	IsFlexible         *bool   `json:"is_flexible,omitempty"`
	MinValue           *float64 `json:"min_value,omitempty"`
	MaxValue           *float64 `json:"max_value,omitempty"`
}

// StructureComponent binds a SalaryComponent to a CompensationStructure and
// says how its amount is derived.
type StructureComponent struct {
	StructureComponentID string             `json:"structure_component_id"`
	TenantID             string             `json:"tenant_id"`
	StructureID          string             `json:"structure_id"`
	ComponentID          string             `json:"component_id"`
	CalculationMethod    CalculationMethod  `json:"calculation_method"` // FIXED, PERCENT_OF_BASE
	CalculationValue     float64            `json:"calculation_value"`
	Sequence             int                `json:"sequence"`

	// Denormalised from salary_components for the payslip view. Read-only.
	ComponentName string       `json:"component_name,omitempty"`
	ComponentCode string       `json:"component_code,omitempty"`
	ComponentType ComponentType `json:"component_type,omitempty"`
	IsTaxable     bool         `json:"is_taxable,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateComponentRequest struct {
	LegalEntityID string   `json:"legal_entity_id"`
	Name          string   `json:"name"`
	Code          string   `json:"code"`
	ComponentType ComponentType `json:"component_type"`
	IsTaxable     *bool    `json:"is_taxable,omitempty"` // pointer: omitted means taxable
	DefaultAmount *float64 `json:"default_amount,omitempty"`
	Currency      string   `json:"currency"`
	Description   *string  `json:"description,omitempty"`
	// Monolith-aligned fields
	StatutoryComponent *bool   `json:"statutory_component,omitempty"`
	TaxCode            *string `json:"tax_code,omitempty"`
	PFContributionType *string `json:"pf_contribution_type,omitempty"`
	ESICoverage        *bool   `json:"esi_coverage,omitempty"`
	PTApplicable       *bool   `json:"pt_applicable,omitempty"`
	IsFlexible         *bool   `json:"is_flexible,omitempty"`
	MinValue           *float64 `json:"min_value,omitempty"`
	MaxValue           *float64 `json:"max_value,omitempty"`
}

// SetStructureComponentsRequest replaces a structure's whole composition.
// Replacement rather than incremental edits: a payslip is only explicable if
// the set of components that produced it was written in one go.
type SetStructureComponentsRequest struct {
	Components []StructureComponentInput `json:"components"`
}

type StructureComponentInput struct {
	ComponentID       string             `json:"component_id"`
	CalculationMethod CalculationMethod  `json:"calculation_method"`
	CalculationValue  float64            `json:"calculation_value"`
	Sequence          int                `json:"sequence"`
}

// ── Breakdown ─────────────────────────────────────────────────────────────────

// BreakdownLine is one resolved component with its computed amount.
type BreakdownLine struct {
	ComponentID       string             `json:"component_id"`
	ComponentCode     string             `json:"component_code"`
	ComponentName     string             `json:"component_name"`
	ComponentType     ComponentType      `json:"component_type"`
	IsTaxable         bool               `json:"is_taxable"`
	CalculationMethod CalculationMethod  `json:"calculation_method"`
	CalculationValue  float64            `json:"calculation_value"`
	Amount            float64            `json:"amount"`
	Sequence          int                `json:"sequence"`
}

// CompensationBreakdown is a structure resolved against a base amount.
//
// BaseAmount is the calculation base (typically basic pay). PERCENT_OF_BASE
// components are computed against it, never against each other, so the result
// does not depend on evaluation order and two callers always agree.
//
//	GrossEarnings = BaseAmount + sum(EARNING lines)
//	NetAmount     = GrossEarnings - sum(DEDUCTION lines)
type CompensationBreakdown struct {
	StructureID     string          `json:"structure_id"`
	StructureName   string          `json:"structure_name"`
	Currency        string          `json:"currency"`
	BaseAmount      float64         `json:"base_amount"`
	Lines           []BreakdownLine `json:"lines"`
	TotalEarnings   float64         `json:"total_earnings"`
	TotalDeductions float64         `json:"total_deductions"`
	TaxableAmount   float64         `json:"taxable_amount"`
	GrossEarnings   float64         `json:"gross_earnings"`
	NetAmount       float64         `json:"net_amount"`
}

var (
	ErrComponentNotFound       = errorString("salary component not found")
	ErrComponentCodeExists     = errorString("salary component code already exists for this legal entity")
	ErrInvalidComponentType    = errorString("component_type must be EARNING or DEDUCTION")
	ErrInvalidCalcMethod       = errorString("calculation_method must be FIXED or PERCENT_OF_BASE")
	ErrInvalidCalcValue        = errorString("calculation_value must be non-negative, and at most 100 for PERCENT_OF_BASE")
	ErrDuplicateComponent      = errorString("the same component appears more than once in this structure")
	ErrComponentEntityMismatch = errorString("component belongs to a different legal entity than the structure")
	ErrNegativeBaseAmount      = errorString("base_amount must not be negative")
)