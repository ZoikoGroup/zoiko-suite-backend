package domain

import "time"

type ContractType string

const (
	ContractTypeFullTime   ContractType = "FULL_TIME"
	ContractTypePartTime   ContractType = "PART_TIME"
	ContractTypeFixedTerm  ContractType = "FIXED_TERM"
	ContractTypeExecutive  ContractType = "EXECUTIVE"
	ContractTypeIntern     ContractType = "INTERN"
	ContractTypeProbation  ContractType = "PROBATION"
)

type ContractStatus string

const (
	ContractStatusDraft       ContractStatus = "DRAFT"
	ContractStatusActive      ContractStatus = "ACTIVE"
	ContractStatusSuperseded  ContractStatus = "SUPERSEDED"
	ContractStatusTerminated  ContractStatus = "TERMINATED"
	ContractStatusExpired     ContractStatus = "EXPIRED"
	ContractStatusPending     ContractStatus = "PENDING"
)

type PayFrequency string

const (
	PayFrequencyMonthly    PayFrequency = "MONTHLY"
	PayFrequencyBiweekly   PayFrequency = "BIWEEKLY"
	PayFrequencyWeekly     PayFrequency = "WEEKLY"
	PayFrequencySemimonthly PayFrequency = "SEMIMONTHLY"
)

type EmploymentContract struct {
	ContractID        string         `json:"contract_id"`
	TenantID          string         `json:"tenant_id"`
	LegalEntityID     string         `json:"legal_entity_id"`
	EmployeeID        string         `json:"employee_id"`
	ContractNumber    string         `json:"contract_number"`
	Version           int            `json:"version"`
	ContractType      ContractType   `json:"contract_type"`
	Status            ContractStatus `json:"status"`
	Title             string         `json:"title"`
	BaseSalaryAmount  float64        `json:"base_salary_amount"`
	Currency          string         `json:"currency"`
	PayFrequency      PayFrequency   `json:"pay_frequency"`
	EffectiveFrom     string         `json:"effective_from"`         // YYYY-MM-DD
	EffectiveTo       *string        `json:"effective_to,omitempty"` // YYYY-MM-DD
	DocumentVaultRef  *string        `json:"document_vault_ref,omitempty"`
	CorrelationID     string         `json:"correlation_id"`

	// ── Monolith-aligned compensation fields ──────────────────────────────
	CTC               *float64 `json:"ctc,omitempty"`               // Cost to Company
	BasicSalary       *float64 `json:"basic_salary,omitempty"`      // Basic component
	HRA               *float64 `json:"hra,omitempty"`               // House Rent Allowance
	SpecialAllowance  *float64 `json:"special_allowance,omitempty"` // Special/Other Allowance
	ConveyanceAllowance *float64 `json:"conveyance_allowance,omitempty"`
	MedicalAllowance  *float64 `json:"medical_allowance,omitempty"`
	LTA               *float64 `json:"lta,omitempty"`               // Leave Travel Allowance

	// ── Employment terms ──────────────────────────────────────────────────
	ProbationPeriodDays *int   `json:"probation_period_days,omitempty"`
	NoticePeriodDays    *int   `json:"notice_period_days,omitempty"`
	WorkingHoursPerWeek *float64 `json:"working_hours_per_week,omitempty"`
	ShiftType           *string `json:"shift_type,omitempty"`         // DAY, NIGHT, ROTATIONAL

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ContractAmendment struct {
	AmendmentID     string    `json:"amendment_id"`
	TenantID        string    `json:"tenant_id"`
	ContractID      string    `json:"contract_id"`
	FromVersion     int       `json:"from_version"`
	ToVersion       int       `json:"to_version"`
	AmendmentReason string    `json:"amendment_reason"`
	AmendedBy       string    `json:"amended_by"`
	EffectiveFrom   string    `json:"effective_from"`
	CreatedAt       time.Time `json:"created_at"`
}

type IssueContractRequest struct {
	LegalEntityID        string   `json:"legal_entity_id"`
	EmployeeID           string   `json:"employee_id"`
	ContractNumber       string   `json:"contract_number,omitempty"`
	ContractType         string   `json:"contract_type"`
	Title                string   `json:"title"`
	BaseSalaryAmount     float64  `json:"base_salary_amount"`
	Currency             string   `json:"currency"`
	PayFrequency         string   `json:"pay_frequency"`
	EffectiveFrom        string   `json:"effective_from"`
	EffectiveTo          *string  `json:"effective_to,omitempty"`
	DocumentVaultRef     *string  `json:"document_vault_ref,omitempty"`
	CorrelationID        string   `json:"correlation_id"`

	// Monolith-aligned fields
	CTC                  *float64 `json:"ctc,omitempty"`
	BasicSalary          *float64 `json:"basic_salary,omitempty"`
	HRA                  *float64 `json:"hra,omitempty"`
	SpecialAllowance     *float64 `json:"special_allowance,omitempty"`
	ConveyanceAllowance  *float64 `json:"conveyance_allowance,omitempty"`
	MedicalAllowance     *float64 `json:"medical_allowance,omitempty"`
	LTA                  *float64 `json:"lta,omitempty"`
	ProbationPeriodDays  *int     `json:"probation_period_days,omitempty"`
	NoticePeriodDays     *int     `json:"notice_period_days,omitempty"`
	WorkingHoursPerWeek  *float64 `json:"working_hours_per_week,omitempty"`
	ShiftType            *string  `json:"shift_type,omitempty"`
}

type AmendContractRequest struct {
	Title                *string  `json:"title,omitempty"`
	BaseSalaryAmount     *float64 `json:"base_salary_amount,omitempty"`
	Currency             *string  `json:"currency,omitempty"`
	PayFrequency         *string  `json:"pay_frequency,omitempty"`
	AmendmentReason      string   `json:"amendment_reason"`
	EffectiveFrom        string   `json:"effective_from"`

	// Monolith-aligned fields
	CTC                  *float64 `json:"ctc,omitempty"`
	BasicSalary          *float64 `json:"basic_salary,omitempty"`
	HRA                  *float64 `json:"hra,omitempty"`
	SpecialAllowance     *float64 `json:"special_allowance,omitempty"`
	ConveyanceAllowance  *float64 `json:"conveyance_allowance,omitempty"`
	MedicalAllowance     *float64 `json:"medical_allowance,omitempty"`
	LTA                  *float64 `json:"lta,omitempty"`
	ProbationPeriodDays  *int     `json:"probation_period_days,omitempty"`
	NoticePeriodDays     *int     `json:"notice_period_days,omitempty"`
	WorkingHoursPerWeek  *float64 `json:"working_hours_per_week,omitempty"`
	ShiftType            *string  `json:"shift_type,omitempty"`
}

type TerminateContractRequest struct {
	TerminationDate string `json:"termination_date"` // YYYY-MM-DD
	TerminationType *string `json:"termination_type,omitempty"` // RESIGNATION, INVOLUNTARY, REDUNDANCY, RETIREMENT
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrContractNotFound               = errorString("employment contract not found")
	ErrEmployeeNotFound               = errorString("employee not found or inactive")
	ErrInvalidContractStatus          = errorString("invalid contract status for operation")
	ErrContractAlreadyTerminated      = errorString("contract is already terminated or superseded")
	ErrAuthorizationDenied            = errorString("authorization denied for employment contract action")
	ErrAuthzServiceUnavailable        = errorString("authorization-svc unavailable")
	ErrIdentityMissing                = errorString("caller identity missing")
	ErrStoreUnavailable               = errorString("employment contract store unavailable")
	ErrEmployeeValidationFailed       = errorString("failed to verify employee's legal entity: employee-master-svc unavailable")
	ErrEmployeeLegalEntityMismatch    = errorString("employee's legal entity does not match the contract's legal_entity_id")
	ErrUnknownContractType            = errorString("contract_type must be one of FULL_TIME, PART_TIME, FIXED_TERM, EXECUTIVE, INTERN, PROBATION")
	ErrUnknownPayFrequency            = errorString("pay_frequency must be one of MONTHLY, BIWEEKLY, WEEKLY, SEMIMONTHLY")
	ErrInvalidCurrency                = errorString("currency must be a three-letter uppercase ISO 4217 code")
	ErrInvalidSalary                  = errorString("base_salary_amount must be greater than zero")
	ErrAmendmentPredatesContract      = errorString("amendment effective_from precedes the contract's effective_from")
	ErrContractNumberVersionExists    = errorString("a contract with this contract_number and version already exists")
	ErrInvalidTerminationType         = errorString("termination_type must be one of RESIGNATION, INVOLUNTARY, REDUNDANCY, RETIREMENT")
	ErrInvalidShiftType               = errorString("shift_type must be one of DAY, NIGHT, ROTATIONAL")
)
