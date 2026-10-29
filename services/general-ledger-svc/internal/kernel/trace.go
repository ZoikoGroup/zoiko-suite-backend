package kernel

import (
	"encoding/json"
	"time"
)

// Trace is the lineage of a posting (standard §31.1, GetPostingTrace): from the journal back through the execution and the
// rule version to the accounting event and its source, and forward to the reversals and corrections that relate to it. One
// accounting event can have a posting in several books, so the trace holds one entry per posting.
type Trace struct {
	Event *AccountingEvent `json:"accounting_event,omitempty"`
	// SupersededBy lists the correction events that name this event as the one they correct.
	SupersededBy []string       `json:"superseded_by_event_ids,omitempty"`
	Postings     []TracePosting `json:"postings"`
}

// TracePosting is one consequence of an event: the book, the rule version that produced it, the execution, the journals
// (the posting itself and anything that reverses or corrects it, in order) and the attempts that failed first.
type TracePosting struct {
	Book      *TraceBook      `json:"book,omitempty"`
	Basis     string          `json:"posting_basis,omitempty"`
	Rule      *TraceRule      `json:"posting_rule,omitempty"`
	Execution *TraceExecution `json:"posting_execution,omitempty"`
	Journals  []TraceJournal  `json:"journals"`
	Relations []TraceRelation `json:"relations,omitempty"`
	Failures  []TraceFailure  `json:"failed_attempts,omitempty"`
}

type TraceBook struct {
	BookID             string `json:"book_id"`
	BookCode           string `json:"book_code"`
	BookType           string `json:"book_type"`
	Framework          string `json:"accounting_framework_id"`
	FunctionalCurrency string `json:"functional_currency"`
}

type TraceRule struct {
	RuleSetID   string `json:"posting_rule_set_id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
	TestPackID  string `json:"test_pack_id"`
	Status      string `json:"status"`
}

type TraceExecution struct {
	ExecutionID     string          `json:"posting_execution_id"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	ProposalHash    string          `json:"proposal_hash,omitempty"`
	LedgerWatermark *int64          `json:"ledger_watermark,omitempty"`
	OutboxEventID   string          `json:"journal_event_id,omitempty"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
	CommittedAt     *time.Time      `json:"committed_at,omitempty"`
	Calculation     json.RawMessage `json:"calculation_trace,omitempty"`
}

type TraceJournal struct {
	Role           string      `json:"role"` // POSTING, REVERSAL, CORRECTION
	JournalID      string      `json:"journal_id"`
	JournalNumber  string      `json:"journal_number,omitempty"`
	Status         string      `json:"status"`
	JournalType    string      `json:"journal_type"`
	PostingDate    string      `json:"posting_date"`
	FiscalPeriod   string      `json:"fiscal_period"`
	Currency       string      `json:"currency_code"`
	Hash           string      `json:"journal_hash,omitempty"`
	EvidenceBundle string      `json:"evidence_bundle_id,omitempty"`
	PostedBy       string      `json:"posted_by,omitempty"`
	ReversalOf     string      `json:"reversal_of_journal_id,omitempty"`
	CorrectionOf   string      `json:"correction_of_journal_id,omitempty"`
	Reason         string      `json:"reversal_reason,omitempty"`
	CorrectionKnd  string      `json:"correction_kind,omitempty"`
	Lines          []TraceLine `json:"lines"`
}

type TraceLine struct {
	LineNo         int               `json:"line_no"`
	AccountCode    string            `json:"account_code"`
	Debit          string            `json:"debit_amount"`
	Credit         string            `json:"credit_amount"`
	TransactionAmt string            `json:"transaction_amount,omitempty"`
	TransactionCcy string            `json:"transaction_currency,omitempty"`
	ExchangeRate   string            `json:"exchange_rate,omitempty"`
	RateRef        string            `json:"rate_ref,omitempty"`
	RuleLineRef    string            `json:"posting_rule_line_ref,omitempty"`
	TaxDetailRef   string            `json:"tax_detail_ref,omitempty"`
	Dimensions     map[string]string `json:"dimensions,omitempty"`
	LedgerEntrySeq *int64            `json:"ledger_entry_seq,omitempty"`
	Description    string            `json:"description,omitempty"`
}

type TraceFailure struct {
	ExecutionID string    `json:"posting_execution_id"`
	Status      string    `json:"status"`
	Reason      string    `json:"failure_reason"`
	At          time.Time `json:"at"`
}

type TraceRelation struct {
	Kind string `json:"kind"` // REVERSES, CORRECTS
	From string `json:"from_journal_id"`
	To   string `json:"to_journal_id"`
}
