// Package domain defines the authoritative types of accounting-period-svc
// (REF-05).
//
// OWNERSHIP. This service owns AccountingPeriod, AccountingPeriodState and
// PeriodStateHistory. It explicitly does NOT own the fiscal-calendar definition
// (REF-04), the close checklist / workflow evidence (ACC-14) or postings. A
// period's boundaries are COPIED from a fiscal-calendar version at
// materialisation and never change afterwards.
//
// No floating point is used. Dates are calendar dates (YYYY-MM-DD strings,
// DATE in Postgres); instants are UTC.
package domain

import (
	"time"
)

// State is the lifecycle state of an accounting period.
type State string

const (
	StateOpen             State = "OPEN"
	StateSoftClosed       State = "SOFT_CLOSED"
	StateHardClosed       State = "HARD_CLOSED"
	StateReopenAuthorized State = "REOPEN_AUTHORIZED"
	StateReclosed         State = "RECLOSED"
)

// AllStates is the state vocabulary.
var AllStates = []State{StateOpen, StateSoftClosed, StateHardClosed, StateReopenAuthorized, StateReclosed}

// Valid reports whether s is in the vocabulary.
func (s State) Valid() bool {
	for _, v := range AllStates {
		if s == v {
			return true
		}
	}
	return false
}

// Kind is the kind of period.
type Kind string

const (
	KindNormal  Kind = "NORMAL"
	KindSpecial Kind = "SPECIAL"
)

// Valid reports whether k is NORMAL or SPECIAL.
func (k Kind) Valid() bool { return k == KindNormal || k == KindSpecial }

// ReopenWindow is the scope and time bound of an authorised reopen.
type ReopenWindow struct {
	BookScope   string    `json:"book_scope"`
	ModuleScope string    `json:"module_scope"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Period is one accounting period instance.
type Period struct {
	PeriodID          string `json:"period_id"`
	TenantID          string `json:"tenant_id"`
	LegalEntityID     string `json:"legal_entity_id"`
	CalendarID        string `json:"calendar_id"`
	CalendarVersionID string `json:"calendar_version_id"`
	// BookScope is opaque; "" means entity-wide (all books). REF-06 will
	// constrain it later.
	BookScope   string `json:"book_scope"`
	ModuleScope string `json:"module_scope"`
	PeriodKey   string `json:"period_key"`
	FiscalYear  int    `json:"fiscal_year"`
	PeriodNo    int    `json:"period_no"`
	StartDate   string `json:"start_date"` // YYYY-MM-DD, inclusive
	EndDate     string `json:"end_date"`   // YYYY-MM-DD, inclusive
	Kind        Kind   `json:"kind"`

	State   State `json:"state"`
	Version int64 `json:"version"`
	// Reopen is set once a reopen was authorised; it is the window the posting
	// gate enforces while the state is REOPEN_AUTHORIZED.
	Reopen *ReopenWindow `json:"reopen_authorization,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Command is a named state command.
type Command string

const (
	CmdMaterialize     Command = "MATERIALIZE"
	CmdSoftClose       Command = "SOFT_CLOSE"
	CmdHardClose       Command = "HARD_CLOSE"
	CmdAuthorizeReopen Command = "AUTHORIZE_REOPEN"
	CmdReclose         Command = "RECLOSE"
)

// HistoryEntry is one append-only PeriodStateHistory row.
type HistoryEntry struct {
	HistoryID           string        `json:"history_id"`
	TenantID            string        `json:"tenant_id"`
	PeriodID            string        `json:"period_id"`
	FromState           State         `json:"from_state"` // "" for the initial MATERIALIZE entry
	ToState             State         `json:"to_state"`
	Command             Command       `json:"command"`
	Acc14WorkflowRef    string        `json:"acc14_workflow_ref,omitempty"`
	ControlSnapshotRef  string        `json:"control_snapshot_ref,omitempty"`
	RequestedBy         string        `json:"requested_by"`
	Reason              string        `json:"reason"`
	RecordedAt          time.Time     `json:"recorded_at"`
	ExpectedVersion     int64         `json:"expected_version"`
	ResultingVersion    int64         `json:"resulting_version"`
	DecisionFingerprint string        `json:"decision_fingerprint,omitempty"`
	Reopen              *ReopenWindow `json:"reopen_authorization,omitempty"`
	CorrelationID       string        `json:"correlation_id,omitempty"`
}

// Posting modes reported by the gate.
const (
	ModeAllowed    = "ALLOWED"
	ModeRestricted = "RESTRICTED"
	ModeBlocked    = "BLOCKED"
)

// Gate reasons.
const (
	ReasonOpen                = "PERIOD_OPEN"
	ReasonSoftClosedException = "SOFT_CLOSED_EXCEPTION_PRESENTED"
	ReasonSoftClosed          = "SOFT_CLOSED_RESTRICTED"
	ReasonHardClosed          = "PERIOD_HARD_CLOSED"
	ReasonReclosed            = "PERIOD_RECLOSED"
	ReasonReopenInScope       = "REOPEN_AUTHORIZED_IN_SCOPE_AND_WINDOW"
	ReasonReopenExpired       = "REOPEN_WINDOW_EXPIRED"
	ReasonReopenOutOfScope    = "OUTSIDE_REOPEN_SCOPE"
	ReasonUnknownState        = "UNKNOWN_STATE"
)

// Decision is the posting-gate verdict for one period.
type Decision struct {
	Allowed bool
	Mode    string
	Reason  string
}

func scopeCovers(authorised, requested string) bool {
	return authorised == "" || authorised == requested
}

// PostingDecision applies the posting rule for the period's state at instant
// now. softException is the caller's soft-close exception assertion. It is a
// pure function, so the gate is table-testable.
//
//	OPEN              -> allowed
//	SOFT_CLOSED       -> restricted: allowed only with the soft-close exception
//	HARD_CLOSED       -> blocked
//	REOPEN_AUTHORIZED -> allowed only inside the authorised scope AND before expires_at
//	RECLOSED          -> blocked
//
// An unknown state is blocked: the gate never fails open.
func (p *Period) PostingDecision(now time.Time, bookScope, moduleScope string, softException bool) Decision {
	switch p.State {
	case StateOpen:
		return Decision{true, ModeAllowed, ReasonOpen}
	case StateSoftClosed:
		if softException {
			return Decision{true, ModeRestricted, ReasonSoftClosedException}
		}
		return Decision{false, ModeRestricted, ReasonSoftClosed}
	case StateHardClosed:
		return Decision{false, ModeBlocked, ReasonHardClosed}
	case StateReclosed:
		return Decision{false, ModeBlocked, ReasonReclosed}
	case StateReopenAuthorized:
		if p.Reopen == nil || !now.Before(p.Reopen.ExpiresAt) {
			return Decision{false, ModeBlocked, ReasonReopenExpired}
		}
		if !scopeCovers(p.Reopen.BookScope, bookScope) || !scopeCovers(p.Reopen.ModuleScope, moduleScope) {
			return Decision{false, ModeBlocked, ReasonReopenOutOfScope}
		}
		return Decision{true, ModeRestricted, ReasonReopenInScope}
	}
	return Decision{false, ModeBlocked, ReasonUnknownState}
}

// CloseStatus maps the period to the legacy financial-close-svc vocabulary
// (OPEN | CLOSED | LOCKED) that general-ledger-svc's CheckPeriodOpen reads.
// An expired REOPEN_AUTHORIZED window reads as CLOSED, consistent with the gate.
func (p *Period) CloseStatus(now time.Time) string {
	switch p.State {
	case StateOpen:
		return "OPEN"
	case StateReopenAuthorized:
		if p.Reopen != nil && now.Before(p.Reopen.ExpiresAt) {
			return "OPEN"
		}
		return "CLOSED"
	case StateSoftClosed, StateReclosed:
		return "CLOSED"
	}
	return "LOCKED" // HARD_CLOSED, and any unknown state: most restrictive
}

// CloseStatusRank orders CloseStatus values by restrictiveness.
func CloseStatusRank(s string) int {
	switch s {
	case "OPEN":
		return 0
	case "CLOSED":
		return 1
	}
	return 2
}
