package domain

import "time"

type EmploymentContract struct {
	ContractID       string    `json:"contract_id"`
	TenantID         string    `json:"tenant_id"`
	LegalEntityID    string    `json:"legal_entity_id"`
	EmployeeID       string    `json:"employee_id"`
	ContractNumber   string    `json:"contract_number"`
	Version          int       `json:"version"`
	ContractType     string    `json:"contract_type"` // FULL_TIME, PART_TIME, FIXED_TERM, EXECUTIVE
	Status           string    `json:"status"`        // DRAFT, ACTIVE, SUPERSEDED, TERMINATED, EXPIRED
	Title            string    `json:"title"`
	BaseSalaryAmount float64   `json:"base_salary_amount"`
	Currency         string    `json:"currency"`
	PayFrequency     string    `json:"pay_frequency"`          // MONTHLY, BIWEEKLY, WEEKLY
	EffectiveFrom    string    `json:"effective_from"`         // YYYY-MM-DD
	EffectiveTo      *string   `json:"effective_to,omitempty"` // YYYY-MM-DD
	DocumentVaultRef *string   `json:"document_vault_ref,omitempty"`
	CorrelationID    string    `json:"correlation_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
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
	LegalEntityID    string  `json:"legal_entity_id"`
	EmployeeID       string  `json:"employee_id"`
	ContractNumber   string  `json:"contract_number,omitempty"`
	ContractType     string  `json:"contract_type"`
	Title            string  `json:"title"`
	BaseSalaryAmount float64 `json:"base_salary_amount"`
	Currency         string  `json:"currency"`
	PayFrequency     string  `json:"pay_frequency"`
	EffectiveFrom    string  `json:"effective_from"`
	EffectiveTo      *string `json:"effective_to,omitempty"`
	DocumentVaultRef *string `json:"document_vault_ref,omitempty"`
	CorrelationID    string  `json:"correlation_id"`
}

type AmendContractRequest struct {
	Title            *string  `json:"title,omitempty"`
	BaseSalaryAmount *float64 `json:"base_salary_amount,omitempty"`
	Currency         *string  `json:"currency,omitempty"`
	PayFrequency     *string  `json:"pay_frequency,omitempty"`
	AmendmentReason  string   `json:"amendment_reason"`
	EffectiveFrom    string   `json:"effective_from"`
}

type TerminateContractRequest struct {
	TerminationDate string `json:"termination_date"` // YYYY-MM-DD
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrContractNotFound          = errorString("employment contract not found")
	ErrEmployeeNotFound          = errorString("employee not found or inactive")
	ErrInvalidContractStatus     = errorString("invalid contract status for operation")
	ErrContractAlreadyTerminated = errorString("contract is already terminated or superseded")
	ErrAuthorizationDenied       = errorString("authorization denied for employment contract action")
	ErrAuthzServiceUnavailable   = errorString("authorization-svc unavailable")
	ErrIdentityMissing           = errorString("caller identity missing")
	ErrStoreUnavailable          = errorString("employment contract store unavailable")

	// ErrEmployeeValidationFailed means employee-master-svc could not be
	// reached to confirm the employee's real legal entity — fail closed
	// rather than proceeding with a placeholder entity that authorization
	// would evaluate meaninglessly.
	ErrEmployeeValidationFailed = errorString("failed to verify employee's legal entity: employee-master-svc unavailable")

	// ErrEmployeeLegalEntityMismatch means the employee's real legal entity
	// (per employee-master-svc) does not match the legal_entity_id on the
	// contract request — issuing the contract anyway would let a caller
	// scope a contract to the wrong legal entity's authorization boundary.
	ErrEmployeeLegalEntityMismatch = errorString("employee's legal entity does not match the contract's legal_entity_id")

	// The vocabulary and range errors below correspond one-for-one to the CHECK
	// constraints in supabase/migrations/0030. They exist so the handler can
	// refuse a bad request as a 400 rather than letting the constraint refuse
	// it as an unmapped 500.
	ErrUnknownContractType = errorString("contract_type must be one of FULL_TIME, PART_TIME, FIXED_TERM, EXECUTIVE")
	ErrUnknownPayFrequency = errorString("pay_frequency must be one of MONTHLY, BIWEEKLY, WEEKLY")
	ErrInvalidCurrency     = errorString("currency must be a three-letter uppercase ISO 4217 code")
	ErrInvalidSalary       = errorString("base_salary_amount must be greater than zero")

	// ErrAmendmentPredatesContract means the amendment's effective_from falls
	// before the contract it amends began. AmendContract closes the prior
	// version by setting its effective_to to this date, so accepting one would
	// leave a contract that ended before it started.
	ErrAmendmentPredatesContract = errorString("amendment effective_from precedes the contract's effective_from")

	// ErrContractNumberVersionExists means a contract with this number and
	// version already exists in the tenant. contract_number is caller-supplied,
	// so reusing one collides with the existing version history rather than
	// starting a new one.
	ErrContractNumberVersionExists = errorString("a contract with this contract_number and version already exists")
)
