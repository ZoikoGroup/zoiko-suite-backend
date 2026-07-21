package domain

import "time"

type Employee struct {
	EmployeeID        string     `json:"employee_id"`
	TenantID          string     `json:"tenant_id"`
	LegalEntityID     string     `json:"legal_entity_id"`
	EmployeeNumber    string     `json:"employee_number"`
	FirstName         string     `json:"first_name"`
	LastName          string     `json:"last_name"`
	Email             string     `json:"email"`
	Phone             *string    `json:"phone,omitempty"`
	JobTitle          string     `json:"job_title"`
	DepartmentID      *string    `json:"department_id,omitempty"`
	ManagerEmployeeID *string    `json:"manager_employee_id,omitempty"`
	WorkerType        string     `json:"worker_type"` // FULL_TIME, PART_TIME, CONTRACTOR
	Status            string     `json:"status"`      // ONBOARDING, ACTIVE, SUSPENDED, TERMINATED
	HireDate          string     `json:"hire_date"`   // YYYY-MM-DD
	TerminationDate   *string    `json:"termination_date,omitempty"`
	EffectiveFrom     time.Time  `json:"effective_from"`
	EffectiveTo       *time.Time `json:"effective_to,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type CreateEmployeeRequest struct {
	LegalEntityID     string  `json:"legal_entity_id"`
	EmployeeNumber    string  `json:"employee_number,omitempty"`
	FirstName         string  `json:"first_name"`
	LastName          string  `json:"last_name"`
	Email             string  `json:"email"`
	Phone             *string `json:"phone,omitempty"`
	JobTitle          string  `json:"job_title,omitempty"`
	DepartmentID      *string `json:"department_id,omitempty"`
	ManagerEmployeeID *string `json:"manager_employee_id,omitempty"`
	WorkerType        string  `json:"worker_type"` // FULL_TIME, PART_TIME, CONTRACTOR
	HireDate          string  `json:"hire_date"`   // YYYY-MM-DD
}

type UpdateEmployeeRequest struct {
	FirstName         *string `json:"first_name,omitempty"`
	LastName          *string `json:"last_name,omitempty"`
	Phone             *string `json:"phone,omitempty"`
	JobTitle          *string `json:"job_title,omitempty"`
	DepartmentID      *string `json:"department_id,omitempty"`
	ManagerEmployeeID *string `json:"manager_employee_id,omitempty"`
	WorkerType        *string `json:"worker_type,omitempty"`
}

type UpdateStatusRequest struct {
	Status          string  `json:"status"` // ACTIVE, SUSPENDED, TERMINATED
	TerminationDate *string `json:"termination_date,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrEmployeeNotFound        = errorString("employee profile not found")
	ErrEmailAlreadyExists      = errorString("employee email already exists in tenant")
	ErrEmployeeNumberExists    = errorString("employee number already exists in tenant")
	ErrInvalidWorkerStatus     = errorString("invalid worker status transition")
	ErrAuthorizationDenied     = errorString("authorization denied for employee master action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("employee master store unavailable")
)