// BNK-10 FX Exposure persistence — fx_rates only. See migration
// 000005_add_bnk10_fx_rates and internal/domain/bnk10.go's own doc
// comments for why exposure/scenario data is never persisted here.
package store

import (
	"context"
	"errors"

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
