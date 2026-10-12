package domain

import "time"

// MatchRunRecord is one persisted, immutable match run.
type MatchRunRecord struct {
	RunID             string      `json:"run_id"`
	TenantID          string      `json:"tenant_id,omitempty"`
	LegalEntityID     string      `json:"legal_entity_id"`
	InvoiceID         string      `json:"invoice_id"`
	RunNumber         int         `json:"run_number"`
	Result            MatchState  `json:"result"`
	Mode              MatchMode   `json:"mode"`
	PolicyVersion     int         `json:"policy_version"`
	PolicySnapshot    MatchPolicy `json:"policy_snapshot"`
	PurchaseOrderID   string      `json:"purchase_order_id"`
	PORevision        *int        `json:"po_revision,omitempty"`
	InvoiceVersion    int         `json:"invoice_version"`
	InputHash         string      `json:"input_hash"`
	Totals            MatchTotals `json:"totals"`
	RequestedBy       string      `json:"requested_by"`
	CorrelationID     string      `json:"correlation_id,omitempty"`
	CreatedAt         time.Time   `json:"created_at"`
	SupersededAt      *time.Time  `json:"superseded_at,omitempty"`
	SupersededByRunID *string     `json:"superseded_by_run_id,omitempty"`
	SupersedeReason   string      `json:"supersede_reason,omitempty"`
}

// MatchResultView is a run with its line facts and exceptions, plus where the
// invoice's own match dimension stands.
type MatchResultView struct {
	Run        *MatchRunRecord        `json:"run"`
	Lines      []MatchLineResult      `json:"lines"`
	Exceptions []MatchExceptionRecord `json:"exceptions"`
	// InvoiceMatchState / Cleared are the invoice's dimension: Cleared is true only
	// for a MATCHED / WITHIN_TOLERANCE run, or once every exception of the live run
	// has an approved variance.
	InvoiceMatchState MatchState `json:"invoice_match_state"`
	Cleared           bool       `json:"cleared"`
	// Created is false when a run command found identical evidence and returned the
	// existing live run (a deterministic re-performance is a no-op).
	Created bool `json:"created"`
}

// SaveMatchRunInput is what the handler hands the store after evaluating a run.
type SaveMatchRunInput struct {
	TenantID        string
	InvoiceID       string
	ExpectedVersion *int
	Command         string // RunInvoiceMatch | ReperformInvoiceMatch
	Actor           string
	CorrelationID   string
	Policy          MatchPolicy
	PurchaseOrderID string
	PORevision      *int
	Outcome         MatchOutcome
}

// Exception actions.
const (
	ActionAcknowledge     = "ACKNOWLEDGE"
	ActionRoute           = "ROUTE"
	ActionApproveVariance = "APPROVE_VARIANCE"
)

type ExceptionAction struct {
	TenantID      string
	ExceptionID   string
	Kind          string
	Actor         string
	Reason        string
	Ref           string
	RouteTo       string
	CorrelationID string
}

type ExceptionFilter struct {
	TenantID      string
	LegalEntityID string
	Status        string
	InvoiceID     string
	Limit         int
}

// SupersedeInput invalidates the live run without starting a new one.
type SupersedeInput struct {
	TenantID        string
	InvoiceID       string
	ExpectedVersion *int
	Actor           string
	Reason          string
	CorrelationID   string
}

// Outbox event types (spec: events produced).
const (
	EventInvoiceMatchStarted          = "InvoiceMatchStarted"
	EventInvoiceMatched               = "InvoiceMatched"
	EventInvoiceMatchExceptionRaised  = "InvoiceMatchExceptionRaised"
	EventInvoiceMatchVarianceApproved = "InvoiceMatchVarianceApproved"
	EventInvoiceMatchSuperseded       = "InvoiceMatchSuperseded"
)
