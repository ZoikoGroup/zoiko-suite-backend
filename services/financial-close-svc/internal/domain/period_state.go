package domain

import "time"

// Period close states (ACC-14, ZS-SVC-B-001 §17; accounting kernel §10.1).
//
//	OPEN → SOFT_CLOSE → CLOSE_REVIEW → HARD_CLOSED → AUTHORIZED_REOPEN → RECLOSED
//	                                              ↑_____________________|
//
// RECLOSED behaves as HARD_CLOSED (and may be reopened again); it is kept
// distinct because the spec names it and because a reclosed period's latest
// evidence comes from a reclose, not the original close.
const (
	PeriodOpen             = "OPEN"
	PeriodSoftClose        = "SOFT_CLOSE"
	PeriodCloseReview      = "CLOSE_REVIEW"
	PeriodHardClosed       = "HARD_CLOSED"
	PeriodAuthorizedReopen = "AUTHORIZED_REOPEN"
	PeriodReclosed         = "RECLOSED"
)

// Posting policies: what a period's state allows, as the kernel standard's
// §10.1 table states it.
const (
	// PostingOpen: "Normal authorized posting permitted."
	PostingOpen = "OPEN"
	// PostingRestricted (SOFT_CLOSE): "Restricted posting; exceptions require
	// elevated approval and reason."
	PostingRestricted = "RESTRICTED"
	// PostingCloseJournalsOnly (CLOSE_REVIEW): "Only designated close
	// journals/workflows permitted."
	PostingCloseJournalsOnly = "CLOSE_JOURNALS_ONLY"
	// PostingReopened (AUTHORIZED_REOPEN, inside its window): "Temporarily
	// reopened under explicit authority; all activity separately evidenced."
	PostingReopened = "REOPENED"
	// PostingClosed: hard closed, reclosed, or a reopen whose window ended.
	PostingClosed = "CLOSED"
)

// MaxReopenWindow caps how long an authorized reopen may last. The standard
// requires a "time-bounded scope"; a month is long enough for any correction
// and short enough that a forgotten reopen cannot leave a closed year open.
const MaxReopenWindow = 30 * 24 * time.Hour

// PostingPolicy is what fp's state allows at now.
func (fp *FiscalPeriod) PostingPolicy(now time.Time) string {
	switch fp.CloseStatus {
	case PeriodOpen:
		return PostingOpen
	case PeriodSoftClose:
		return PostingRestricted
	case PeriodCloseReview:
		return PostingCloseJournalsOnly
	case PeriodAuthorizedReopen:
		if fp.ReopenExpiresAt != nil && now.Before(*fp.ReopenExpiresAt) {
			return PostingReopened
		}
		return PostingClosed
	}
	return PostingClosed
}

// LegacyCloseStatus is close_status as services written against the old
// OPEN/LOCKED model read it. Four of them (general-ledger, asset-management,
// inventory-management, project-accounting) treat anything other than
// LOCKED/CLOSED as open, so a new state name would make them post into a
// hard-closed month. This answers OPEN only when posting is unrestricted and
// LOCKED otherwise: soft close and close review therefore read as locked to
// them — stricter than the standard, never looser — until they read
// period_state and posting_policy instead.
func (fp *FiscalPeriod) LegacyCloseStatus(now time.Time) string {
	switch fp.PostingPolicy(now) {
	case PostingOpen, PostingReopened:
		return "OPEN"
	}
	return "LOCKED"
}

// CloseJournalsAllowed reports whether the close's own journals (accrual and
// prepayment recognition and termination — the "designated close journals"
// of the standard) may post in fp now: through soft close and close review,
// and inside a reopen window; never once hard closed.
func (fp *FiscalPeriod) CloseJournalsAllowed(now time.Time) bool {
	switch fp.PostingPolicy(now) {
	case PostingOpen, PostingRestricted, PostingCloseJournalsOnly, PostingReopened:
		return true
	}
	return false
}

// OrdinaryPostingAllowed reports whether ordinary (non-close) postings may
// land in fp now: open, or inside a reopen window.
func (fp *FiscalPeriod) OrdinaryPostingAllowed(now time.Time) bool {
	switch fp.PostingPolicy(now) {
	case PostingOpen, PostingReopened:
		return true
	}
	return false
}

// PeriodUpdate is one state transition and the fields it sets.
type PeriodUpdate struct {
	To              string
	PrincipalID     string
	Reason          string
	ReopenRequestID string
	At              time.Time

	// Set on hard close and reclose.
	LockedAt      *time.Time
	EvidenceDocID *string
	// Set on an approved reopen; cleared on reclose.
	ReopenedAt      *time.Time
	ReopenExpiresAt *time.Time
	ClearReopen     bool
}

// PeriodTransition is one row of a period's close history.
type PeriodTransition struct {
	TransitionID    string    `json:"transition_id"`
	TenantID        string    `json:"tenant_id"`
	FiscalPeriodID  string    `json:"fiscal_period_id"`
	FromState       string    `json:"from_state"`
	ToState         string    `json:"to_state"`
	PrincipalID     string    `json:"principal_id"`
	Reason          string    `json:"reason,omitempty"`
	ReopenRequestID string    `json:"reopen_request_id,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
}

// Reopen request statuses.
const (
	ReopenPending  = "PENDING"
	ReopenApproved = "APPROVED"
	ReopenRejected = "REJECTED"
)

// ReopenRequest asks to reopen a closed period until ReopenUntil. It takes
// effect only when a different principal approves it.
type ReopenRequest struct {
	RequestID              string     `json:"request_id"`
	TenantID               string     `json:"tenant_id"`
	FiscalPeriodID         string     `json:"fiscal_period_id"`
	RequestedByPrincipalID string     `json:"requested_by_principal_id"`
	Reason                 string     `json:"reason"`
	ReopenUntil            time.Time  `json:"reopen_until"`
	Status                 string     `json:"status"`
	DecidedByPrincipalID   string     `json:"decided_by_principal_id,omitempty"`
	DecidedAt              *time.Time `json:"decided_at,omitempty"`
	DecisionReason         string     `json:"decision_reason,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
}

// RequestReopenRequest is the body of a reopen request.
type RequestReopenRequest struct {
	Reason      string    `json:"reason"`
	ReopenUntil time.Time `json:"reopen_until"`
}

// DecideReopenRequest is the body of an approval or rejection.
type DecideReopenRequest struct {
	Reason string `json:"reason"`
}

// PeriodTransitionRequest is the body of a soft-close or close-review
// command; the reason is optional.
type PeriodTransitionRequest struct {
	Reason string `json:"reason"`
}

// PeriodStatusView is GET /v1/close/periods/status: close_status keeps its
// legacy meaning (see LegacyCloseStatus); period_state and posting_policy
// are the full answer.
type PeriodStatusView struct {
	FiscalPeriodID  string     `json:"fiscal_period_id,omitempty"`
	PeriodName      string     `json:"period_name"`
	CloseStatus     string     `json:"close_status"`
	PeriodState     string     `json:"period_state"`
	PostingPolicy   string     `json:"posting_policy"`
	ReopenExpiresAt *time.Time `json:"reopen_expires_at,omitempty"`
}

// CloseHistory is GET /v1/close/periods/{id}/history.
type CloseHistory struct {
	FiscalPeriodID string             `json:"fiscal_period_id"`
	Transitions    []PeriodTransition `json:"transitions"`
	ReopenRequests []ReopenRequest    `json:"reopen_requests"`
}

// AvailableCloseActions is GET /v1/close/periods/{id}/available-actions.
type AvailableCloseActions struct {
	FiscalPeriodID string   `json:"fiscal_period_id"`
	PeriodState    string   `json:"period_state"`
	PostingPolicy  string   `json:"posting_policy"`
	Actions        []string `json:"actions"`
}

const (
	ErrInvalidPeriodTransition = errorString("the period is not in a state that allows this transition")
	ErrReopenRequestPending    = errorString("a reopen request for this period is already pending")
	ErrReopenRequestNotFound   = errorString("reopen request not found or already decided")
	ErrReopenSelfApproval      = errorString("a reopen must be approved by someone other than the person who requested it")
	ErrReopenWindowInvalid     = errorString("reopen_until must be in the future and no more than 30 days away")
	ErrReopenRetired           = errorString("a closed period is reopened by request and independent approval: POST /v1/close/periods/{id}/reopen-requests, then /v1/close/reopen-requests/{request_id}/approve")
)
