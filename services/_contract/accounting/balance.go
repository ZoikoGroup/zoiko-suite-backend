package accounting

import (
	"errors"
	"time"

	"zoiko.io/contract/types"
)

// SnapshotStatus represents the certification state of a balance snapshot.
type SnapshotStatus string

const (
	SnapshotStatusGenerated  SnapshotStatus = "GENERATED"
	SnapshotStatusCertified  SnapshotStatus = "CERTIFIED"
	SnapshotStatusSuperseded SnapshotStatus = "SUPERSEDED"
)

// LedgerBalanceSnapshot represents a signed, reproducible balance snapshot for period-close and reporting use (ACC-SNAP).
// Per ZS-DATA-001 Section 11 & Section 23 Stage 5:
// "Snapshot is not a substitute for journal lines; it is a controlled derived artifact."
type LedgerBalanceSnapshot struct {
	BalanceSnapshotID    types.UUID         `json:"balance_snapshot_id"`
	TenantID             types.UUID         `json:"tenant_id"`
	LedgerID             types.UUID         `json:"ledger_id"`
	FiscalPeriodID       types.UUID         `json:"fiscal_period_id"`
	AccountID            types.UUID         `json:"account_id"`
	AsOfPostingWatermark time.Time          `json:"as_of_posting_watermark"` // Completeness point
	OpeningBalance       types.MoneyDecimal `json:"opening_balance"`
	PeriodDebitTotal     types.MoneyDecimal `json:"period_debit_total"`
	PeriodCreditTotal    types.MoneyDecimal `json:"period_credit_total"`
	ClosingBalance       types.MoneyDecimal `json:"closing_balance"`
	SnapshotHash         string             `json:"snapshot_hash"` // Cryptographic SHA-256 integrity hash
	Status               SnapshotStatus     `json:"status"`
	GeneratedAt          time.Time          `json:"generated_at"`
	CertifiedBy          string             `json:"certified_by,omitempty"`
	CertificationRef     *types.UUID        `json:"certification_ref,omitempty"`
}

// Validate checks snapshot integrity.
func (s LedgerBalanceSnapshot) Validate() error {
	if s.BalanceSnapshotID.IsNil() || s.TenantID.IsNil() || s.LedgerID.IsNil() || s.FiscalPeriodID.IsNil() {
		return errors.New("balance_snapshot requires non-nil IDs")
	}
	if s.SnapshotHash == "" {
		return errors.New("balance_snapshot requires snapshot_hash")
	}
	if s.AsOfPostingWatermark.IsZero() {
		return errors.New("balance_snapshot requires as_of_posting_watermark")
	}
	return nil
}
