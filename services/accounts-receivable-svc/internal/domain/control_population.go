package domain

import "time"

// Control population contract (docs/architecture/control-population-contract.md).

// MaxControlPopulationRecords is the hard cap on one extraction. Above it the
// endpoint answers 422 rather than a truncated page.
const MaxControlPopulationRecords = 200000

// ErrPopulationTooLarge is returned when the in-scope population exceeds
// MaxControlPopulationRecords.
var ErrPopulationTooLarge = errorString("control population exceeds the 200000 record cap")

// ControlPopulationQuery selects one page of the open-invoices population.
type ControlPopulationQuery struct {
	TenantID      string
	LegalEntityID string
	// CutOff is the last day of the requested period; nil means no date cut-off.
	CutOff *time.Time
	Limit  int
	// AfterRecordID is the keyset position (last record_id of the previous page);
	// empty for the first page.
	AfterRecordID string
}

// ControlRecord is one record of a control population. Amount is a decimal string.
type ControlRecord struct {
	RecordID   string            `json:"record_id"`
	Reference  string            `json:"reference"`
	Amount     string            `json:"amount"`
	Currency   string            `json:"currency"`
	Date       string            `json:"date"`
	Attributes map[string]string `json:"attributes"`
}

// DeclaredTotals covers the WHOLE in-scope population, not the page.
type DeclaredTotals struct {
	RowCount int64             `json:"row_count"`
	Totals   map[string]string `json:"totals"`
}

// ControlPopulationPage is a page plus the whole-set watermark and totals, all
// read from one snapshot.
type ControlPopulationPage struct {
	Records        []ControlRecord `json:"records"`
	NextCursor     string          `json:"next_cursor"`
	Watermark      string          `json:"watermark"`
	DeclaredTotals DeclaredTotals  `json:"declared_totals"`
}
