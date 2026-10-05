package domain

// Control population contract (docs/architecture/control-population-contract.md).

// MigrationBatchTieoutQuery selects the migration-batch-tieout population: exactly
// one record for one batch.
type MigrationBatchTieoutQuery struct {
	TenantID      string
	LegalEntityID string
	BatchID       string
	Limit         int
	// AfterRecordID is the keyset position (last record_id of the previous page).
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

// ControlPopulationPage is a page plus the whole-set watermark and totals.
type ControlPopulationPage struct {
	Records        []ControlRecord `json:"records"`
	NextCursor     string          `json:"next_cursor"`
	Watermark      string          `json:"watermark"`
	DeclaredTotals DeclaredTotals  `json:"declared_totals"`
}
