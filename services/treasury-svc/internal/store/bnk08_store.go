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
	// EffectiveStatus is derived, never stored — see domain.CashPositionSnapshot's
	// own doc comment.
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
	breakdown, err := json.Marshal(calc.AccountBreakdown)
	if err != nil {
		return nil, err
	}
	var snap domain.CashPositionSnapshot
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
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
		return scanCashPosition(row, &snap)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
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
		return scanCashPosition(row, &snap)
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
	existing, err := s.GetCashPositionSnapshot(ctx, p.TenantID, p.SnapshotID)
	if err != nil {
		return nil, err
	}
	if !domain.CanSupersedeCashPosition(existing.Status) {
		return nil, domain.ErrInvalidCashPositionTransition
	}
	var snap domain.CashPositionSnapshot
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE cash_position_snapshots SET status='SUPERSEDED', superseded_by=$3
			WHERE snapshot_id=$1 AND tenant_id=$2 AND status <> 'SUPERSEDED'
			RETURNING `+cashPositionColumns,
			p.SnapshotID, p.TenantID, p.NewSnapshotID)
		return scanCashPosition(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidCashPositionTransition
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}
