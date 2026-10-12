package domain

import "time"

type WorkerType string

const (
	WorkerTypeFullTime   WorkerType = "FULL_TIME"
	WorkerTypePartTime   WorkerType = "PART_TIME"
	WorkerTypeContractor WorkerType = "CONTRACTOR"
	WorkerTypeIntern     WorkerType = "INTERN"
	WorkerTypeProbation  WorkerType = "PROBATION"
)

type EmployeeStatus string

const (
	EmployeeStatusOnboarding   EmployeeStatus = "ONBOARDING"
	EmployeeStatusActive       EmployeeStatus = "ACTIVE"
	EmployeeStatusInactive     EmployeeStatus = "INACTIVE"
	EmployeeStatusPending      EmployeeStatus = "PENDING"
	EmployeeStatusOnLeave      EmployeeStatus = "ON_LEAVE"
	EmployeeStatusTerminated   EmployeeStatus = "TERMINATED"
	EmployeeStatusResigned     EmployeeStatus = "RESIGNED"
	EmployeeStatusDeactivated  EmployeeStatus = "DEACTIVATED"
	EmployeeStatusSuspended    EmployeeStatus = "SUSPENDED"
	EmployeeStatusLocked       EmployeeStatus = "LOCKED"
	EmployeeStatusArchived     EmployeeStatus = "ARCHIVED"
	EmployeeStatusPasswordReset EmployeeStatus = "PASSWORD_RESET_REQUIRED"
)

type Gender string

const (
	GenderMale   Gender = "MALE"
	GenderFemale Gender = "FEMALE"
	GenderOther  Gender = "OTHER"
)

type Employee struct {
	EmployeeID        string         `json:"employee_id"`
	TenantID          string         `json:"tenant_id"`
	LegalEntityID     string         `json:"legal_entity_id"`
	EmployeeNumber    string         `json:"employee_number"`
	FirstName         string         `json:"first_name"`
	LastName          string         `json:"last_name"`
	Email             string         `json:"email"`
	Phone             *string        `json:"phone,omitempty"`
	JobTitle          string         `json:"job_title"`
	DepartmentID      *string        `json:"department_id,omitempty"`
	ManagerEmployeeID *string        `json:"manager_employee_id,omitempty"`
	WorkerType        WorkerType     `json:"worker_type"`
	Status            EmployeeStatus `json:"status"`
	HireDate          string         `json:"hire_date"`   // YYYY-MM-DD
	TerminationDate   *string        `json:"termination_date,omitempty"`
	EffectiveFrom     time.Time      `json:"effective_from"`
	EffectiveTo       *time.Time     `json:"effective_to,omitempty"`

	// ── Personal profile ──────────────────────────────────────────────────
	DateOfBirth       *string `json:"date_of_birth,omitempty"` // YYYY-MM-DD
	Gender            Gender  `json:"gender,omitempty"`
	ProfilePictureURL *string `json:"profile_picture_url,omitempty"`
	PersonalEmail     *string `json:"personal_email,omitempty"`
	WorkEmail         *string `json:"work_email,omitempty"`

	// ── Address ───────────────────────────────────────────────────────────
	CurrentAddress   *string `json:"current_address,omitempty"`
	PermanentAddress *string `json:"permanent_address,omitempty"`
	City             *string `json:"city,omitempty"`
	State            *string `json:"state,omitempty"`
	Country          *string `json:"country,omitempty"`
	PostalCode       *string `json:"postal_code,omitempty"`

	// ── Org placement ─────────────────────────────────────────────────────
	Company          *string `json:"company,omitempty"`
	BusinessUnit     *string `json:"business_unit,omitempty"`
	Division         *string `json:"division,omitempty"`
	Team             *string `json:"team,omitempty"`
	DesignationID    *string `json:"designation_id,omitempty"`
	ConfirmationDate *string `json:"confirmation_date,omitempty"` // YYYY-MM-DD, end of probation

	// ── Compensation (for payroll integration) ────────────────────────────
	BasicSalary *string `json:"basic_salary,omitempty"` // numeric string
	CTC         *string `json:"ctc,omitempty"`          // numeric string

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (e *Employee) FullName() string {
	return e.FirstName + " " + e.LastName
}

func (e *Employee) IsActive() bool {
	return e.Status == EmployeeStatusActive
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
	WorkerType        string  `json:"worker_type"` // FULL_TIME, PART_TIME, CONTRACTOR, INTERN, PROBATION
	HireDate          string  `json:"hire_date"`   // YYYY-MM-DD

	DateOfBirth       *string `json:"date_of_birth,omitempty"`
	Gender            *string `json:"gender,omitempty"` // MALE, FEMALE, OTHER
	ProfilePictureURL *string `json:"profile_picture_url,omitempty"`
	PersonalEmail     *string `json:"personal_email,omitempty"`
	WorkEmail         *string `json:"work_email,omitempty"`

	CurrentAddress   *string `json:"current_address,omitempty"`
	PermanentAddress *string `json:"permanent_address,omitempty"`
	City             *string `json:"city,omitempty"`
	State            *string `json:"state,omitempty"`
	Country          *string `json:"country,omitempty"`
	PostalCode       *string `json:"postal_code,omitempty"`

	Company          *string `json:"company,omitempty"`
	BusinessUnit     *string `json:"business_unit,omitempty"`
	Division         *string `json:"division,omitempty"`
	Team             *string `json:"team,omitempty"`
	DesignationID    *string `json:"designation_id,omitempty"`
	ConfirmationDate *string `json:"confirmation_date,omitempty"`

	BasicSalary *string `json:"basic_salary,omitempty"`
	CTC         *string `json:"ctc,omitempty"`
}

type UpdateEmployeeRequest struct {
	FirstName         *string `json:"first_name,omitempty"`
	LastName          *string `json:"last_name,omitempty"`
	Phone             *string `json:"phone,omitempty"`
	JobTitle          *string `json:"job_title,omitempty"`
	DepartmentID      *string `json:"department_id,omitempty"`
	ManagerEmployeeID *string `json:"manager_employee_id,omitempty"`
	WorkerType        *string `json:"worker_type,omitempty"`
	Status            *string `json:"status,omitempty"`

	DateOfBirth       *string `json:"date_of_birth,omitempty"`
	Gender            *string `json:"gender,omitempty"`
	ProfilePictureURL *string `json:"profile_picture_url,omitempty"`
	PersonalEmail     *string `json:"personal_email,omitempty"`
	WorkEmail         *string `json:"work_email,omitempty"`

	CurrentAddress   *string `json:"current_address,omitempty"`
	PermanentAddress *string `json:"permanent_address,omitempty"`
	City             *string `json:"city,omitempty"`
	State            *string `json:"state,omitempty"`
	Country          *string `json:"country,omitempty"`
	PostalCode       *string `json:"postal_code,omitempty"`

	Company          *string `json:"company,omitempty"`
	BusinessUnit     *string `json:"business_unit,omitempty"`
	Division         *string `json:"division,omitempty"`
	Team             *string `json:"team,omitempty"`
	DesignationID    *string `json:"designation_id,omitempty"`
	ConfirmationDate *string `json:"confirmation_date,omitempty"`

	BasicSalary *string `json:"basic_salary,omitempty"`
	CTC         *string `json:"ctc,omitempty"`
}

type UpdateStatusRequest struct {
	Status          string  `json:"status"` // ONBOARDING, ACTIVE, INACTIVE, PENDING, ON_LEAVE, TERMINATED, RESIGNED, DEACTIVATED, SUSPENDED, LOCKED, ARCHIVED, PASSWORD_RESET_REQUIRED
	TerminationDate *string `json:"termination_date,omitempty"`
}

// EmployeeFilter narrows a directory listing. Every field is optional; an empty
// value means "do not filter on this". It exists so ListEmployees does not grow
// a new positional string parameter each time a reporting rollup needs one.
type EmployeeFilter struct {
	LegalEntityID     string
	Status            string
	WorkerType        string
	DepartmentID      string
	ManagerEmployeeID string
	BusinessUnit      string
	Division          string
	DesignationID     string
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrEmployeeNotFound        = errorString("employee profile not found")
	ErrEmailAlreadyExists      = errorString("employee email already exists in tenant")
	ErrEmployeeNumberExists    = errorString("employee number already exists in tenant")
	ErrWorkEmailAlreadyExists  = errorString("employee work email already exists in tenant")
	ErrInvalidWorkerStatus     = errorString("invalid worker status transition")
	ErrInvalidGender           = errorString("invalid gender value")
	ErrInvalidDate             = errorString("date must be formatted YYYY-MM-DD")
	ErrAuthorizationDenied     = errorString("authorization denied for employee master action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("employee master store unavailable")
)
