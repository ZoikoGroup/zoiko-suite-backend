package domain

import "time"

// MaxControlPopulationRecords is the hard cap on one extraction. Above it the
// endpoint answers 422 rather than a truncated page.
const MaxControlPopulationRecords = 200000

// ErrPopulationTooLarge is returned when the in-scope population exceeds
// MaxControlPopulationRecords.
var ErrPopulationTooLarge = errorString("control population exceeds the 200000 record cap")

// Control population names served by this service.
const (
	PopulationPaySlips    = "pay-slips"
	PopulationPayrollRuns = "payroll-runs"
)

// Control population measures: which pay figure `amount` carries.
const (
	MeasureGross = "gross"
	MeasureNet   = "net"
)

// ControlPopulationQuery selects one page of a payroll control population.
type ControlPopulationQuery struct {
	TenantID      string
	LegalEntityID string
	Population    string // PopulationPaySlips | PopulationPayrollRuns
	Measure       string // MeasureGross | MeasureNet
	// PeriodStart is the first day of the requested calendar month; nil means no
	// date filter. Payroll populations are FLOW populations: the run pay date must
	// fall in that month exactly.
	PeriodStart *time.Time
	Limit       int
	// AfterRecordID is the keyset position (last record_id of the previous page);
	// empty for the first page.
	AfterRecordID string
}

// ControlRecord is one record of a control population. Amount is a decimal
// string. It carries no employee name or contact detail, ever.
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
