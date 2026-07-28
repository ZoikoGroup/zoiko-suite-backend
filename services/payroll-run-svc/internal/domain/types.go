package domain

import "time"

type PayrollRun struct {
	RunID                string     `json:"run_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	RunNumber            string     `json:"run_number"`
	PayPeriodStart       string     `json:"pay_period_start"` // YYYY-MM-DD
	PayPeriodEnd         string     `json:"pay_period_end"`   // YYYY-MM-DD
	PayDate              string     `json:"pay_date"`         // YYYY-MM-DD
	Status               string     `json:"status"`           // INITIATED, CALCULATED, BLOCKED, COMPLETED
	IsShadowRun          bool       `json:"is_shadow_run"`
	TotalGrossPay        float64    `json:"total_gross_pay"`
	TotalNetPay          float64    `json:"total_net_pay"`
	TotalTaxDeductions   float64    `json:"total_tax_deductions"`
	TotalOtherDeductions float64    `json:"total_other_deductions"`
	EmployeeCount        int        `json:"employee_count"`
	CorrelationID        string     `json:"correlation_id"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	FinalizedAt          *time.Time `json:"finalized_at,omitempty"`
}

type PaySlip struct {
	SlipID             string    `json:"slip_id"`
	TenantID           string    `json:"tenant_id"`
	RunID              string    `json:"run_id"`
	EmployeeID         string    `json:"employee_id"`
	EmployeeNumber     string    `json:"employee_number"`
	EmployeeName       string    `json:"employee_name"`
	GrossPay           float64   `json:"gross_pay"`
	TaxWithheld        float64   `json:"tax_withheld"`
	BenefitsDeductions float64   `json:"benefits_deductions"`
	NetPay             float64   `json:"net_pay"`
	Currency           string    `json:"currency"`
	EffectiveDate      string    `json:"effective_date"`
	CreatedAt          time.Time `json:"created_at"`
}

type ShadowComparison struct {
	ComparisonID      string    `json:"comparison_id"`
	TenantID          string    `json:"tenant_id"`
	RunID             string    `json:"run_id"`
	EmployeeID        string    `json:"employee_id"`
	LegacyGrossPay    float64   `json:"legacy_gross_pay"`
	LegacyNetPay      float64   `json:"legacy_net_pay"`
	LegacyTaxWithheld float64   `json:"legacy_tax_withheld"`
	ZoikoGrossPay     float64   `json:"zoiko_gross_pay"`
	ZoikoNetPay       float64   `json:"zoiko_net_pay"`
	ZoikoTaxWithheld  float64   `json:"zoiko_tax_withheld"`
	GrossVariance     float64   `json:"gross_variance"`
	NetVariance       float64   `json:"net_variance"`
	TaxVariance       float64   `json:"tax_variance"`
	IsEquivalent      bool      `json:"is_equivalent"`
	CreatedAt         time.Time `json:"created_at"`
}

type InitiatePayrollRunRequest struct {
	LegalEntityID  string `json:"legal_entity_id"`
	RunNumber      string `json:"run_number,omitempty"`
	PayPeriodStart string `json:"pay_period_start"`
	PayPeriodEnd   string `json:"pay_period_end"`
	PayDate        string `json:"pay_date"`
	IsShadowRun    bool   `json:"is_shadow_run"`
	CorrelationID  string `json:"correlation_id"`
}

type ShadowInputItem struct {
	EmployeeID        string  `json:"employee_id"`
	LegacyGrossPay    float64 `json:"legacy_gross_pay"`
	LegacyNetPay      float64 `json:"legacy_net_pay"`
	LegacyTaxWithheld float64 `json:"legacy_tax_withheld"`
}

type CalculateRunRequest struct {
	ShadowBaselineItems []ShadowInputItem `json:"shadow_baseline_items,omitempty"`
}

type FinalizeRunRequest struct {
	ConfirmationNote string `json:"confirmation_note,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrPayrollRunNotFound      = errorString("payroll run not found")
	ErrRunAlreadyFinalized     = errorString("payroll run is finalized and immutable")
	ErrRunNotCalculated        = errorString("payroll run must be calculated before finalization")
	ErrRunBlocked              = errorString("payroll run blocked due to calculation anomalies")
	ErrAuthorizationDenied     = errorString("authorization denied for payroll action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("payroll store unavailable")

	// ErrContractLookupFailed means employment-contracts-svc could not
	// confirm a real active salary contract for one or more employees in
	// this run. Calculation must fail closed here rather than falling
	// back to a fabricated baseline salary — computing real pay off a
	// made-up number is worse than refusing to compute it at all.
	ErrContractLookupFailed = errorString("failed to verify an active salary contract for one or more employees")
)
