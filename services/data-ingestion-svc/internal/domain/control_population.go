package domain

import "errors"

// Control population contract types (ZS-CONTROL-001 §9,
// docs/architecture/control-population-contract.md).

// PopulationImportBatchTieout is the population FIN-CTRL-033 (import batch completeness) reads.
const PopulationImportBatchTieout = "import-batch-tieout"

// ErrImportBatchNotFound is returned when the batch does not exist for the tenant and legal entity.
var ErrImportBatchNotFound = errors.New("import batch not found")

// ControlPopulationRecord is one record of the wire contract. Every attribute is a string; counts and totals are
// exact decimal strings, and an absent declared value is the EMPTY string (never a made-up zero).
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

// ControlPopulationPage is one page plus the whole-set watermark and totals.
type ControlPopulationPage struct {
	Records        []ControlPopulationRecord
	NextRecordID   string
	Watermark      string
	DeclaredTotals ControlDeclaredTotals
}

// ImportBatchTieoutQuery scopes the import-batch-tieout population: exactly one batch of one legal entity.
type ImportBatchTieoutQuery struct {
	LegalEntityID string
	BatchID       string
	AfterRecordID string
}
