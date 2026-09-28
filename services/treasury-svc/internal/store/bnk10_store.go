// BNK-10 FX Exposure persistence — fx_rates only. See migration
// 000005_add_bnk10_fx_rates and internal/domain/bnk10.go's own doc
// comments for why exposure/scenario data is never persisted here.
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

// RecordFXRate is BNK-10's only write command — a plain append; there is
// no update path by design (see migration 000005's reject_fx_rate_mutation
// trigger). A correction is simply a new rate with a later EffectiveAt.
func (s *PgStore) RecordFXRate(ctx context.Context, p domain.RecordFXRateParams) (*domain.FXRate, error) {
	var r domain.FXRate
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO fx_rates (tenant_id, currency_pair, rate, effective_at, recorded_by_principal_id)
			VALUES ($1,$2,$3,$4,$5)
			RETURNING rate_id, tenant_id, currency_pair, rate, effective_at, recorded_by_principal_id, created_at`,
			p.TenantID, p.CurrencyPair, p.Rate, p.EffectiveAt, p.RecordedByPrincipalID)
		return row.Scan(&r.RateID, &r.TenantID, &r.CurrencyPair, &r.Rate, &r.EffectiveAt, &r.RecordedByPrincipalID, &r.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetLatestFXRate returns the most recent (by effective_at) rate recorded
// for a currency pair, or domain.ErrFXRateNotFound if none exists yet.
func (s *PgStore) GetLatestFXRate(ctx context.Context, tenantID, currencyPair string) (*domain.FXRate, error) {
	var r domain.FXRate
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT rate_id, tenant_id, currency_pair, rate, effective_at, recorded_by_principal_id, created_at
			FROM fx_rates WHERE tenant_id=$1 AND currency_pair=$2
			ORDER BY effective_at DESC LIMIT 1`,
			tenantID, currencyPair)
		return row.Scan(&r.RateID, &r.TenantID, &r.CurrencyPair, &r.Rate, &r.EffectiveAt, &r.RecordedByPrincipalID, &r.CreatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrFXRateNotFound
		}
		return nil, err
	}
	return &r, nil
}

// ── FXExposureSnapshot (Wave 15) ────────────────────────────────────────────

const fxExposureColumns = `
	snapshot_id, tenant_id, legal_entity_id, exposure_currency, functional_currency, as_of_timestamp,
	rate_used, rate_as_of, rate_version, netting_scope,
	buckets, gross_exposure_amount, net_exposure_amount,
	status, has_stale_component, published_by_principal_id, published_at, superseded_by,
	calculated_by_principal_id, correlation_id, created_at`

func scanFXExposureSnapshot(row pgx.Row, s *domain.FXExposureSnapshot) error {
	var buckets []byte
	if err := row.Scan(&s.SnapshotID, &s.TenantID, &s.LegalEntityID, &s.ExposureCurrency, &s.FunctionalCurrency, &s.AsOfTimestamp,
		&s.RateUsed, &s.RateAsOf, &s.RateVersion, &s.NettingScope,
		&buckets, &s.GrossExposureAmount, &s.NetExposureAmount,
		&s.Status, &s.HasStaleComponent, &s.PublishedByPrincipalID, &s.PublishedAt, &s.SupersededBy,
		&s.CalculatedByPrincipalID, &s.CorrelationID, &s.CreatedAt,
	); err != nil {
		return err
	}
	if len(buckets) > 0 {
		if err := json.Unmarshal(buckets, &s.Buckets); err != nil {
			return err
		}
	}
	s.EffectiveStatus = s.Status
	if s.Status == domain.FXExposurePublished && s.HasStaleComponent {
		s.EffectiveStatus = "STALE"
	}
	return nil
}

// CreateFXExposureSnapshot is CalculateFXExposure's real entry point — a
// CALCULATED row, immutable from the moment it's written (see migration
// 000009's own trigger).
func (s *PgStore) CreateFXExposureSnapshot(ctx context.Context, p domain.CalculateFXExposureParams, calc domain.FXExposureCalculation) (*domain.FXExposureSnapshot, error) {
	buckets, err := json.Marshal(calc.Buckets)
	if err != nil {
		return nil, err
	}
	var snap domain.FXExposureSnapshot
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO fx_exposure_snapshots (
				snapshot_id, tenant_id, legal_entity_id, exposure_currency, functional_currency, as_of_timestamp,
				rate_used, rate_as_of, rate_version, netting_scope,
				buckets, gross_exposure_amount, net_exposure_amount,
				status, has_stale_component, calculated_by_principal_id, correlation_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'CALCULATED',$14,$15,$16)
			RETURNING `+fxExposureColumns,
			uuid.New().String(), p.TenantID, p.LegalEntityID, p.ExposureCurrency, p.FunctionalCurrency, calc.AsOfTimestamp,
			calc.RateUsed, calc.RateAsOf, calc.RateVersion, p.NettingScope,
			buckets, calc.GrossExposureAmount, calc.NetExposureAmount,
			calc.HasStaleComponent, p.ActorPrincipalID, p.CorrelationID,
		)
		return scanFXExposureSnapshot(row, &snap)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

func (s *PgStore) GetFXExposureSnapshot(ctx context.Context, tenantID, snapshotID string) (*domain.FXExposureSnapshot, error) {
	var snap domain.FXExposureSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+fxExposureColumns+` FROM fx_exposure_snapshots WHERE snapshot_id=$1 AND tenant_id=$2`, snapshotID, tenantID)
		return scanFXExposureSnapshot(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFXExposureSnapshotNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

// GetLatestFXExposure is GetFXExposureSnapshot's list-style counterpart —
// the most recently calculated snapshot for a legal entity + currency
// pair, regardless of status.
func (s *PgStore) GetLatestFXExposure(ctx context.Context, tenantID, legalEntityID, exposureCurrency, functionalCurrency string) (*domain.FXExposureSnapshot, error) {
	var snap domain.FXExposureSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+fxExposureColumns+` FROM fx_exposure_snapshots
			WHERE tenant_id=$1 AND legal_entity_id=$2 AND exposure_currency=$3 AND functional_currency=$4
			ORDER BY created_at DESC LIMIT 1`,
			tenantID, legalEntityID, exposureCurrency, functionalCurrency)
		return scanFXExposureSnapshot(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

// GetFXExposureAsOf mirrors BNK-08's GetCashPositionAsOf idiom.
func (s *PgStore) GetFXExposureAsOf(ctx context.Context, tenantID, legalEntityID, exposureCurrency, functionalCurrency string, asOf time.Time) (*domain.FXExposureSnapshot, error) {
	var snap domain.FXExposureSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+fxExposureColumns+` FROM fx_exposure_snapshots
			WHERE tenant_id=$1 AND legal_entity_id=$2 AND exposure_currency=$3 AND functional_currency=$4 AND created_at <= $5
			ORDER BY created_at DESC LIMIT 1`,
			tenantID, legalEntityID, exposureCurrency, functionalCurrency, asOf)
		return scanFXExposureSnapshot(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

// ListFXCurrencyBreakdown is GetCurrencyBreakdown's real query for BNK-10
// — the latest snapshot on file for EACH exposure/functional currency
// pair a legal entity has ever calculated, mirroring BNK-08's own
// ListCurrencyBreakdown exactly (a cross-currency view, not a deeper new
// capability).
func (s *PgStore) ListFXCurrencyBreakdown(ctx context.Context, tenantID, legalEntityID string) ([]domain.FXExposureSnapshot, error) {
	var out []domain.FXExposureSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (exposure_currency, functional_currency) `+fxExposureColumns+`
			FROM fx_exposure_snapshots
			WHERE tenant_id=$1 AND legal_entity_id=$2
			ORDER BY exposure_currency, functional_currency, created_at DESC`,
			tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var snap domain.FXExposureSnapshot
			if err := scanFXExposureSnapshot(rows, &snap); err != nil {
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

// PublishFXExposureSnapshot: CALCULATED -> PUBLISHED. Refuses a snapshot
// calculated from a stale rate — the doc's own "never carry forward
// stale data as current" rule, same as BNK-08's PublishCashPositionSnapshot.
func (s *PgStore) PublishFXExposureSnapshot(ctx context.Context, p domain.PublishFXExposureParams) (*domain.FXExposureSnapshot, error) {
	existing, err := s.GetFXExposureSnapshot(ctx, p.TenantID, p.SnapshotID)
	if err != nil {
		return nil, err
	}
	if !domain.CanPublishFXExposure(existing.Status) {
		return nil, domain.ErrInvalidFXExposureTransition
	}
	if existing.HasStaleComponent {
		return nil, domain.ErrFXExposureStaleCannotPublish
	}
	var snap domain.FXExposureSnapshot
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE fx_exposure_snapshots SET status='PUBLISHED', published_by_principal_id=$3, published_at=now()
			WHERE snapshot_id=$1 AND tenant_id=$2 AND status='CALCULATED'
			RETURNING `+fxExposureColumns,
			p.SnapshotID, p.TenantID, p.ActorPrincipalID)
		return scanFXExposureSnapshot(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidFXExposureTransition
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}

// SupersedeFXExposureSnapshot marks an existing, non-SUPERSEDED snapshot
// SUPERSEDED, pointing at newSnapshotID.
func (s *PgStore) SupersedeFXExposureSnapshot(ctx context.Context, p domain.SupersedeFXExposureParams) (*domain.FXExposureSnapshot, error) {
	existing, err := s.GetFXExposureSnapshot(ctx, p.TenantID, p.SnapshotID)
	if err != nil {
		return nil, err
	}
	if !domain.CanSupersedeFXExposure(existing.Status) {
		return nil, domain.ErrInvalidFXExposureTransition
	}
	var snap domain.FXExposureSnapshot
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE fx_exposure_snapshots SET status='SUPERSEDED', superseded_by=$3
			WHERE snapshot_id=$1 AND tenant_id=$2 AND status <> 'SUPERSEDED'
			RETURNING `+fxExposureColumns,
			p.SnapshotID, p.TenantID, p.NewSnapshotID)
		return scanFXExposureSnapshot(row, &snap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidFXExposureTransition
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &snap, nil
}
