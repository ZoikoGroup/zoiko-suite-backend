package domain

import "time"

type LeaveType struct {
	LeaveTypeID        string  `json:"leave_type_id"`
	TenantID           string  `json:"tenant_id"`
	LegalEntityID      string  `json:"legal_entity_id"`
	Name               string  `json:"name"`
	Code               string  `json:"code"` // VACATION, SICK_LEAVE, MATERNITY, PATERNITY, BEREAVEMENT, UNPAID
	IsPaid             bool    `json:"is_paid"`
	AccrualRatePerYear float64 `json:"accrual_rate_per_year"`
	MaxBalance         float64 `json:"max_balance"`
	Status             string  `json:"status"` // ACTIVE, INACTIVE

	// ── Policy ────────────────────────────────────────────────────────────
	// Enforced by the service at submission time, not advisory.
	CarryForwardAllowed  bool    `json:"carry_forward_allowed"`
	CarryForwardMaxHours float64 `json:"carry_forward_max_hours"`
	MinNoticeDays        int     `json:"min_notice_days"`      // 0 permits same-day and retroactive
	MaxConsecutiveDays   int     `json:"max_consecutive_days"` // 0 means unlimited
	RequiresApproval     bool    `json:"requires_approval"`    // false auto-approves on submit

	// Display metadata carried for the caller; the service never reads these.
	ColorHex *string `json:"color_hex,omitempty"`
	Icon     *string `json:"icon,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Holiday is one non-working day for a legal entity. Leave taken across a
// holiday should not consume balance for that day.
type Holiday struct {
	HolidayID     string    `json:"holiday_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	Name          string    `json:"name"`
	Date          string    `json:"date"`         // YYYY-MM-DD
	HolidayType   string    `json:"holiday_type"` // PUBLIC, COMPANY, OPTIONAL
	IsRecurring   bool      `json:"is_recurring"`
	Description   *string   `json:"description,omitempty"`
	Status        string    `json:"status"` // ACTIVE, INACTIVE
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
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
	CorrelationID string     `json:"correlation_id"`
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

	CarryForwardAllowed  bool    `json:"carry_forward_allowed"`
	CarryForwardMaxHours float64 `json:"carry_forward_max_hours"`
	MinNoticeDays        int     `json:"min_notice_days"`
	MaxConsecutiveDays   int     `json:"max_consecutive_days"`
	// RequiresApproval is a pointer so an omitted field defaults to true.
	// A plain bool would silently make every new leave type auto-approving.
	RequiresApproval *bool   `json:"requires_approval,omitempty"`
	ColorHex         *string `json:"color_hex,omitempty"`
	Icon             *string `json:"icon,omitempty"`
}

type CreateHolidayRequest struct {
	LegalEntityID string  `json:"legal_entity_id"`
	Name          string  `json:"name"`
	Date          string  `json:"date"`                   // YYYY-MM-DD
	HolidayType   string  `json:"holiday_type,omitempty"` // defaults to PUBLIC
	IsRecurring   bool    `json:"is_recurring"`
	Description   *string `json:"description,omitempty"`
}

// HolidayFilter narrows a calendar read. From/To are inclusive bounds.
type HolidayFilter struct {
	LegalEntityID   string
	From            string // YYYY-MM-DD, optional
	To              string // YYYY-MM-DD, optional
	IncludeInactive bool
}

type AccrueBalanceRequest struct {
	EmployeeID  string  `json:"employee_id"`
	LeaveTypeID string  `json:"leave_type_id"`
	Hours       float64 `json:"hours"`
}

type SubmitLeaveRequest struct {
	EmployeeID    string  `json:"employee_id"`
	LeaveTypeID   string  `json:"leave_type_id"`
	StartDate     string  `json:"start_date"`
	EndDate       string  `json:"end_date"`
	TotalHours    float64 `json:"total_hours"`
	Reason        *string `json:"reason,omitempty"`
	CorrelationID string  `json:"correlation_id"`
}

type ReviewLeaveRequest struct {
	ReviewerNotes string `json:"reviewer_notes"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrLeaveTypeNotFound       = errorString("leave type not found")
	ErrLeaveTypeCodeExists     = errorString("leave type code already exists for this legal entity")
	ErrHolidayNotFound         = errorString("holiday not found")
	ErrHolidayDateExists       = errorString("an active holiday already exists on that date for this legal entity")
	ErrInvalidHolidayType      = errorString("holiday_type must be PUBLIC, COMPANY, or OPTIONAL")
	ErrInvalidDate             = errorString("date must be formatted YYYY-MM-DD")
	ErrEndBeforeStart          = errorString("end_date must not precede start_date")
	ErrNoticeTooShort          = errorString("leave starts sooner than this leave type allows")
	ErrSpanTooLong             = errorString("leave spans more consecutive days than this leave type allows")
	ErrInvalidPolicy           = errorString("invalid leave type policy")
	ErrBalanceNotFound         = errorString("leave balance not found")
	ErrRequestNotFound         = errorString("leave request not found")
	ErrInsufficientBalance     = errorString("insufficient leave balance available")
	ErrInvalidStatusTransition = errorString("invalid leave request status transition")
	ErrEmployeeNotFound        = errorString("employee not found or inactive")
	ErrAuthorizationDenied     = errorString("authorization denied for leave action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("leave & absence store unavailable")

	// ErrEmployeeValidationFailed means employee-master-svc could not be
	// reached to confirm the employee's real legal entity — fail closed
	// rather than proceeding with a placeholder entity that authorization
	// would evaluate meaninglessly.
	ErrEmployeeValidationFailed = errorString("failed to verify employee's legal entity: employee-master-svc unavailable")
)
