package domain

import (
	"errors"
	"time"
)

var (
	ErrRequirementNotFound = errors.New("filing requirement not found")
	ErrAlreadySubmitted    = errors.New("filing requirement is already submitted")
	ErrAlreadyConfirmed    = errors.New("filing requirement is already confirmed")
	ErrAlreadyOverdue      = errors.New("filing requirement is already marked overdue")
)

type FilingStatus string

const (
	StatusScheduled  FilingStatus = "SCHEDULED"
	StatusDraftReady FilingStatus = "DRAFT_READY"
	StatusSubmitted  FilingStatus = "SUBMITTED"
	StatusConfirmed  FilingStatus = "CONFIRMED"
	StatusOverdue    FilingStatus = "OVERDUE"
	StatusRejected   FilingStatus = "REJECTED"
)

// FilingRequirement represents a scheduled or active statutory filing requirement tracked by authority.
type FilingRequirement struct {
	FilingID              string       `json:"filing_id"`
	TenantID              string       `json:"tenant_id"`
	LegalEntityID         string       `json:"legal_entity_id"`
	JurisdictionID        string       `json:"jurisdiction_id"`
	FilingAuthority       string       `json:"filing_authority"`
	FilingType            string       `json:"filing_type"`
	PeriodKey             string       `json:"period_key"`
	DueDate               string       `json:"due_date"`
	Status                FilingStatus `json:"status"`
	SubmissionReference   *string      `json:"submission_reference,omitempty"`
	SubmittedAt           *time.Time   `json:"submitted_at,omitempty"`
	SubmittedBy           *string      `json:"submitted_by,omitempty"`
	ConfirmationReference *string      `json:"confirmation_reference,omitempty"`
	ConfirmedAt           *time.Time   `json:"confirmed_at,omitempty"`
	RejectionReason       string       `json:"rejection_reason,omitempty"`
	Notes                 string       `json:"notes,omitempty"`
	CreatedBy             string       `json:"created_by"`
	CreatedAt             time.Time    `json:"created_at"`
	UpdatedAt             time.Time    `json:"updated_at"`
}

// CreateRequirementRequest is the payload to schedule a new filing requirement.
type CreateRequirementRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	JurisdictionID  string `json:"jurisdiction_id"`
	FilingAuthority string `json:"filing_authority"`
	FilingType      string `json:"filing_type"`
	PeriodKey       string `json:"period_key"`
	DueDate         string `json:"due_date"`
	Notes           string `json:"notes,omitempty"`
	CreatedBy       string `json:"created_by"`
}

// SubmitFilingRequest is the payload to record authority submission.
type SubmitFilingRequest struct {
	SubmissionReference string `json:"submission_reference"`
	SubmittedBy         string `json:"submitted_by"`
	Notes               string `json:"notes,omitempty"`
}

// ConfirmFilingRequest is the payload to record authority confirmation.
type ConfirmFilingRequest struct {
	ConfirmationReference string `json:"confirmation_reference"`
	Notes                 string `json:"notes,omitempty"`
}

// CheckOverdue checks if the filing requirement is past its due date.
func (f *FilingRequirement) CheckOverdue(todayStr string) bool {
	if f.Status == StatusSubmitted || f.Status == StatusConfirmed {
		return false
	}
	if todayStr > f.DueDate {
		f.Status = StatusOverdue
		return true
	}
	return false
}
