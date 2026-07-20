package domain

import "time"

type Employee struct {
	EmployeeID      string     `json:"employee_id"`
	TenantID        string     `json:"tenant_id"`
	LegalEntityID   string     `json:"legal_entity_id"`
	FirstName       string     `json:"first_name"`
	LastName        string     `json:"last_name"`
	Email           string     `json:"email"`
	WorkerType      string     `json:"worker_type"` // FULL_TIME, PART_TIME, CONTRACTOR
	Status          string     `json:"status"`      // ONBOARDING, ACTIVE, SUSPENDED, TERMINATED
	HireDate        string     `json:"hire_date"`   // YYYY-MM-DD
	TerminationDate *string    `json:"termination_date,omitempty"`
	EffectiveFrom   time.Time  `json:"effective_from"`
	EffectiveTo     *time.Time `json:"effective_to,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type CreateEmployeeRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	FirstName     string `json:"first_name"`
	LastName      string `json:"last_name"`
	Email         string `json:"email"`
	WorkerType    string `json:"worker_type"` // FULL_TIME, PART_TIME, CONTRACTOR
	HireDate      string `json:"hire_date"`   // YYYY-MM-DD
}

type UpdateStatusRequest struct {
	Status          string  `json:"status"` // ACTIVE, SUSPENDED, TERMINATED
	TerminationDate *string `json:"termination_date,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrEmployeeNotFound     = errorString("employee profile not found")
	ErrEmailAlreadyExists   = errorString("employee email already exists in tenant")
	ErrInvalidWorkerStatus  = errorString("invalid worker status transition")
	ErrAuthorizationDenied  = errorString("authorization denied for employee master action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing      = errorString("caller identity missing")
	ErrStoreUnavailable     = errorString("employee master store unavailable")
)