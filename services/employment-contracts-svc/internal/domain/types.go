package domain

import "time"

type EmploymentContract struct {
	ContractID       string     `json:"contract_id"`
	TenantID         string     `json:"tenant_id"`
	LegalEntityID    string     `json:"legal_entity_id"`
	EmployeeID       string     `json:"employee_id"`
	ContractNumber   string     `json:"contract_number"`
	Version          int        `json:"version"`
	ContractType     string     `json:"contract_type"` // FULL_TIME, PART_TIME, FIXED_TERM, EXECUTIVE
	Status           string     `json:"status"`        // DRAFT, ACTIVE, SUPERSEDED, TERMINATED, EXPIRED
	Title            string     `json:"title"`
	BaseSalaryAmount float64    `json:"base_salary_amount"`
	Currency         string     `json:"currency"`
	PayFrequency     string     `json:"pay_frequency"` // MONTHLY, BIWEEKLY, WEEKLY
	EffectiveFrom    string     `json:"effective_from"` // YYYY-MM-DD
	EffectiveTo      *string    `json:"effective_to,omitempty"` // YYYY-MM-DD
	DocumentVaultRef *string    `json:"document_vault_ref,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
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
)