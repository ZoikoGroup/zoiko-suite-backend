package kernel

import "context"

// Report-to-source trace (standard §31, §32, A12): a figure of a retained snapshot is explained by the ledger entries behind it,
// and each entry by the journal, the accounting event, the rule version and the evidence it came from. The trace of one account of
// a snapshot also proves itself: what the entries add up to is compared with the figure the snapshot holds.

const (
	DefaultSnapshotTracePage = 200
	MaxSnapshotTracePage     = 1000
)

// SnapshotTraceQuery selects the entries behind one account of a snapshot. After is the last entry sequence already seen.
type SnapshotTraceQuery struct {
	SnapshotID  string
	AccountCode string
	After       int64
	Limit       int
}

// TraceTotals is what a set of ledger entries adds up to.
type TraceTotals struct {
	Debit   string `json:"debit_total"`
	Credit  string `json:"credit_total"`
	Net     string `json:"net_balance"`
	Entries int    `json:"entry_count"`
}

// TraceSource is the business fact a journal came from.
type TraceSource struct {
	AccountingEventID string   `json:"accounting_event_id"`
	EventType         string   `json:"event_type"`
	SourceSystem      string   `json:"source_system"`
	SourceObjectType  string   `json:"source_object_type"`
	SourceObjectID    string   `json:"source_object_id"`
	SourceEventID     string   `json:"source_event_id"`
	EvidenceRefs      []string `json:"evidence_refs,omitempty"`
}

// TraceEntryRule is the rule version that produced the journal.
type TraceEntryRule struct {
	RuleSetID   string `json:"posting_rule_set_id"`
	Name        string `json:"name,omitempty"`
	Version     string `json:"version"`
	ExecutionID string `json:"posting_execution_id,omitempty"`
}

// SnapshotTraceEntry is one ledger entry behind a snapshot figure and everything it leads back to.
type SnapshotTraceEntry struct {
	EntrySeq         int64           `json:"ledger_entry_seq"`
	JournalID        string          `json:"journal_id"`
	JournalNumber    string          `json:"journal_number,omitempty"`
	JournalType      string          `json:"journal_type"`
	JournalPurpose   string          `json:"journal_purpose,omitempty"`
	PostingDate      string          `json:"posting_date"`
	FiscalPeriod     string          `json:"fiscal_period"`
	LineNo           int             `json:"line_no"`
	Debit            string          `json:"debit_amount"`
	Credit           string          `json:"credit_amount"`
	Description      string          `json:"description,omitempty"`
	RuleLineRef      string          `json:"posting_rule_line_ref,omitempty"`
	TaxDetailRef     string          `json:"tax_detail_ref,omitempty"`
	SourceLineRef    string          `json:"source_line_ref,omitempty"`
	EvidenceBundleID string          `json:"evidence_bundle_id,omitempty"`
	EvidenceRefs     []string        `json:"evidence_refs,omitempty"`
	Source           *TraceSource    `json:"source,omitempty"`
	Rule             *TraceEntryRule `json:"posting_rule,omitempty"`
}

// SnapshotTrace is the lineage of one account of a snapshot.
type SnapshotTrace struct {
	SnapshotID      string       `json:"snapshot_id"`
	LegalEntityID   string       `json:"legal_entity_id"`
	BookID          string       `json:"accounting_book_id"`
	BookCode        string       `json:"book_code"`
	FiscalPeriod    string       `json:"fiscal_period"`
	AsOf            string       `json:"as_of"`
	LedgerWatermark int64        `json:"ledger_watermark"`
	ContentHash     string       `json:"content_hash"`
	AccountCode     string       `json:"account_code"`
	Figure          SnapshotLine `json:"snapshot_figure"`
	Traced          TraceTotals  `json:"traced_total"`
	// Reconciles is true when the entries behind the account add up, debit and credit, to the figure the snapshot holds.
	Reconciles bool                 `json:"reconciles"`
	Entries    []SnapshotTraceEntry `json:"entries"`
	// NextAfter is the entry sequence to continue from; absent on the last page.
	NextAfter int64 `json:"next_after,omitempty"`
}

// SnapshotTraceRepo resolves the lineage of a snapshot account from the ledger.
type SnapshotTraceRepo interface {
	SnapshotTrace(ctx context.Context, tenant string, q SnapshotTraceQuery) (*SnapshotTrace, error)
}
