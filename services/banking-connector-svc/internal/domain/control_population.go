package domain

import "errors"

// Control-population contract (docs/architecture/control-population-contract.md).
// financial-control-svc pulls these read-only populations to test bank data
// against the ledger without ever touching this service's database.

const (
	PopulationBankTransactions = "bank-transactions"
	PopulationBankStatements   = "bank-statements"

	// ControlPopulationMaxRecords is the contract's hard cap. Above it the
	// source answers 422, never a truncated page.
	ControlPopulationMaxRecords = 200000
)

// ErrPopulationTooLarge is returned when the in-scope set exceeds
// ControlPopulationMaxRecords.
var ErrPopulationTooLarge = errors.New("control population exceeds the 200000 record cap")

// ControlPopulationQuery is one page request. All scoping fields are
// mandatory except PeriodEnd and BankAccountID.
type ControlPopulationQuery struct {
	Population    string
	TenantID      string
	LegalEntityID string
	// PeriodEnd is the inclusive last day (YYYY-MM-DD) of the period month,
	// or "" for no date cut-off.
	PeriodEnd     string
	BankAccountID string
	// AfterRecordID is the keyset position (exclusive); "" for the first page.
	AfterRecordID string
	Limit         int
}

// ControlRecord is one record of the contract's wire shape. Amount is a
// decimal STRING, never a JSON number.
type ControlRecord struct {
	RecordID   string            `json:"record_id"`
	Reference  string            `json:"reference"`
	Amount     string            `json:"amount"`
	Currency   string            `json:"currency"`
	Date       string            `json:"date"`
	Attributes map[string]string `json:"attributes"`
}

// ControlDeclaredTotals covers the WHOLE in-scope population, not the page.
type ControlDeclaredTotals struct {
	RowCount int               `json:"row_count"`
	Totals   map[string]string `json:"totals"`
}

// ControlPopulationPage is one page plus the whole-set aggregates, all read
// in a single snapshot.
type ControlPopulationPage struct {
	Records        []ControlRecord       `json:"records"`
	NextCursor     string                `json:"next_cursor"`
	Watermark      string                `json:"watermark"`
	DeclaredTotals ControlDeclaredTotals `json:"declared_totals"`
}
