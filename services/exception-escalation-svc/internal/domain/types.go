package domain

import (
	"errors"
	"time"
)

var (
	ErrExceptionCaseNotFound = errors.New("exception case not found")
	ErrCaseAlreadyClosed     = errors.New("exception case is already closed")
	ErrEscalationNotFound    = errors.New("escalation record not found")
)

type SeverityLevel string

const (
	SeverityLow      SeverityLevel = "LOW"
	SeverityMedium   SeverityLevel = "MEDIUM"
	SeverityHigh     SeverityLevel = "HIGH"
	SeverityCritical SeverityLevel = "CRITICAL"
)

type CaseStatus string

const (
	CaseOpen               CaseStatus = "OPEN"
	CaseUnderInvestigation CaseStatus = "UNDER_INVESTIGATION"
	CaseEscalated          CaseStatus = "ESCALATED"
	CaseResolved           CaseStatus = "RESOLVED"
	CaseClosed             CaseStatus = "CLOSED"
)

type EscalationStatus string

const (
	EscalationPending  EscalationStatus = "PENDING"
	EscalationAccepted EscalationStatus = "ACCEPTED"
	EscalationActioned EscalationStatus = "ACTIONED"
	EscalationRejected EscalationStatus = "REJECTED"
)

// ExceptionCase represents a governed compliance exception linked to a material object.
type ExceptionCase struct {
	ExceptionCaseID  string        `json:"exception_case_id"`
	TenantID         string        `json:"tenant_id"`
	LegalEntityID    string        `json:"legal_entity_id"`
	JurisdictionID   string        `json:"jurisdiction_id"`
	ExceptionType    string        `json:"exception_type"`
	SeverityLevel    SeverityLevel `json:"severity_level"`
	LinkedObjectType string        `json:"linked_object_type"`
	LinkedObjectID   string        `json:"linked_object_id"`
	Description      string        `json:"description"`
	CaseStatus       CaseStatus    `json:"case_status"`
	AssignedToRole   string        `json:"assigned_to_role,omitempty"`
	AssignedToUser   string        `json:"assigned_to_user,omitempty"`
	EscalatedAt      *time.Time    `json:"escalated_at,omitempty"`
	ClosedAt         *time.Time    `json:"closed_at,omitempty"`
	ClosedBy         string        `json:"closed_by,omitempty"`
	ClosureReason    string        `json:"closure_reason,omitempty"`
	CreatedBy        string        `json:"created_by"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`

	Escalations []EscalationRecord `json:"escalations,omitempty"`
}

// EscalationRecord represents a specific escalation action step taken on an exception case.
type EscalationRecord struct {
	EscalationRecordID string           `json:"escalation_record_id"`
	TenantID           string           `json:"tenant_id"`
	ExceptionCaseID    string           `json:"exception_case_id"`
	EscalatedToRole    string           `json:"escalated_to_role"`
	EscalatedToUser    string           `json:"escalated_to_user,omitempty"`
	EscalationReason   string           `json:"escalation_reason"`
	EscalationStatus   EscalationStatus `json:"escalation_status"`
	EscalatedBy        string           `json:"escalated_by"`
	EscalatedAt        time.Time        `json:"escalated_at"`
	ResolvedAt         *time.Time       `json:"resolved_at,omitempty"`
	ResponseNotes      string           `json:"response_notes,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

// CreateExceptionRequest is the payload to open a new exception case.
type CreateExceptionRequest struct {
	LegalEntityID    string        `json:"legal_entity_id"`
	JurisdictionID   string        `json:"jurisdiction_id"`
	ExceptionType    string        `json:"exception_type"`
	SeverityLevel    SeverityLevel `json:"severity_level"`
	LinkedObjectType string        `json:"linked_object_type"`
	LinkedObjectID   string        `json:"linked_object_id"`
	Description      string        `json:"description"`
	AssignedToRole   string        `json:"assigned_to_role,omitempty"`
	CreatedBy        string        `json:"created_by"`
}

// EscalateCaseRequest is the payload to escalate an exception case.
type EscalateCaseRequest struct {
	EscalatedToRole  string `json:"escalated_to_role"`
	EscalatedToUser  string `json:"escalated_to_user,omitempty"`
	EscalationReason string `json:"escalation_reason"`
	EscalatedBy      string `json:"escalated_by"`
}

// ResolveCaseRequest is the payload to resolve and close an exception case.
type ResolveCaseRequest struct {
	ClosedBy      string `json:"closed_by"`
	ClosureReason string `json:"closure_reason"`
}
