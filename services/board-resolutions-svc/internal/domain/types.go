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

	// ErrResolutionNotOpen means the resolution is not in OPEN status — only
	// an OPEN resolution may receive a vote or be closed.
	ErrResolutionNotOpen = errors.New("resolution is not open for voting")

	// ErrNotEligibleVoter means the caller is not on the resolution's frozen
	// voter roster. LEG-04 §6.1: "Quorum and voter eligibility are evaluated
	// against a frozen as-of entitlement population" — only roster members
	// may cast a vote.
	ErrNotEligibleVoter = errors.New("principal is not on the resolution's voter roster")

	// ErrAlreadyVoted means this voter already cast a vote on this
	// resolution. A vote is evidence once cast, not a mutable field.
	ErrAlreadyVoted = errors.New("principal has already voted on this resolution")

	// ErrEmptyRoster means OpenVoting was called with no voters — a
	// resolution cannot be opened for voting against an empty entitlement
	// population.
	ErrEmptyRoster = errors.New("voter roster must not be empty")

	// ErrInvalidQuorumThreshold means the requested quorum threshold is not
	// achievable against the roster size supplied.
	ErrInvalidQuorumThreshold = errors.New("quorum threshold must be between 1 and the roster size")

	// ErrResolutionNotPassed means SupersedeResolution was called on a
	// resolution that never reached PASSED — only a passed resolution can be
	// superseded.
	ErrResolutionNotPassed = errors.New("only a passed resolution may be superseded")
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

// ResolutionStatus follows LEG-04's lifecycle (docs/architecture/original_doc
// §6): Proposed -> Open -> Passed/Failed -> Superseded. Draft, Executed,
// Effective and Archived are deliberately not implemented in this pass — see
// migration 000003's doc comment for why.
type ResolutionStatus string

const (
	ResolutionStatusProposed  ResolutionStatus = "PROPOSED"
	ResolutionStatusOpen      ResolutionStatus = "OPEN"
	ResolutionStatusPassed    ResolutionStatus = "PASSED"
	ResolutionStatusFailed    ResolutionStatus = "FAILED"
	ResolutionStatusSuperseded ResolutionStatus = "SUPERSEDED"
)

// IsFinal reports whether the resolution has reached a terminal status and can
// no longer be voted on, opened, or closed.
//
// One definition, used everywhere a transition checks status. They used to
// disagree across call sites: the closing action's own list once omitted a
// terminal status, so a resolution already in that status could be
// transitioned again.
func (s ResolutionStatus) IsFinal() bool {
	return s == ResolutionStatusPassed || s == ResolutionStatusFailed || s == ResolutionStatusSuperseded
}

type Vote string

const (
	VoteFor     Vote = "FOR"
	VoteAgainst Vote = "AGAINST"
	VoteAbstain Vote = "ABSTAIN"
)

func (v Vote) IsValid() bool {
	switch v {
	case VoteFor, VoteAgainst, VoteAbstain:
		return true
	}
	return false
}

// VoterRosterEntry is one principal entitled to vote on a resolution, frozen
// at OpenVoting time. See migration 000003's doc comment: this is
// caller-asserted, not sourced from a director/shareholder register, because
// neither LEG-02 nor LEG-03 exists yet.
type VoterRosterEntry struct {
	ResolutionID      string    `json:"resolution_id"`
	VoterPrincipalID  string    `json:"voter_principal_id"`
	FrozenAt          time.Time `json:"frozen_at"`
}

// CastVoteRecord is one voter's vote on a resolution — the per-voter ledger
// LEG-04 names as GetVoteLedger. Append-only: a second vote from the same
// voter on the same resolution is refused, not overwritten.
type CastVoteRecord struct {
	ResolutionID     string    `json:"resolution_id"`
	VoterPrincipalID string    `json:"voter_principal_id"`
	Vote             Vote      `json:"vote"`
	CastAt           time.Time `json:"cast_at"`
	CastBy           string    `json:"cast_by"`
}

// QuorumEvidence is the computed answer to GetQuorumEvidence: how many of the
// frozen roster have voted, against the threshold frozen at OpenVoting time.
type QuorumEvidence struct {
	ResolutionID     string `json:"resolution_id"`
	RosterSize       int    `json:"roster_size"`
	QuorumThreshold  int    `json:"quorum_threshold"`
	VotesCast        int    `json:"votes_cast"`
	VotesFor         int    `json:"votes_for"`
	VotesAgainst     int    `json:"votes_against"`
	Abstentions      int    `json:"abstentions"`
	QuorumMet        bool   `json:"quorum_met"`
}

type OpenVotingRequest struct {
	VoterPrincipalIDs []string `json:"voter_principal_ids"`
	QuorumThreshold   int      `json:"quorum_threshold"`
}

type CastVoteRequest struct {
	Vote Vote `json:"vote"`
}

type SupersedeResolutionRequest struct {
	SupersededBy string `json:"superseded_by"`
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
	QuorumThreshold  *int               `json:"quorum_threshold,omitempty"`
	VotingOpenedAt   *time.Time         `json:"voting_opened_at,omitempty"`
	VotingClosedAt   *time.Time         `json:"voting_closed_at,omitempty"`
	SupersededBy     *string            `json:"superseded_by,omitempty"`
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

type CloseVotingRequest struct {
	DocumentVaultID *string `json:"document_vault_id,omitempty"`
}
