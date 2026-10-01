package domain

import "errors"

// MaxControlPopulationRecords is the control-population contract's hard cap.
// Above it the source answers 422 rather than a truncated page.
const MaxControlPopulationRecords = 200000

// ErrControlPopulationTooLarge is returned when the in-scope population
// exceeds MaxControlPopulationRecords.
var ErrControlPopulationTooLarge = errors.New("control population exceeds the 200000 record cap")

// ControlPopulationRecord is one record of the control-population wire contract.
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

// ControlPopulationPage is one keyset page plus whole-set watermark/totals.
type ControlPopulationPage struct {
	Records        []ControlPopulationRecord
	NextRecordID   string // last record_id of this page when more remain, else ""
	Watermark      string
	DeclaredTotals ControlDeclaredTotals
}

// DestinationChangesQuery scopes one page of the destination-changes
// population. ChangedFrom/ChangedTo are inclusive UTC dates (YYYY-MM-DD).
type DestinationChangesQuery struct {
	LegalEntityID string
	ChangedFrom   string
	ChangedTo     string
	Limit         int
	AfterRecordID string // decoded cursor: last destination_id of the previous page
}
