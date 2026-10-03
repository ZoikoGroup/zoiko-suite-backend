package domain

// Control-population contract (docs/architecture/control-population-contract.md).

const (
	// PopulationEntryLegs is the one population this service serves.
	PopulationEntryLegs = "entry-legs"

	LegSource = "source"
	LegTarget = "target"

	// ControlPopulationMaxRecords is the hard cap; above it the source answers 422.
	ControlPopulationMaxRecords = 200000

	// ErrPopulationTooLarge means the in-scope population exceeds the cap.
	ErrPopulationTooLarge = errorString("control population exceeds the 200000 record cap")
)

// ControlPopulationQuery is a validated request for one page of a population.
type ControlPopulationQuery struct {
	TenantID      string
	LegalEntityID string
	Leg           string // LegSource | LegTarget
	AfterRecordID string // raw (decoded) cursor; "" = first page
	Limit         int
}

// ControlRecord is one record. Amount is exact NUMERIC text, never a float.
type ControlRecord struct {
	RecordID   string            `json:"record_id"`
	Reference  string            `json:"reference"`
	Amount     string            `json:"amount"`
	Currency   string            `json:"currency"`
	Date       string            `json:"date"`
	Attributes map[string]string `json:"attributes"`
}

type ControlDeclaredTotals struct {
	RowCount int               `json:"row_count"`
	Totals   map[string]string `json:"totals"`
}

type ControlPopulationPage struct {
	Records        []ControlRecord       `json:"records"`
	NextCursor     string                `json:"next_cursor"`
	Watermark      string                `json:"watermark"`
	DeclaredTotals ControlDeclaredTotals `json:"declared_totals"`
}
