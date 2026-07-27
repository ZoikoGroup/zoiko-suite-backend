package domain

import "time"

type LeaveType struct {
	LeaveTypeID        string    `json:"leave_type_id"`
	TenantID           string    `json:"tenant_id"`
	LegalEntityID      string    `json:"legal_entity_id"`
	Name               string    `json:"name"`
	Code               string    `json:"code"` // VACATION, SICK_LEAVE, MATERNITY, PATERNITY, BEREAVEMENT, UNPAID
	IsPaid             bool      `json:"is_paid"`
	AccrualRatePerYear float64   `json:"accrual_rate_per_year"`
	MaxBalance         float64   `json:"max_balance"`
	Status             string    `json:"status"` // ACTIVE, INACTIVE
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type LeaveBalance struct {
	BalanceID      string    `json:"balance_id"`
	TenantID       string    `json:"tenant_id"`
	EmployeeID     string    `json:"employee_id"`
	LeaveTypeID    string    `json:"leave_type_id"`
	LeaveTypeName  string    `json:"leave_type_name,omitempty"`
	LeaveTypeCode  string    `json:"leave_type_code,omitempty"`
	AllocatedHours float64   `json:"allocated_hours"`
	UsedHours      float64   `json:"used_hours"`
	PendingHours   float64   `json:"pending_hours"`
	AvailableHours float64   `json:"available_hours"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type LeaveRequest struct {
	RequestID     string     `json:"request_id"`
	TenantID      string     `json:"tenant_id"`
	EmployeeID    string     `json:"employee_id"`
	LeaveTypeID   string     `json:"leave_type_id"`
	LeaveTypeName string     `json:"leave_type_name,omitempty"`
	StartDate     string     `json:"start_date"`
	EndDate       string     `json:"end_date"`
	TotalHours    float64    `json:"total_hours"`
	Reason        *string    `json:"reason,omitempty"`
	Status        string     `json:"status"` // SUBMITTED, APPROVED, REJECTED, CANCELLED
	ReviewerID    *string    `json:"reviewer_id,omitempty"`
	ReviewerNotes *string    `json:"reviewer_notes,omitempty"`
	ReviewedAt    *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type CreateLeaveTypeRequest struct {
	LegalEntityID      string  `json:"legal_entity_id"`
	Name               string  `json:"name"`
	Code               string  `json:"code"`
	IsPaid             bool    `json:"is_paid"`
	AccrualRatePerYear float64 `json:"accrual_rate_per_year"`
	MaxBalance         float64 `json:"max_balance"`
}

type AccrueBalanceRequest struct {
	EmployeeID  string  `json:"employee_id"`
	LeaveTypeID string  `json:"leave_type_id"`
	Hours       float64 `json:"hours"`
}

type SubmitLeaveRequest struct {
	EmployeeID  string  `json:"employee_id"`
	LeaveTypeID string  `json:"leave_type_id"`
	StartDate   string  `json:"start_date"`
	EndDate     string  `json:"end_date"`
	TotalHours  float64 `json:"total_hours"`
	Reason      *string `json:"reason,omitempty"`
}

type ReviewLeaveRequest struct {
	ReviewerNotes string `json:"reviewer_notes"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrLeaveTypeNotFound       = errorString("leave type not found")
	ErrBalanceNotFound         = errorString("leave balance not found")
	ErrRequestNotFound         = errorString("leave request not found")
	ErrInsufficientBalance     = errorString("insufficient leave balance available")
	ErrInvalidStatusTransition = errorString("invalid leave request status transition")
	ErrEmployeeNotFound        = errorString("employee not found or inactive")
	ErrAuthorizationDenied     = errorString("authorization denied for leave action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("leave & absence store unavailable")
)