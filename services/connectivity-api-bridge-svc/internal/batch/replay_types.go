package batch

import (
	"encoding/json"
	"time"
)

// Replay job states (§10.3).
const (
	ReplayDryRunComplete  = "DRY_RUN_COMPLETE"
	ReplayPendingApproval = "PENDING_APPROVAL"
	ReplayApproved        = "APPROVED"
	ReplayRunning         = "RUNNING"
	ReplayCompleted       = "COMPLETED"
	ReplayRejected        = "REJECTED"
	ReplayCancelled       = "CANCELLED"
	ReplayFailed          = "FAILED"
)

// Replay scope kinds. There is no unscoped replay: "replay everything" is not a shortcut.
const (
	ScopeBatch     = "BATCH"
	ScopeExchanges = "EXCHANGES"
	ScopeWindow    = "WINDOW"
)

// Replay reasons (§10.3).
const (
	ReasonIncidentRepair      = "INCIDENT_REPAIR"
	ReasonSourceCorrection    = "SOURCE_CORRECTION"
	ReasonProviderRedelivery  = "PROVIDER_REDELIVERY"
	ReasonMigrationValidation = "MIGRATION_VALIDATION"
	ReasonApprovedBackfill    = "APPROVED_BACKFILL"
)

var replayReasons = map[string]bool{ReasonIncidentRepair: true, ReasonSourceCorrection: true, ReasonProviderRedelivery: true,
	ReasonMigrationValidation: true, ReasonApprovedBackfill: true}

// Risk levels of a replay.
const (
	RiskLow  = "LOW"
	RiskHigh = "HIGH"
)

// ReplayScope is what a replay is about. At least one explicit selector is required.
type ReplayScope struct {
	// BATCH scope: the rows of one batch, selected by their outcome and/or their row numbers.
	BatchID    string   `json:"batch_id,omitempty"`
	RowStates  []string `json:"row_states,omitempty"`
	RowNumbers []int    `json:"row_numbers,omitempty"`
	// EXCHANGES scope: explicit exchange ids.
	ExchangeIDs []string `json:"exchange_ids,omitempty"`
	// WINDOW scope: the held exchanges of one connection and operation within a bounded time window.
	ConnectionID string     `json:"connection_id,omitempty"`
	OperationID  string     `json:"operation_id,omitempty"`
	From         *time.Time `json:"from,omitempty"`
	To           *time.Time `json:"to,omitempty"`
}

// ReplayJob is the durable record of a replay or backfill: who asked, why, over what, under which versions, who
// approved it, what a dry run projected and what the run did.
type ReplayJob struct {
	ReplayID          string          `json:"replay_id"`
	TenantID          string          `json:"tenant_id"`
	ConnectionID      string          `json:"connection_id,omitempty"`
	OperationID       string          `json:"operation_id,omitempty"`
	BatchID           string          `json:"batch_id,omitempty"`
	ScopeKind         string          `json:"scope_kind"`
	Scope             json.RawMessage `json:"scope"`
	ReasonCode        string          `json:"reason_code"`
	Reason            string          `json:"reason"`
	DryRun            bool            `json:"dry_run"`
	DryRunOf          string          `json:"dry_run_of,omitempty"`
	UseCurrentMapping bool            `json:"use_current_mapping"`
	State             string          `json:"state"`
	Risk              string          `json:"risk"`
	RiskReasons       []string        `json:"risk_reasons,omitempty"`
	ContractVersionID string          `json:"contract_version_id"`
	MappingVersionID  string          `json:"mapping_version_id,omitempty"`
	SelectedItems     int             `json:"selected_items"`
	Projected         json.RawMessage `json:"projected,omitempty"`
	Result            json.RawMessage `json:"result,omitempty"`
	RequestedBy       string          `json:"requested_by"`
	DecidedBy         string          `json:"decided_by,omitempty"`
	DecidedAt         *time.Time      `json:"decided_at,omitempty"`
	DecisionNote      string          `json:"decision_note,omitempty"`
	CorrelationID     string          `json:"correlation_id,omitempty"`
	LeaseUntil        *time.Time      `json:"-"`
	Version           int64           `json:"version"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	StartedAt         *time.Time      `json:"started_at,omitempty"`
	FinishedAt        *time.Time      `json:"finished_at,omitempty"`
}

// ReplayItem is one exchange a replay covers, with what it was and what it became (or would become).
type ReplayItem struct {
	ReplayID     string `json:"replay_id"`
	TenantID     string `json:"tenant_id"`
	ExchangeID   string `json:"exchange_id"`
	BeforeState  string `json:"before_state"`
	BeforeClass  string `json:"before_class,omitempty"`
	BeforeReason string `json:"before_reason,omitempty"`
	AfterState   string `json:"after_state,omitempty"`
	AfterClass   string `json:"after_class,omitempty"`
	AfterReason  string `json:"after_reason,omitempty"`
	Changed      bool   `json:"changed"`
	Applied      bool   `json:"applied"`
	Skipped      string `json:"skipped,omitempty"`
	Done         bool   `json:"done"`
}

// ReplayFilter narrows a list of replay jobs.
type ReplayFilter struct {
	State   string
	BatchID string
	Limit   int
}

// ReplayTx is what MutateReplay hands to its function.
type ReplayTx struct {
	J   *ReplayJob
	Out []OutEvent
}

// Replay summary counts: what a dry run projects and what a run did.
type ReplayCounts struct {
	Selected    int `json:"selected"`
	Replayable  int `json:"replayable"`
	WouldAccept int `json:"accepted"`
	Quarantined int `json:"quarantined"`
	Duplicate   int `json:"duplicate"`
	Changed     int `json:"changed"`
	Applied     int `json:"applied"`
	Skipped     int `json:"skipped"`
	Failed      int `json:"failed"`
}
