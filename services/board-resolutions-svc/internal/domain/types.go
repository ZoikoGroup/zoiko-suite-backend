package domain

import (
	"errors"
	"time"
)

var (
	ErrMeetingNotFound            = errors.New("board meeting not found")
	ErrResolutionNotFound         = errors.New("board resolution not found")
	ErrResolutionAlreadyFinalized = errors.New("resolution is already passed, rejected, or rescinded")

	// ErrTenantMissing means the request carried no X-Tenant-Id. It is an
	// unauthenticated request, not an empty tenant named "default".
	ErrTenantMissing = errors.New("tenant scope missing")

	// ErrInvalidField is what a value Postgres could not accept becomes —
	// a malformed date, a malformed identifier — so a caller's typo answers
	// 400 rather than the 500 an unmapped driver error produced.
	ErrInvalidField = errors.New("a submitted field is not a valid value")

	// ErrSelfApprovalNotAllowed enforces the platform's Segregation of Duties
	// doctrine (docs/original_doc/zoiko_suite_doc1.txt §12.3): the principal
	// who created a record may not be the same principal who approves,
	// executes, or passes it.
	ErrSelfApprovalNotAllowed = errors.New("principal may not approve or decide on their own submission")
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

// IsFinal reports whether the resolution has reached a terminal status and can
// no longer be voted on or passed.
//
// One definition, used by both transitions. They each carried their own list
// and the lists disagreed: the closing action's omitted REJECTED, so a
// resolution the board had rejected could still be passed into force.
func (s ResolutionStatus) IsFinal() bool {
	return s == ResolutionStatusPassed || s == ResolutionStatusRejected || s == ResolutionStatusRescinded
}

// IsValidCategory reports whether c is one of the five categories the schema
// documents. The category is not decoration: it is the domain_code sent to
// evidence-requirements-svc, so an unrecognised one asks the catalog about a
// domain that does not exist and comes back with no requirements — an evidence
// gate silently bypassed by a typo.
func (c ResolutionCategory) IsValid() bool {
	switch c {
	case ResolutionCategoryGovernance, ResolutionCategoryFinancial,
		ResolutionCategoryOperational, ResolutionCategoryExecutive, ResolutionCategoryStatutory:
		return true
	}
	return false
}

// MeetingFilter and ResolutionFilter carry every constraint on a register
// read, including the paging bounds — grouped into a struct rather than a
// growing list of same-typed string parameters that a caller could transpose
// without the compiler noticing.
type MeetingFilter struct {
	LegalEntityID string
	Limit         int
	Offset        int
}

type ResolutionFilter struct {
	LegalEntityID string
	MeetingID     string
	Status        string
	Limit         int
	Offset        int
}

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
