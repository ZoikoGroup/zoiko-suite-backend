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

// OldestBankBalanceAsOf is the as-of time of the oldest bank balance a
// snapshot composed — the time the position as a whole is only as fresh
// as. ok is false when it composed none.
func OldestBankBalanceAsOf(lines []CashPositionAccountLine) (oldest time.Time, ok bool) {
	for _, l := range lines {
		if !ok || l.AsOfTimestamp.Before(oldest) {
			oldest, ok = l.AsOfTimestamp, true
		}
	}
	return oldest, ok
}

// CashPositionStale is "stale is derived from source freshness, not
// operator choice", evaluated at now rather than frozen at calculation:
// true when the calculation itself found a stale or missing bank balance,
// or when the oldest bank balance it composed has since aged past
// BankBalanceStalenessThreshold. A snapshot calculated and published from
// fresh balances therefore becomes stale as those balances age, instead of
// being served as current indefinitely.
func CashPositionStale(staleAtCalculation bool, lines []CashPositionAccountLine, now time.Time) bool {
	if staleAtCalculation {
		return true
	}
	oldest, ok := OldestBankBalanceAsOf(lines)
	return ok && now.Sub(oldest) > BankBalanceStalenessThreshold
}

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
	Balance       Decimal   `json:"balance"`
	AsOfTimestamp time.Time `json:"as_of_timestamp"`
}

// CashPositionSnapshot is BNK-08's real persisted entity.
type CashPositionSnapshot struct {
	SnapshotID        string    `json:"snapshot_id"`
	TenantID          string    `json:"tenant_id"`
	LegalEntityID     string    `json:"legal_entity_id"`
	ReportingCurrency string    `json:"reporting_currency"`
	AsOfTimestamp     time.Time `json:"as_of_timestamp"`

	BankBalance          Decimal `json:"bank_balance"`
	RestrictedAmount     Decimal `json:"restricted_amount"`
	PendingAPCommitments Decimal `json:"pending_ap_commitments"`
	PayrollObligations   Decimal `json:"payroll_obligations"`
	TaxLiabilities       Decimal `json:"tax_liabilities"`
	AvailableCash        Decimal `json:"available_cash"`

	FXRateVersion    string                    `json:"fx_rate_version,omitempty"`
	AccountBreakdown []CashPositionAccountLine `json:"account_breakdown"`

	Status string `json:"status"`
	// EffectiveStatus is Status, except PUBLISHED becomes "STALE" (never
	// stored) when HasStaleComponent — see this file's own doc comment.
	// Computed by the store on every read, never persisted.
	EffectiveStatus string `json:"effective_status"`
	// HasStaleComponent is CashPositionStale at the time of the read: the
	// flag stored at calculation, or the composed bank balances having aged
	// past the threshold since.
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
	RestrictedAmount                           Decimal
	CorrelationID                              string
	ActorPrincipalID                           string
}

// CashPositionCalculation is the composed result of the exact
// bank-balance/AP/obligations arithmetic GetEffectiveCash already
// performs — computed by the handler (which owns the cross-service
// clients), then handed to the store to persist as a new snapshot row.
type CashPositionCalculation struct {
	BankBalance          Decimal
	PendingAPCommitments Decimal
	PayrollObligations   Decimal
	TaxLiabilities       Decimal
	AvailableCash        Decimal
	AccountBreakdown     []CashPositionAccountLine
	HasStaleComponent    bool
	AsOfTimestamp        time.Time
	FXRateVersion        string
}

// RefreshCashPositionParams recalculates and, if PriorSnapshotID is set,
// atomically supersedes that prior snapshot with the new one in the same
// transaction — "refresh" is "calculate again, then retire the old one,"
// not a separate mechanism from Calculate/Supersede. The prior snapshot
// must be for the same legal entity and reporting currency.
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
	// ErrCashPositionScopeMismatch: a snapshot can only be superseded by a
	// different, live snapshot for the same legal entity and reporting
	// currency — never by one for another entity, which would let a
	// caller authorized for one entity retire another's position.
	ErrCashPositionScopeMismatch = errorString("a cash position snapshot can only be superseded by another live snapshot for the same legal entity and reporting currency")
)
