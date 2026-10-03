package domain

// Control population contract (docs/architecture/control-population-contract.md).
// financial-control-svc asks this service for the records behind a financial
// population instead of trusting a headline total.

// PopulationOpenInvoices is the one population accounts-payable-svc serves.
const PopulationOpenInvoices = "open-invoices"

// MaxControlPopulationRecords is the contract's hard cap. Above it the source
// answers 422 rather than truncating.
const MaxControlPopulationRecords = 200000

var (
	// ErrPopulationTooLarge: the in-scope population exceeds the hard cap.
	ErrPopulationTooLarge = errorString("control population exceeds the 200000 record cap")
	// ErrInvalidCursor: the cursor is not one this service issued.
	ErrInvalidCursor = errorString("cursor is not valid")
)

// ControlPopulationQuery is a validated request for one page of a population.
type ControlPopulationQuery struct {
	TenantID      string
	LegalEntityID string
	// PeriodEnd is the inclusive created_at cut-off date (YYYY-MM-DD), or ""
	// for no cut-off.
	PeriodEnd string
	Limit     int
	// AfterRecordID is the decoded cursor: the last record_id already served.
	AfterRecordID string
}

// ControlRecord is one record of a control population.
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
	RowCount int               `json:"row_count"`
	Totals   map[string]string `json:"totals"`
}

// ControlPopulationPage is one page plus whole-set aggregates, all read from
// one database snapshot.
type ControlPopulationPage struct {
	Records []ControlRecord `json:"records"`
	// NextRecordID is the last record_id on the page when more remain, else "".
	NextRecordID   string         `json:"-"`
	Watermark      string         `json:"watermark"`
	DeclaredTotals DeclaredTotals `json:"declared_totals"`
}
