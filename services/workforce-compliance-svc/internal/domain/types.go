package domain

import (
	"errors"
	"time"
)

var (
	ErrRecordNotFound       = errors.New("compliance record not found")
	ErrVisaExpired          = errors.New("visa has expired")
	ErrWorkingHoursExceeded = errors.New("statutory working hours limit exceeded")
	ErrUnauthorizedAccess   = errors.New("unauthorized compliance action")
	ErrAlertNotFound        = errors.New("compliance alert not found")
	ErrAlreadyFlagged       = errors.New("visa is already flagged for expiration")

	ErrAuthorizationDenied     = errors.New("authorization denied for this compliance action")
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved tenant (no X-Tenant-Id header) — fail closed rather than
	// defaulting to a shared tenant bucket.
	ErrIdentityMissing = errors.New("tenant identity missing")
)

type VerificationStatus string

const (
	VerificationStatusPending  VerificationStatus = "PENDING"
	VerificationStatusVerified VerificationStatus = "VERIFIED"
	VerificationStatusExpired  VerificationStatus = "EXPIRED"
	VerificationStatusRejected VerificationStatus = "REJECTED"
)

type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "INFO"
	AlertSeverityWarning  AlertSeverity = "WARNING"
	AlertSeverityCritical AlertSeverity = "CRITICAL"
)

type WorkAuthorization struct {
	AuthID         string             `json:"auth_id"`
	TenantID       string             `json:"tenant_id"`
	LegalEntityID  string             `json:"legal_entity_id"`
	EmployeeID     string             `json:"employee_id"`
	DocumentType   string             `json:"document_type"` // e.g. I-9, RIGHT_TO_WORK, PASSPORT, WORK_PERMIT
	DocumentNumber string             `json:"document_number"`
	IssueDate      string             `json:"issue_date"`
	ExpiryDate     *string            `json:"expiry_date,omitempty"`
	Status         VerificationStatus `json:"status"`
	VerifiedBy     *string            `json:"verified_by,omitempty"`
	VerifiedAt     *time.Time         `json:"verified_at,omitempty"`
	EffectiveFrom  string             `json:"effective_from"`
	EffectiveTo    *string            `json:"effective_to,omitempty"`
	CorrelationID  string             `json:"correlation_id"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

type VisaRecord struct {
	VisaID           string             `json:"visa_id"`
	TenantID         string             `json:"tenant_id"`
	LegalEntityID    string             `json:"legal_entity_id"`
	EmployeeID       string             `json:"employee_id"`
	VisaType         string             `json:"visa_type"` // H1B, L1, O1, TIER_2, STAMP
	IssuingCountry   string             `json:"issuing_country"`
	ExpirationDate   string             `json:"expiration_date"`
	GracePeriodDays  int                `json:"grace_period_days"`
	Status           VerificationStatus `json:"status"`
	FlaggedForExpiry bool               `json:"flagged_for_expiry"`
	CorrelationID    string             `json:"correlation_id"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

type WorkingHourLog struct {
	LogID             string    `json:"log_id"`
	TenantID          string    `json:"tenant_id"`
	LegalEntityID     string    `json:"legal_entity_id"`
	EmployeeID        string    `json:"employee_id"`
	WorkDate          string    `json:"work_date"`
	HoursWorked       float64   `json:"hours_worked"`
	OvertimeHours     float64   `json:"overtime_hours"`
	WeeklyAccumulated float64   `json:"weekly_accumulated"`
	IsBreached        bool      `json:"is_breached"`
	MaxAllowedHours   float64   `json:"max_allowed_hours"`
	CorrelationID     string    `json:"correlation_id"`
	CreatedAt         time.Time `json:"created_at"`
}

type ComplianceAlert struct {
	AlertID       string        `json:"alert_id"`
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	EmployeeID    string        `json:"employee_id"`
	Category      string        `json:"category"` // VISA_EXPIRATION, HOUR_LIMIT_BREACH, I9_VERIFICATION_REQUIRED
	Severity      AlertSeverity `json:"severity"`
	Message       string        `json:"message"`
	IsResolved    bool          `json:"is_resolved"`
	ResolvedBy    *string       `json:"resolved_by,omitempty"`
	ResolvedAt    *time.Time    `json:"resolved_at,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

type CreateWorkAuthRequest struct {
	LegalEntityID  string  `json:"legal_entity_id"`
	EmployeeID     string  `json:"employee_id"`
	DocumentType   string  `json:"document_type"`
	DocumentNumber string  `json:"document_number"`
	IssueDate      string  `json:"issue_date"`
	ExpiryDate     *string `json:"expiry_date,omitempty"`
	EffectiveFrom  string  `json:"effective_from"`
	CorrelationID  string  `json:"correlation_id"`
}

type VerifyWorkAuthRequest struct {
	VerifiedBy string `json:"verified_by"`
}

type CreateVisaRecordRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	EmployeeID      string `json:"employee_id"`
	VisaType        string `json:"visa_type"`
	IssuingCountry  string `json:"issuing_country"`
	ExpirationDate  string `json:"expiration_date"`
	GracePeriodDays int    `json:"grace_period_days"`
	CorrelationID   string `json:"correlation_id"`
}

type LogWorkingHoursRequest struct {
	LegalEntityID string  `json:"legal_entity_id"`
	EmployeeID    string  `json:"employee_id"`
	WorkDate      string  `json:"work_date"`
	HoursWorked   float64 `json:"hours_worked"`
	OvertimeHours float64 `json:"overtime_hours"`
	CorrelationID string  `json:"correlation_id"`
}
