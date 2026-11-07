package domain

import "errors"

// Control population contract types (ZS-CONTROL-001 §9,
// docs/architecture/control-population-contract.md).

// MaxControlPopulationRecords is the contract's hard cap. Above it the source
// answers 422 rather than a truncated page.
const MaxControlPopulationRecords = 200000

// ErrControlPopulationTooLarge is returned when the in-scope population exceeds
// MaxControlPopulationRecords.
var ErrControlPopulationTooLarge = errors.New("control population exceeds the 200000 record cap")

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

// ControlPopulationPage is one keyset page plus the whole-set watermark and totals.
type ControlPopulationPage struct {
	Records        []ControlPopulationRecord
	NextRecordID   string // last record_id of this page when more remain, else ""
	Watermark      string
	DeclaredTotals ControlDeclaredTotals
}

// FXRateObservationsQuery scopes one page of the fx-rate-observations population:
// every fx_rates row of the tenant whose effective date (UTC) lies in the
// calendar month PeriodID (YYYY-MM). FX rates are tenant-level market
// observations, not scoped to a legal entity.
type FXRateObservationsQuery struct {
	PeriodID      string
	Limit         int
	AfterRecordID string
}

// BankAccountOwnershipQuery scopes one page of the bank-account-ownership
// population: every bank account of the legal entity, in every status.
type BankAccountOwnershipQuery struct {
	LegalEntityID string
	Limit         int
	AfterRecordID string
}
