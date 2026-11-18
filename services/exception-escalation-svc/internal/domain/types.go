package domain

import (
	"errors"
	"time"
)

var (
	ErrExceptionCaseNotFound   = errors.New("exception_case not found")
	ErrEscalationRecordNotFound = errors.New("escalation_record not found")
	ErrTenantMissing           = errors.New("tenant scope missing")
	ErrVersionMismatch         = errors.New("version mismatch")
	ErrCaseAlreadyClosed       = errors.New("exception case is already closed")

	// ErrSelfApprovalNotAllowed enforces Segregation of Duties
	ErrSelfApprovalNotAllowed = errors.New("principal may not approve or decide on their own submission")
)

// ExceptionType represents the exception type per Doc 04 §13.1
type ExceptionType string

const (
	ExceptionTypeMissedDeadline     ExceptionType = "MISSED_DEADLINE"
	ExceptionTypeInsufficientEvidence ExceptionType = "INSUFFICIENT_EVIDENCE"
	ExceptionTypeRuleConflict       ExceptionType = "RULE_CONFLICT"
	ExceptionTypeJurisdictionChange ExceptionType = "JURISDICTION_CHANGE"
	ExceptionTypeDataQuality        ExceptionType = "DATA_QUALITY"
)

// SeverityLevel represents the severity level per Doc 04 §13.1
type SeverityLevel string

const (
	SeverityCritical SeverityLevel = "CRITICAL"
	SeverityHigh     SeverityLevel = "HIGH"
	SeverityMedium   SeverityLevel = "MEDIUM"
	SeverityLow      SeverityLevel = "LOW"
)

// LinkedObjectType represents the linked object type per Doc 04 §13.1
type LinkedObjectType string

const (
	LinkedObjectObligation  LinkedObjectType = "OBLIGATION"
	LinkedObjectFiling      LinkedObjectType = "FILING"
	LinkedObjectPolicy      LinkedObjectType = "POLICY"
	LinkedObjectContract    LinkedObjectType = "CONTRACT"
)

// CaseStatus represents the case status per Doc 04 §13.1
type CaseStatus string

const (
	CaseStatusOpen         CaseStatus = "OPEN"
	CaseStatusInvestigating CaseStatus = "INVESTIGATING"
	CaseStatusResolved     CaseStatus = "RESOLVED"
	CaseStatusEscalated    CaseStatus = "ESCALATED"
	CaseStatusClosed       CaseStatus = "CLOSED"
)

// EscalationStatus represents the escalation status per Doc 04 §13.1
type EscalationStatus string

const (
	EscalationStatusPending      EscalationStatus = "PENDING"
	EscalationStatusAcknowledged EscalationStatus = "ACKNOWLEDGED"
	EscalationStatusResolved     EscalationStatus = "RESOLVED"
	EscalationStatusExpired      EscalationStatus = "EXPIRED"
)

// ExceptionCase represents the exception case per Doc 04 §13.1
type ExceptionCase struct {
	ExceptionCaseID   string        `json:"exception_case_id"`
	TenantID          string        `json:"tenant_id"`
	LegalEntityID     string        `json:"legal_entity_id"`
	ExceptionType     ExceptionType `json:"exception_type"`
	SeverityLevel     SeverityLevel `json:"severity_level"`
	LinkedObjectType  LinkedObjectType `json:"linked_object_type"`
	LinkedObjectID    string        `json:"linked_object_id"`
	CaseStatus        CaseStatus    `json:"case_status"`
	EscalatedAt       *time.Time    `json:"escalated_at,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	UpdatedAt         time.Time     `json:"updated_at"`
	Version           int           `json:"version"`
}

// EscalationRecord represents the escalation record per Doc 04 §13.1
type EscalationRecord struct {
	EscalationRecordID  string           `json:"escalation_record_id"`
	ExceptionCaseID     string           `json:"exception_case_id"`
	EscalatedToRole     string           `json:"escalated_to_role"`
	EscalationReason    string           `json:"escalation_reason"`
	EscalationStatus    EscalationStatus `json:"escalation_status"`
	EscalatedAt         time.Time        `json:"escalated_at"`
	CreatedAt           time.Time        `json:"created_at"`
	CreatedByPrincipalID string          `json:"created_by_principal_id"`
}

// CreateExceptionCaseParams holds input parameters for creating an exception case
type CreateExceptionCaseParams struct {
	ExceptionCaseID        string
	TenantID               string
	LegalEntityID          string
	ExceptionType          ExceptionType
	SeverityLevel          SeverityLevel
	LinkedObjectType       LinkedObjectType
	LinkedObjectID         string
	CreatedByPrincipalID   string
	CorrelationID          string
}

// UpdateExceptionCaseStatusParams holds input parameters for updating exception case status
type UpdateExceptionCaseStatusParams struct {
	CaseStatus     CaseStatus
	ExpectedVersion int
}

// CreateEscalationRecordParams holds input parameters for creating an escalation record
type CreateEscalationRecordParams struct {
	EscalationRecordID     string
	ExceptionCaseID        string
	EscalatedToRole        string
	EscalationReason       string
	CreatedByPrincipalID   string
	CorrelationID          string
}

// ListExceptionCasesFilter holds optional filters for querying exception cases
type ListExceptionCasesFilter struct {
	TenantID       string
	LegalEntityID  string
	ExceptionType  ExceptionType
	CaseStatus     CaseStatus
	SeverityLevel  SeverityLevel
	Limit          int
	Offset         int
}

// ListEscalationRecordsFilter holds optional filters for querying escalation records
type ListEscalationRecordsFilter struct {
	ExceptionCaseID string
	EscalatedToRole string
	EscalationStatus EscalationStatus
	Limit           int
	Offset          int
}

// IsValidExceptionType checks if the exception type is valid
func IsValidExceptionType(t ExceptionType) bool {
	switch t {
	case ExceptionTypeMissedDeadline, ExceptionTypeInsufficientEvidence, ExceptionTypeRuleConflict,
		ExceptionTypeJurisdictionChange, ExceptionTypeDataQuality:
		return true
	default:
		return false
	}
}

// IsValidSeverityLevel checks if the severity level is valid
func IsValidSeverityLevel(s SeverityLevel) bool {
	switch s {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow:
		return true
	default:
		return false
	}
}

// IsValidLinkedObjectType checks if the linked object type is valid
func IsValidLinkedObjectType(t LinkedObjectType) bool {
	switch t {
	case LinkedObjectObligation, LinkedObjectFiling, LinkedObjectPolicy, LinkedObjectContract:
		return true
	default:
		return false
	}
}

// IsValidCaseStatus checks if the case status is valid
func IsValidCaseStatus(s CaseStatus) bool {
	switch s {
	case CaseStatusOpen, CaseStatusInvestigating, CaseStatusResolved, CaseStatusEscalated, CaseStatusClosed:
		return true
	default:
		return false
	}
}

// IsValidEscalationStatus checks if the escalation status is valid
func IsValidEscalationStatus(s EscalationStatus) bool {
	switch s {
	case EscalationStatusPending, EscalationStatusAcknowledged, EscalationStatusResolved, EscalationStatusExpired:
		return true
	default:
		return false
	}
}