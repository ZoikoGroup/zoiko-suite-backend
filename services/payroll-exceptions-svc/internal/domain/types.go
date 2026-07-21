package domain

import "time"

type PayrollException struct {
	ExceptionID     string     `json:"exception_id"`
	TenantID        string     `json:"tenant_id"`
	PayrollRunID    string     `json:"payroll_run_id"`
	EmployeeID      *string    `json:"employee_id,omitempty"`
	ExceptionCode   string     `json:"exception_code"`
	Severity        string     `json:"severity"` // BLOCKER, CRITICAL, WARNING
	Description     string     `json:"description"`
	DetailsJSON     string     `json:"details_json"`
	Status          string     `json:"status"` // OPEN, IN_REVIEW, RESOLVED, WAIVED
	ResolutionNotes *string    `json:"resolution_notes,omitempty"`
	ResolvedBy      *string    `json:"resolved_by,omitempty"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type RaiseExceptionRequest struct {
	PayrollRunID  string  `json:"payroll_run_id"`
	EmployeeID    *string `json:"employee_id,omitempty"`
	ExceptionCode string  `json:"exception_code"`
	Severity      string  `json:"severity"`
	Description   string  `json:"description"`
	DetailsJSON   string  `json:"details_json,omitempty"`
}

type ResolveExceptionRequest struct {
	ResolutionNotes string `json:"resolution_notes"`
	Status          string `json:"status"` // RESOLVED or WAIVED
}

type ReleaseBlockerSummary struct {
	PayrollRunID    string `json:"payroll_run_id"`
	TotalExceptions int    `json:"total_exceptions"`
	BlockerCount    int    `json:"blocker_count"`
	WarningCount    int    `json:"warning_count"`
	CanRelease      bool   `json:"can_release"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrExceptionNotFound       = errorString("payroll exception not found")
	ErrAlreadyResolved         = errorString("payroll exception already resolved or waived")
	ErrEmployeeNotFound        = errorString("employee not found or inactive")
	ErrAuthorizationDenied     = errorString("authorization denied for exception action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("payroll exceptions store unavailable")
)