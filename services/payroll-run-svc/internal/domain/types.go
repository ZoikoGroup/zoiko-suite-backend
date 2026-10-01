package domain

import "time"

type PayrollRunStatus string

const (
	PayrollRunStatusInitiated   PayrollRunStatus = "INITIATED"
	PayrollRunStatusCalculated  PayrollRunStatus = "CALCULATED"
	PayrollRunStatusBlocked     PayrollRunStatus = "BLOCKED"
	PayrollRunStatusCompleted   PayrollRunStatus = "COMPLETED"
	PayrollRunStatusReversed    PayrollRunStatus = "REVERSED"
	PayrollRunStatusOnHold      PayrollRunStatus = "ON_HOLD"
)

type PayrollRun struct {
	RunID                string           `json:"run_id"`
	TenantID             string           `json:"tenant_id"`
	LegalEntityID        string           `json:"legal_entity_id"`
	RunNumber            string           `json:"run_number"`
	PayPeriodStart       string           `json:"pay_period_start"` // YYYY-MM-DD
	PayPeriodEnd         string           `json:"pay_period_end"`   // YYYY-MM-DD
	PayDate              string           `json:"pay_date"`         // YYYY-MM-DD
	Status               PayrollRunStatus `json:"status"`
	IsShadowRun          bool             `json:"is_shadow_run"`
	TotalGrossPay        float64          `json:"total_gross_pay"`
	TotalNetPay          float64          `json:"total_net_pay"`
	TotalTaxDeductions   float64          `json:"total_tax_deductions"`
	TotalOtherDeductions float64          `json:"total_other_deductions"`
	EmployeeCount        int              `json:"employee_count"`
	CorrelationID        string           `json:"correlation_id"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
	FinalizedAt          *time.Time       `json:"finalized_at,omitempty"`

	GovernanceDecisionID *string `json:"governance_decision_id,omitempty"`
	SnapshotHash         *string `json:"snapshot_hash,omitempty"`

	// Monolith-aligned fields
	TotalEmployerContributions *float64 `json:"total_employer_contributions,omitempty"`
	TotalEmployeeContributions *float64 `json:"total_employee_contributions,omitempty"`
	TotalTDS                   *float64 `json:"total_tds,omitempty"`
	TotalPF                    *float64 `json:"total_pf,omitempty"`
	TotalESI                   *float64 `json:"total_esi,omitempty"`
	TotalPT                    *float64 `json:"total_pt,omitempty"`
	BatchID                    *string  `json:"batch_id,omitempty"`
	ProcessedBy                *string  `json:"processed_by,omitempty"`
	ApprovedBy                 *string  `json:"approved_by,omitempty"`
	ApprovedAt                 *time.Time `json:"approved_at,omitempty"`
}

type PaySlip struct {
	SlipID             string  `json:"slip_id"`
	TenantID           string  `json:"tenant_id"`
	RunID              string  `json:"run_id"`
	EmployeeID         string  `json:"employee_id"`
	EmployeeNumber     string  `json:"employee_number"`
	EmployeeName       string  `json:"employee_name"`
	GrossPay           float64 `json:"gross_pay"`
	TaxWithheld        float64 `json:"tax_withheld"`
	BenefitsDeductions float64 `json:"benefits_deductions"`
	NetPay             float64 `json:"net_pay"`
	Currency           string  `json:"currency"`
	EffectiveDate      string  `json:"effective_date"`

	TaxableAmount float64 `json:"taxable_amount"`

	StructureID *string `json:"structure_id,omitempty"`

	Items []PaySlipItem `json:"items,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	// Monolith-aligned fields
	BasicSalary       *float64 `json:"basic_salary,omitempty"`
	HRA               *float64 `json:"hra,omitempty"`
	SpecialAllowance  *float64 `json:"special_allowance,omitempty"`
	Conveyance        *float64 `json:"conveyance,omitempty"`
	MedicalAllowance  *float64 `json:"medical_allowance,omitempty"`
	LTA               *float64 `json:"lta,omitempty"`
	PFEmployee        *float64 `json:"pf_employee,omitempty"`
	PFEmployer        *float64 `json:"pf_employer,omitempty"`
	ESIEmployee       *float64 `json:"esi_employee,omitempty"`
	ESIEmployer       *float64 `json:"esi_employer,omitempty"`
	PT                *float64 `json:"pt,omitempty"`
	TDS               *float64 `json:"tds,omitempty"`
	OtherDeductions   *float64 `json:"other_deductions,omitempty"`
	Arrears           *float64 `json:"arrears,omitempty"`
	Bonus             *float64 `json:"bonus,omitempty"`
	OvertimePay       *float64 `json:"overtime_pay,omitempty"`
	LeaveEncashment   *float64 `json:"leave_encashment,omitempty"`
	Reimbursements    *float64 `json:"reimbursements,omitempty"`
	AdvanceDeduction  *float64 `json:"advance_deduction,omitempty"`
	LoanDeduction     *float64 `json:"loan_deduction,omitempty"`
}

type PaySlipItem struct {
	ItemID            string    `json:"item_id"`
	TenantID          string    `json:"tenant_id"`
	SlipID            string    `json:"slip_id"`
	ComponentID       *string   `json:"component_id,omitempty"`
	ComponentCode     string    `json:"component_code"`
	ComponentName     string    `json:"component_name"`
	ComponentType     string    `json:"component_type"` // EARNING, DEDUCTION
	IsTaxable         bool      `json:"is_taxable"`
	CalculationMethod string    `json:"calculation_method"`
	CalculationValue  float64   `json:"calculation_value"`
	Amount            float64   `json:"amount"`
	Sequence          int       `json:"sequence"`
	CreatedAt         time.Time `json:"created_at"`
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
	ConfirmationNote     string  `json:"confirmation_note,omitempty"`
	GovernanceDecisionID *string `json:"governance_decision_id,omitempty"`
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

	ErrContractLookupFailed    = errorString("failed to verify an active salary contract for one or more employees")
	ErrCompensationLookupFailed = errorString("failed to resolve compensation breakdown: compensation-svc unavailable")
)
