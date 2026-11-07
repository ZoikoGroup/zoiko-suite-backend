package domain

import (
	"errors"
	"time"
)

// Control population contract types (ZS-CONTROL-001 §9,
// docs/architecture/control-population-contract.md).

// PopulationPaymentAttempts is the population FIN-CTRL-004 (payment provider settlement) reads.
const PopulationPaymentAttempts = "payment-attempts"

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

// PaymentAttemptsQuery scopes one page of the payment-attempts population: every payment initiation
// attempt of the legal entity, in every status, whose execution date is on or before CutOff.
type PaymentAttemptsQuery struct {
	LegalEntityID string
	// CutOff is the last day of the requested period; nil means no date cut-off.
	CutOff        *time.Time
	Limit         int
	AfterRecordID string
}
