package domain

import (
	"errors"
	"time"
)

var (
	ErrRecordNotFound        = errors.New("compliance record not found")
	ErrVisaExpired           = errors.New("visa has expired")
	ErrWorkingHoursExceeded  = errors.New("statutory working hours limit exceeded")
	ErrUnauthorizedAccess    = errors.New("unauthorized compliance action")
)

type VerificationStatus string

const (
	VerificationStatusPending   VerificationStatus = "PENDING"
	VerificationStatusVerified  VerificationStatus = "VERIFIED"
	VerificationStatusExpired   VerificationStatus = "EXPIRED"
	VerificationStatusRejected  VerificationStatus = "REJECTED"
)

type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "INFO"
	AlertSeverityWarning  AlertSeverity = "WARNING"
	AlertSeverityCritical AlertSeverity = "CRITICAL"
)

type WorkAuthorization struct {
	AuthID          string             `json:"auth_id"`
	TenantID        string             `json:"tenant_id"`
	LegalEntityID   string             `json:"legal_entity_id"`
	EmployeeID      string             `json:"employee_id"`
	DocumentType    string             `json:"document_type"` // e.g. I-9, RIGHT_TO_WORK, PASSPORT, WORK_PERMIT
	DocumentNumber  string             `json:"document_number"`
	IssueDate       string             `json:"issue_date"`
	ExpiryDate      *string            `json:"expiry_date,omitempty"`
	Status          VerificationStatus `json:"status"`
	VerifiedBy      *string            `json:"verified_by,omitempty"`
	VerifiedAt      *time.Time         `json:"verified_at,omitempty"`
	EffectiveFrom   string             `json:"effective_from"`
	EffectiveTo     *string            `json:"effective_to,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

type VisaRecord struct {
	VisaID          string             `json:"visa_id"`
	TenantID        string             `json:"tenant_id"`
	LegalEntityID   string             `json:"legal_entity_id"`
	EmployeeID      string             `json:"employee_id"`
	VisaType        string             `json:"visa_type"` // H1B, L1, O1, TIER_2, STAMP
	IssuingCountry  string             `json:"issuing_country"`
	ExpirationDate  string             `json:"expiration_date"`
	GracePeriodDays int                `json:"grace_period_days"`
	Status          VerificationStatus `json:"status"`
	FlaggedForExpiry bool              `json:"flagged_for_expiry"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

type WorkingHourLog struct {
	LogID           string    `json:"log_id"`
	TenantID        string    `json:"tenant_id"`
	LegalEntityID   string    `json:"legal_entity_id"`
	EmployeeID      string    `json:"employee_id"`
	WorkDate        string    `json:"work_date"`
	HoursWorked     float64   `json:"hours_worked"`
	OvertimeHours   float64   `json:"overtime_hours"`
	WeeklyAccumulated float64 `json:"weekly_accumulated"`
	IsBreached      bool      `json:"is_breached"`
	MaxAllowedHours float64   `json:"max_allowed_hours"`
	CreatedAt       time.Time `json:"created_at"`
}

type ComplianceAlert struct {
	AlertID         string        `json:"alert_id"`
	TenantID        string        `json:"tenant_id"`
	LegalEntityID   string        `json:"legal_entity_id"`
	EmployeeID      string        `json:"employee_id"`
	Category        string        `json:"category"` // VISA_EXPIRATION, HOUR_LIMIT_BREACH, I9_VERIFICATION_REQUIRED
	Severity        AlertSeverity `json:"severity"`
	Message         string        `json:"message"`
	IsResolved      bool          `json:"is_resolved"`
	ResolvedBy      *string       `json:"resolved_by,omitempty"`
	ResolvedAt      *time.Time    `json:"resolved_at,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

type CreateWorkAuthRequest struct {
	LegalEntityID  string  `json:"legal_entity_id"`
	EmployeeID     string  `json:"employee_id"`
	DocumentType   string  `json:"document_type"`
	DocumentNumber string  `json:"document_number"`
	IssueDate      string  `json:"issue_date"`
	ExpiryDate     *string `json:"expiry_date,omitempty"`
	EffectiveFrom  string  `json:"effective_from"`
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
}

type LogWorkingHoursRequest struct {
	LegalEntityID string  `json:"legal_entity_id"`
	EmployeeID    string  `json:"employee_id"`
	WorkDate      string  `json:"work_date"`
	HoursWorked   float64 `json:"hours_worked"`
	OvertimeHours float64 `json:"overtime_hours"`
}
