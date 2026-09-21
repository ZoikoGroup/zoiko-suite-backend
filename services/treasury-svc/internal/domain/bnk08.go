package domain

import "time"

// BNK-08 Cash Position. Before this file, GetEffectiveCash
// (internal/handler/handler.go) was a pure live-composition read — the
// exact bank-balance/AP/obligations arithmetic this file's
// CalculateCashPosition reuses — that never persisted anything.
// CashPositionSnapshot is the doc's own entity: a real
// Calculated -> Published -> Superseded lifecycle with its own table,
// mirroring the append-only/superseded-by shape every other
// history/evidence table in this service already uses
// (bank_account_history, bank_account_ownership_evidence).
//
// "Stale" is deliberately NOT a stored status — the doc's own words:
// "stale is derived from source freshness, not operator choice." A
// PUBLISHED snapshot whose bank-balance component has aged past
// BankBalanceStalenessThreshold is reported as stale at READ time
// (GetCashPosition/GetCashPositionAsOf's EffectiveStatus field), never by
// mutating the stored status column.
const (
	CashPositionCalculated = "CALCULATED"
	CashPositionPublished  = "PUBLISHED"
	CashPositionSuperseded = "SUPERSEDED"
)

// CanPublishCashPosition additionally requires !HasStaleComponent at the
// call site (checked in the store, not here, since it needs the row's own
// value) — the doc's "never silently carry forward old bank balance as
// current" rule applied to the one command that marks a snapshot
// authoritative.
func CanPublishCashPosition(status string) bool { return status == CashPositionCalculated }

// CanSupersedeCashPosition: any non-SUPERSEDED snapshot can be superseded
// — a CALCULATED one that turned out wrong, or a PUBLISHED one being
// replaced by a fresher calculation.
func CanSupersedeCashPosition(status string) bool {
	return status == CashPositionCalculated || status == CashPositionPublished
}

// CashPositionAccountLine is one bank account's contribution to a
// snapshot's BankBalance — the real data GetAccountDrilldown reads back,
// captured at calculate time.
type CashPositionAccountLine struct {
	BankAccountID string    `json:"bank_account_id"`
	AccountName   string    `json:"account_name"`
	Balance       float64   `json:"balance"`
	AsOfTimestamp time.Time `json:"as_of_timestamp"`
}

// CashPositionSnapshot is BNK-08's real persisted entity.
type CashPositionSnapshot struct {
	SnapshotID        string    `json:"snapshot_id"`
	TenantID          string    `json:"tenant_id"`
	LegalEntityID     string    `json:"legal_entity_id"`
	ReportingCurrency string    `json:"reporting_currency"`
	AsOfTimestamp     time.Time `json:"as_of_timestamp"`

	BankBalance          float64 `json:"bank_balance"`
	RestrictedAmount     float64 `json:"restricted_amount"`
	PendingAPCommitments float64 `json:"pending_ap_commitments"`
	PayrollObligations   float64 `json:"payroll_obligations"`
	TaxLiabilities       float64 `json:"tax_liabilities"`
	AvailableCash        float64 `json:"available_cash"`

	FXRateVersion    string                     `json:"fx_rate_version,omitempty"`
	AccountBreakdown []CashPositionAccountLine `json:"account_breakdown"`

	Status                 string     `json:"status"`
	// EffectiveStatus is Status, except PUBLISHED becomes "STALE" (never
	// stored) when HasStaleComponent — see this file's own doc comment.
	// Computed by the store on every read, never persisted.
	EffectiveStatus        string     `json:"effective_status"`
	HasStaleComponent      bool       `json:"has_stale_component"`
	PublishedByPrincipalID string     `json:"published_by_principal_id,omitempty"`
	PublishedAt            *time.Time `json:"published_at,omitempty"`
	SupersededBy           *string    `json:"superseded_by,omitempty"`

	CalculatedByPrincipalID string    `json:"calculated_by_principal_id"`
	CorrelationID           string    `json:"correlation_id,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
}

// CalculateCashPositionParams. RestrictedAmount is the one
// configuration-pending input — see this file's package doc: the
// classification SOURCE isn't decided by this wave, only the exclusion
// arithmetic (AvailableCash = BankBalance - RestrictedAmount - AP -
// Payroll - Tax) is real here.
type CalculateCashPositionParams struct {
	TenantID, LegalEntityID, ReportingCurrency string
	RestrictedAmount                           float64
	CorrelationID                              string
	ActorPrincipalID                           string
}

// CashPositionCalculation is the composed result of the exact
// bank-balance/AP/obligations arithmetic GetEffectiveCash already
// performs — computed by the handler (which owns the cross-service
// clients), then handed to the store to persist as a new snapshot row.
type CashPositionCalculation struct {
	BankBalance          float64
	PendingAPCommitments float64
	PayrollObligations   float64
	TaxLiabilities       float64
	AvailableCash        float64
	AccountBreakdown     []CashPositionAccountLine
	HasStaleComponent    bool
	AsOfTimestamp        time.Time
	FXRateVersion        string
}

// RefreshCashPositionParams recalculates and, if PriorSnapshotID is set,
// atomically supersedes that prior snapshot with the new one in the same
// transaction — "refresh" is "calculate again, then retire the old one,"
// not a separate mechanism from Calculate/Supersede.
type RefreshCashPositionParams struct {
	CalculateCashPositionParams
	PriorSnapshotID string
}

type PublishCashPositionParams struct {
	TenantID, SnapshotID, ActorPrincipalID string
}

type SupersedeCashPositionParams struct {
	TenantID, SnapshotID, NewSnapshotID string
}

var (
	ErrCashPositionSnapshotNotFound  = errorString("cash position snapshot not found")
	ErrInvalidCashPositionTransition = errorString("cash position snapshot is not in a state that permits this action")
	// ErrCashPositionStaleCannotPublish is the doc's own rule applied to
	// PublishCashPositionSnapshot: a snapshot with a stale component
	// cannot be marked authoritative — recalculate first.
	ErrCashPositionStaleCannotPublish = errorString("cash position snapshot has a stale component and cannot be published as current")
)
