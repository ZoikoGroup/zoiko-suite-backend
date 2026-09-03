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

	// ── Personal profile ──────────────────────────────────────────────────
	// Personal data, not employment data: it is deliberately absent from the
	// list projection so a directory listing never spreads date-of-birth or a
	// home address further than the one caller who asked for that employee.
	DateOfBirth       *string `json:"date_of_birth,omitempty"` // YYYY-MM-DD
	Gender            *string `json:"gender,omitempty"`
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
	// Reporting labels that sit alongside the authoritative department_id —
	// org-structure-svc owns the real hierarchy, these are the free-text
	// groupings an HR system needs for reporting and payroll cost splits.
	Company          *string `json:"company,omitempty"`
	BusinessUnit     *string `json:"business_unit,omitempty"`
	Division         *string `json:"division,omitempty"`
	Team             *string `json:"team,omitempty"`
	DesignationID    *string `json:"designation_id,omitempty"`
	ConfirmationDate *string `json:"confirmation_date,omitempty"` // YYYY-MM-DD, end of probation

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
}

type UpdateEmployeeRequest struct {
	FirstName         *string `json:"first_name,omitempty"`
	LastName          *string `json:"last_name,omitempty"`
	Phone             *string `json:"phone,omitempty"`
	JobTitle          *string `json:"job_title,omitempty"`
	DepartmentID      *string `json:"department_id,omitempty"`
	ManagerEmployeeID *string `json:"manager_employee_id,omitempty"`
	WorkerType        *string `json:"worker_type,omitempty"`

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
}

type UpdateStatusRequest struct {
	Status          string  `json:"status"` // ACTIVE, SUSPENDED, TERMINATED
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
