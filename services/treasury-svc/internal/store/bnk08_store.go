// BNK-08 Cash Position persistence — see migration
// 000008_add_bnk08_cash_position_snapshot for the schema this operates on.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/treasury-svc/internal/domain"
)

const cashPositionColumns = `
	snapshot_id, tenant_id, legal_entity_id, reporting_currency, as_of_timestamp,
	bank_balance, restricted_amount, pending_ap_commitments, payroll_obligations, tax_liabilities, available_cash,
	fx_rate_version, account_breakdown,
	status, has_stale_component, published_by_principal_id, published_at, superseded_by,
	calculated_by_principal_id, correlation_id, created_at`

func scanCashPosition(row pgx.Row, s *domain.CashPositionSnapshot) error {
	var breakdown []byte
	if err := row.Scan(&s.SnapshotID, &s.TenantID, &s.LegalEntityID, &s.ReportingCurrency, &s.AsOfTimestamp,
		&s.BankBalance, &s.RestrictedAmount, &s.PendingAPCommitments, &s.PayrollObligations, &s.TaxLiabilities, &s.AvailableCash,
		&s.FXRateVersion, &breakdown,
		&s.Status, &s.HasStaleComponent, &s.PublishedByPrincipalID, &s.PublishedAt, &s.SupersededBy,
		&s.CalculatedByPrincipalID, &s.CorrelationID, &s.CreatedAt,
	); err != nil {
		return err
	}
	if len(breakdown) > 0 {
		if err := json.Unmarshal(breakdown, &s.AccountBreakdown); err != nil {
			return err
		}
	}
	// Staleness and EffectiveStatus are derived on every read, never stored
	// — see domain.CashPositionStale. The stored has_stale_component is the
	// calculation's own verdict; aging is applied here.
	s.HasStaleComponent = domain.CashPositionStale(s.HasStaleComponent, s.AccountBreakdown, time.Now())
	s.EffectiveStatus = s.Status
	if s.Status == domain.CashPositionPublished && s.HasStaleComponent {
		s.EffectiveStatus = "STALE"
	}
	return nil
}

// CreateCashPositionSnapshot is CalculateCashPosition's real entry
// point — a CALCULATED row, immutable from the moment it's written (see
// migration 000008's own trigger).
func (s *PgStore) CreateCashPositionSnapshot(ctx context.Context, p domain.CalculateCashPositionParams, calc domain.CashPositionCalculation) (*domain.CashPositionSnapshot, error) {
	return s.RefreshCashPositionSnapshot(ctx, domain.RefreshCashPositionParams{CalculateCashPositionParams: p}, calc)
}

// RefreshCashPositionSnapshot writes a new CALCULATED snapshot and, when
// PriorSnapshotID is set, supersedes the prior one with it in the same
// transaction. Before, refresh created the new snapshot and superseded the
// prior one in two transactions: a failed supersede returned an error
// while the new snapshot stayed, and a retry created another. The prior
// snapshot must be a live one for the same legal entity and reporting
// currency, so a refresh authorized for one entity cannot retire
// another's.
func (s *PgStore) RefreshCashPositionSnapshot(ctx context.Context, p domain.RefreshCashPositionParams, calc domain.CashPositionCalculation) (*domain.CashPositionSnapshot, error) {
	breakdown, err := json.Marshal(calc.AccountBreakdown)
	if err != nil {
		return nil, err
	}
	var snap domain.CashPositionSnapshot
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		if err := s.insertCashPositionSnapshot(ctx, tx, p.CalculateCashPositionParams, calc, breakdown, &snap); err != nil {
			return err
		}
		if p.PriorSnapshotID == "" {
			return nil
		}
		_, err := supersedeCashPosition(ctx, tx, p.TenantID, p.PriorSnapshotID, snap.SnapshotID)
		return err
	})
	if err != nil {
		return nil, cashPositionStoreErr(err)
	}
	return &snap, nil
}

// cashPositionStoreErr passes the BNK-08 sentinels through and wraps
// anything else as a store failure.
func cashPositionStoreErr(err error) error {
	for _, sentinel := range []error{domain.ErrCashPositionSnapshotNotFound, domain.ErrInvalidCashPositionTransition, domain.ErrCashPositionScopeMismatch} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

// insertCashPositionSnapshot inserts one CALCULATED snapshot and enqueues
// its events inside tx.
func (s *PgStore) insertCashPositionSnapshot(ctx context.Context, tx pgx.Tx, p domain.CalculateCashPositionParams, calc domain.CashPositionCalculation, breakdown []byte, snap *domain.CashPositionSnapshot) error {
	row := tx.QueryRow(ctx, `
		INSERT INTO cash_position_snapshots (
			snapshot_id, tenant_id, legal_entity_id, reporting_currency, as_of_timestamp,
			bank_balance, restricted_amount, pending_ap_commitments, payroll_obligations, tax_liabilities, available_cash,
			fx_rate_version, account_breakdown,
			status, has_stale_component, calculated_by_principal_id, correlation_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'CALCULATED',$14,$15,$16)
		RETURNING `+cashPositionColumns,
		uuid.New().String(), p.TenantID, p.LegalEntityID, p.ReportingCurrency, calc.AsOfTimestamp,
		calc.BankBalance, p.RestrictedAmount, calc.PendingAPCommitments, calc.PayrollObligations, calc.TaxLiabilities, calc.AvailableCash,
		calc.FXRateVersion, breakdown,
		calc.HasStaleComponent, p.ActorPrincipalID, p.CorrelationID,
	)
	if err := scanCashPosition(row, snap); err != nil {
		return err
	}
	// Enqueue cash_position.calculated event
	if err := s.enqueueCashPositionEvent(ctx, tx, "calculated", "cash_position.calculated", p.TenantID, p.LegalEntityID, snap.SnapshotID, p.ActorPrincipalID, p.CorrelationID, map[string]any{
		"snapshot_id":         snap.SnapshotID,
		"tenant_id":           snap.TenantID,
		"legal_entity_id":     snap.LegalEntityID,
		"reporting_currency":  snap.ReportingCurrency,
		"status":              snap.Status,
		"has_stale_component": snap.HasStaleComponent,
	}); err != nil {
		return err
	}
	// Enqueue cash_position.became_stale event if HasStaleComponent is true
	if snap.HasStaleComponent {
		return s.enqueueCashPositionEvent(ctx, tx, "became-stale", "cash_position.became_stale", p.TenantID, p.LegalEntityID, snap.SnapshotID, p.ActorPrincipalID, p.CorrelationID, map[string]any{
			"snapshot_id":        snap.SnapshotID,
			"tenant_id":          snap.TenantID,
			"legal_entity_id":    snap.LegalEntityID,
			"reporting_currency": snap.ReportingCurrency,
		})
	}
	return nil
}

func (s *PgStore) GetCashPositionSnapshot(ctx context.Context, tenantID, snapshotID string) (*domain.CashPositionSnapshot, error) {
	var snap domain.CashPositionSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+cashPositionColumns+` FROM cash_position_snapshots WHERE snapshot_id=$1 AND tenant_id=$2`, snapshotID, tenantID)
		return scanCashPosition(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCashPositionSnapshotNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

// GetLatestCashPosition is GetCashPosition's real query — the most
// recently calculated snapshot for a legal entity + reporting currency,
// regardless of status (a caller that only wants PUBLISHED ones filters
// client-side, or calls GetCashPositionAsOf — this is the "current state
// of the world" read).
func (s *PgStore) GetLatestCashPosition(ctx context.Context, tenantID, legalEntityID, reportingCurrency string) (*domain.CashPositionSnapshot, error) {
	var snap domain.CashPositionSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+cashPositionColumns+` FROM cash_position_snapshots
			WHERE tenant_id=$1 AND legal_entity_id=$2 AND reporting_currency=$3
			ORDER BY created_at DESC LIMIT 1`,
			tenantID, legalEntityID, reportingCurrency)
		return scanCashPosition(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

// GetCashPositionAsOf mirrors BNK-01's GetBankAccountAsOf idiom: the
// latest snapshot whose created_at is not after asOf — historical
// reconstruction, not a live recompute.
func (s *PgStore) GetCashPositionAsOf(ctx context.Context, tenantID, legalEntityID, reportingCurrency string, asOf time.Time) (*domain.CashPositionSnapshot, error) {
	var snap domain.CashPositionSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+cashPositionColumns+` FROM cash_position_snapshots
			WHERE tenant_id=$1 AND legal_entity_id=$2 AND reporting_currency=$3 AND created_at <= $4
			ORDER BY created_at DESC LIMIT 1`,
			tenantID, legalEntityID, reportingCurrency, asOf)
		return scanCashPosition(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

// ListCurrencyBreakdown is GetCurrencyBreakdown's real query: the latest
// snapshot on file for EACH reporting currency a legal entity has ever
// calculated one in — a cross-currency view of the entity's position,
// not a deeper new capability.
func (s *PgStore) ListCurrencyBreakdown(ctx context.Context, tenantID, legalEntityID string) ([]domain.CashPositionSnapshot, error) {
	var out []domain.CashPositionSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (reporting_currency) `+cashPositionColumns+`
			FROM cash_position_snapshots
			WHERE tenant_id=$1 AND legal_entity_id=$2
			ORDER BY reporting_currency, created_at DESC`,
			tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var snap domain.CashPositionSnapshot
			if err := scanCashPosition(rows, &snap); err != nil {
				return err
			}
			out = append(out, snap)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// PublishCashPositionSnapshot: CALCULATED -> PUBLISHED. Refuses a
// snapshot with a stale component — the doc's own "never silently carry
// forward old bank balance as current" rule, enforced at the one command
// that marks a snapshot authoritative.
func (s *PgStore) PublishCashPositionSnapshot(ctx context.Context, p domain.PublishCashPositionParams) (*domain.CashPositionSnapshot, error) {
	existing, err := s.GetCashPositionSnapshot(ctx, p.TenantID, p.SnapshotID)
	if err != nil {
		return nil, err
	}
	if !domain.CanPublishCashPosition(existing.Status) {
		return nil, domain.ErrInvalidCashPositionTransition
	}
	if existing.HasStaleComponent {
		return nil, domain.ErrCashPositionStaleCannotPublish
	}
	var snap domain.CashPositionSnapshot
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE cash_position_snapshots SET status='PUBLISHED', published_by_principal_id=$3, published_at=now()
			WHERE snapshot_id=$1 AND tenant_id=$2 AND status='CALCULATED'
			RETURNING `+cashPositionColumns,
			p.SnapshotID, p.TenantID, p.ActorPrincipalID)
		if err := scanCashPosition(row, &snap); err != nil {
			return err
		}
		// Enqueue cash_position.published event
		return s.enqueueCashPositionEvent(ctx, tx, "published", "cash_position.published", p.TenantID, snap.LegalEntityID, p.SnapshotID, p.ActorPrincipalID, "", map[string]any{
			"snapshot_id":        snap.SnapshotID,
			"tenant_id":          snap.TenantID,
			"legal_entity_id":    snap.LegalEntityID,
			"reporting_currency": snap.ReportingCurrency,
			"status":             snap.Status,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidCashPositionTransition
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

// SupersedeCashPositionSnapshot marks an existing, non-SUPERSEDED
// snapshot SUPERSEDED, pointing at newSnapshotID — the one-time-set
// superseded_by write migration 000008's trigger allows.
func (s *PgStore) SupersedeCashPositionSnapshot(ctx context.Context, p domain.SupersedeCashPositionParams) (*domain.CashPositionSnapshot, error) {
	var snap *domain.CashPositionSnapshot
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		snap, err = supersedeCashPosition(ctx, tx, p.TenantID, p.SnapshotID, p.NewSnapshotID)
		return err
	})
	if err != nil {
		return nil, cashPositionStoreErr(err)
	}
	return snap, nil
}

// supersedeCashPosition retires snapshotID in favour of newSnapshotID
// inside tx. The old snapshot is locked first; the replacement must be a
// different, live snapshot for the same legal entity and reporting
// currency. Before, any snapshot ID — another entity's, or the snapshot
// itself — was accepted as the replacement.
func supersedeCashPosition(ctx context.Context, tx pgx.Tx, tenantID, snapshotID, newSnapshotID string) (*domain.CashPositionSnapshot, error) {
	var old, repl domain.CashPositionSnapshot
	if err := scanCashPosition(tx.QueryRow(ctx, `SELECT `+cashPositionColumns+` FROM cash_position_snapshots WHERE snapshot_id=$1 AND tenant_id=$2 FOR UPDATE`, snapshotID, tenantID), &old); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCashPositionSnapshotNotFound
		}
		return nil, err
	}
	if !domain.CanSupersedeCashPosition(old.Status) {
		return nil, domain.ErrInvalidCashPositionTransition
	}
	if err := scanCashPosition(tx.QueryRow(ctx, `SELECT `+cashPositionColumns+` FROM cash_position_snapshots WHERE snapshot_id=$1 AND tenant_id=$2`, newSnapshotID, tenantID), &repl); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCashPositionSnapshotNotFound
		}
		return nil, err
	}
	if repl.SnapshotID == old.SnapshotID || repl.Status == domain.CashPositionSuperseded ||
		repl.LegalEntityID != old.LegalEntityID || repl.ReportingCurrency != old.ReportingCurrency {
		return nil, domain.ErrCashPositionScopeMismatch
	}
	var snap domain.CashPositionSnapshot
	err := scanCashPosition(tx.QueryRow(ctx, `
		UPDATE cash_position_snapshots SET status='SUPERSEDED', superseded_by=$3
		WHERE snapshot_id=$1 AND tenant_id=$2 AND status <> 'SUPERSEDED'
		RETURNING `+cashPositionColumns,
		snapshotID, tenantID, newSnapshotID), &snap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidCashPositionTransition
	}
	if err != nil {
		return nil, err
	}
	return &snap, nil
}
