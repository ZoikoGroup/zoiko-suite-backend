package domain

import "time"

// Control population contract (ZS-CONTROL-001 §9,
// docs/architecture/control-population-contract.md). general-ledger-svc
// implements the "account-postings" population: one record per posted
// ledger entry on the requested accounts.

// MaxControlPopulationRecords is the contract's hard cap. Above it the source
// answers 422 rather than a truncated page.
const MaxControlPopulationRecords = 200000

// ErrControlPopulationTooLarge is returned when the in-scope population
// exceeds MaxControlPopulationRecords.
var ErrControlPopulationTooLarge = errorString("control population exceeds the 200000 record cap")

// AccountPostingsQuery scopes one page of the account-postings population.
type AccountPostingsQuery struct {
	LegalEntityID string
	// PeriodEnd is the "YYYY-MM-DD" last day of period_id's month; empty
	// means no date cut-off.
	PeriodEnd     string
	AccountCodes  []string
	NormalBalance string // DEBIT or CREDIT
	Limit         int
	// AfterRecordID is the decoded cursor: the last journal_line_id of the
	// previous page. Empty for the first page.
	AfterRecordID string
}

// ControlPopulationRecord is one record of the wire contract.
type ControlPopulationRecord struct {
	RecordID   string            `json:"record_id"`
	Reference  string            `json:"reference"`
	Amount     string            `json:"amount"`
	Currency   string            `json:"currency"`
	Date       string            `json:"date"`
	Attributes map[string]string `json:"attributes"`
}

// ControlDeclaredTotals covers the WHOLE in-scope population, not the page.
type ControlDeclaredTotals struct {
	RowCount int64             `json:"row_count"`
	Totals   map[string]string `json:"totals"`
}

// AccountPostingsPage is one page plus the whole-set watermark and totals,
// all read in the same snapshot.
type AccountPostingsPage struct {
	Records        []ControlPopulationRecord
	NextRecordID   string // last record_id of this page when more remain, else ""
	Watermark      string
	DeclaredTotals ControlDeclaredTotals
}

// ControlPopulationPage is the page shape shared by every general-ledger
// population (account-postings, journal-account-totals, trial-balance).
type ControlPopulationPage = AccountPostingsPage

// JournalAccountTotalsQuery scopes one page of the journal-account-totals
// population: posted ledger entries on AccountCodes grouped by journal and
// currency. There is no period cut-off (a lifetime population).
type JournalAccountTotalsQuery struct {
	LegalEntityID string
	AccountCodes  []string
	NormalBalance string // DEBIT or CREDIT
	Limit         int
	// AfterRecordID is the decoded cursor: the last "<journal_id>:<currency>"
	// record_id of the previous page. Empty for the first page.
	AfterRecordID string
}

// TrialBalancePopulationQuery scopes one page of the read-only trial-balance
// population: every posted ledger entry of FiscalPeriod, grouped by account and
// currency.
type TrialBalancePopulationQuery struct {
	LegalEntityID string
	FiscalPeriod  string // exact text, not parsed
	Limit         int
	// AfterRecordID is the last "<account_code>:<currency>" record_id of the
	// previous page. Empty for the first page.
	AfterRecordID string
}

// FiscalPeriodPopulationQuery scopes one page of a population defined purely
// by an exact fiscal_period text match: journal-balances,
// control-account-postings and manual-journals.
type FiscalPeriodPopulationQuery struct {
	LegalEntityID string
	FiscalPeriod  string // exact text, not parsed
	Limit         int
	// AfterRecordID is the decoded cursor: the last record_id of the previous
	// page. Empty for the first page.
	AfterRecordID string
}

// UnpostedEventsQuery scopes one page of the unposted-events population:
// posting executions not COMMITTED and created strictly before CreatedBefore.
type UnpostedEventsQuery struct {
	LegalEntityID string
	// CreatedBefore is an absolute instant (UTC). age_days is computed from it,
	// never from the wall clock.
	CreatedBefore time.Time
	Limit         int
	AfterRecordID string
}

// EventJournalBreaksQuery scopes one page of the event-journal-breaks
// population: accounting-event/journal linkage breaks strictly before
// CreatedBefore.
type EventJournalBreaksQuery struct {
	LegalEntityID string
	CreatedBefore time.Time
	Limit         int
	AfterRecordID string
}
