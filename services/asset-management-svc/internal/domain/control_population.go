package domain

import "errors"

// Control population contract types (ZS-CONTROL-001 §9,
// docs/architecture/control-population-contract.md).

// PopulationDepreciationRunPostings is the population FIN-CTRL-011 (depreciation run to GL) reads.
const PopulationDepreciationRunPostings = "depreciation-run-postings"

// MaxControlPopulationRecords is the contract's hard cap. Above it the source answers 422.
const MaxControlPopulationRecords = 200000

// ErrControlPopulationTooLarge is returned when the in-scope population exceeds MaxControlPopulationRecords.
var ErrControlPopulationTooLarge = errors.New("control population exceeds the 200000 record cap")

// ControlPopulationRecord is one record of the wire contract. Amount is a decimal STRING.
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

// ControlPopulationPage is one keyset page plus the whole-set watermark and totals.
type ControlPopulationPage struct {
	Records        []ControlPopulationRecord
	NextRecordID   string // last record_id of this page when more remain, else ""
	Watermark      string
	DeclaredTotals ControlDeclaredTotals
}

// DepreciationRunPostingsQuery scopes one page of the depreciation-run-postings population: one record per
// (live run, asset category) of the legal entity, the unit the run is posted to the ledger in.
type DepreciationRunPostingsQuery struct {
	LegalEntityID string
	// Currency is the currency the run's depreciation is posted in. The register carries none of its own: it is the
	// configured currency of the accounting book the kernel path posts into (clients.WithKernelBook).
	Currency string
	// PeriodID ("YYYY-MM") limits the population to runs of that fiscal period or earlier; empty means no cut-off.
	PeriodID      string
	Limit         int
	AfterRecordID string
}
