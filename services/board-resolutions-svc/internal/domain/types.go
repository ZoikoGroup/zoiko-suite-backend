package domain

import (
	"errors"
	"time"
)

var (
	ErrMeetingNotFound          = errors.New("board meeting not found")
	ErrResolutionNotFound       = errors.New("board resolution not found")
	ErrResolutionAlreadyFinalized = errors.New("resolution is already passed, rejected, or rescinded")
)

type MeetingStatus string

const (
	MeetingStatusScheduled  MeetingStatus = "SCHEDULED"
	MeetingStatusInProgress MeetingStatus = "IN_PROGRESS"
	MeetingStatusAdjourned  MeetingStatus = "ADJOURNED"
	MeetingStatusCancelled  MeetingStatus = "CANCELLED"
)

type ResolutionCategory string

const (
	ResolutionCategoryGovernance  ResolutionCategory = "GOVERNANCE"
	ResolutionCategoryFinancial   ResolutionCategory = "FINANCIAL"
	ResolutionCategoryOperational ResolutionCategory = "OPERATIONAL"
	ResolutionCategoryExecutive   ResolutionCategory = "EXECUTIVE"
	ResolutionCategoryStatutory   ResolutionCategory = "STATUTORY"
)

type ResolutionStatus string

const (
	ResolutionStatusProposed  ResolutionStatus = "PROPOSED"
	ResolutionStatusPassed    ResolutionStatus = "PASSED"
	ResolutionStatusRejected  ResolutionStatus = "REJECTED"
	ResolutionStatusRescinded ResolutionStatus = "RESCINDED"
)

type BoardMeeting struct {
	MeetingID      string        `json:"meeting_id"`
	TenantID       string        `json:"tenant_id"`
	LegalEntityID  string        `json:"legal_entity_id"`
	Title          string        `json:"title"`
	ScheduledAt    time.Time     `json:"scheduled_at"`
	Location       string        `json:"location,omitempty"`
	Status         MeetingStatus `json:"status"`
	MinutesSummary string        `json:"minutes_summary,omitempty"`
	EffectiveFrom  string        `json:"effective_from"`
	EffectiveTo    *string       `json:"effective_to,omitempty"`
	CreatedBy      string        `json:"created_by"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

type BoardResolution struct {
	ResolutionID     string             `json:"resolution_id"`
	MeetingID        string             `json:"meeting_id"`
	TenantID         string             `json:"tenant_id"`
	LegalEntityID    string             `json:"legal_entity_id"`
	ResolutionNumber string             `json:"resolution_number"`
	Title            string             `json:"title"`
	Content          string             `json:"content"`
	Category         ResolutionCategory `json:"category"`
	Status           ResolutionStatus   `json:"status"`
	VotesFor         int                `json:"votes_for"`
	VotesAgainst     int                `json:"votes_against"`
	Abstentions      int                `json:"abstentions"`
	PassedAt         *time.Time         `json:"passed_at,omitempty"`
	PassedBy         *string            `json:"passed_by,omitempty"`
	DocumentVaultID  *string            `json:"document_vault_id,omitempty"`
	EffectiveFrom    string             `json:"effective_from"`
	EffectiveTo      *string            `json:"effective_to,omitempty"`
	CreatedBy        string             `json:"created_by"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

type CreateMeetingRequest struct {
	LegalEntityID string    `json:"legal_entity_id"`
	Title         string    `json:"title"`
	ScheduledAt   time.Time `json:"scheduled_at"`
	Location      string    `json:"location,omitempty"`
	EffectiveFrom string    `json:"effective_from"`
	CreatedBy     string    `json:"created_by"`
}

type CreateResolutionRequest struct {
	MeetingID        string             `json:"meeting_id"`
	LegalEntityID    string             `json:"legal_entity_id"`
	ResolutionNumber string             `json:"resolution_number"`
	Title            string             `json:"title"`
	Content          string             `json:"content"`
	Category         ResolutionCategory `json:"category"`
	EffectiveFrom    string             `json:"effective_from"`
	EffectiveTo      *string            `json:"effective_to,omitempty"`
	CreatedBy        string             `json:"created_by"`
}

type RecordVotesRequest struct {
	VotesFor     int `json:"votes_for"`
	VotesAgainst int `json:"votes_against"`
	Abstentions  int `json:"abstentions"`
}

type PassResolutionRequest struct {
	PassedBy        string  `json:"passed_by"`
	DocumentVaultID *string `json:"document_vault_id,omitempty"`
}
