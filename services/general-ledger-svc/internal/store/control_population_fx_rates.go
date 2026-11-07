package store

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// fxRateObservationsScope is the fx-rate-observations control population (ZS-CONTROL-001 FIN-CTRL-026, "FX rate source
// completeness"): the ledger's own sourced, typed FX rates, which revaluation and translation read. One record per
// rate whose rate_type matches and whose rate_date lies in the month; the currency pair is both the reference and the
// group attribute the coverage test uses, and the rate's source is carried so a gap can be attributed to a feed.
//
// amount is the rate (exact NUMERIC), currency is XXX (a rate is not money), and there is no ledger sequence, so
// max_seq is 0.
//
// $1 tenant_id, $2 rate_type, $3 first day of the month, $4 last day of the month.
const fxRateObservationsScope = `
	SELECT f.fx_rate_id::text                              AS record_id,
	       (f.base_currency || '/' || f.quote_currency)    AS reference,
	       f.rate                                          AS amount,
	       'XXX'::text                                     AS currency,
	       f.rate_date                                     AS txn_date,
	       0::bigint                                       AS max_seq,
	       jsonb_build_object(
	           'pair',           f.base_currency || '/' || f.quote_currency,
	           'rate_type',      f.rate_type,
	           'source',         f.source,
	           'source_rate_id', f.source_rate_id,
	           'ingested_by',    f.ingested_by,
	           'override',       f.override::text,
	           'effective_at',   to_char(f.effective_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')) AS attrs
	FROM fx_rates f
	WHERE f.tenant_id = $1::uuid
	  AND f.rate_type = $2::text
	  AND f.rate_date BETWEEN $3::date AND $4::date`

// QueryFXRateObservations returns one page of the fx-rate-observations population.
func (s *PgStore) QueryFXRateObservations(ctx context.Context, tenantID string, q domain.FXRateObservationsQuery) (*domain.ControlPopulationPage, error) {
	first, err := time.Parse("2006-01", q.PeriodID)
	if err != nil {
		return nil, fmt.Errorf("period_id must be YYYY-MM: %w", err)
	}
	last := first.AddDate(0, 1, -1)
	return s.queryGroupedPopulation(ctx, tenantID, groupedPopulationSpec{
		scope:      fxRateObservationsScope,
		scopeArgs:  []any{tenantID, q.RateType, first.Format("2006-01-02"), last.Format("2006-01-02")},
		digestExpr: wave5DigestExpr,
		wmPrefix:   "gl11",
		after:      q.AfterRecordID,
		limit:      q.Limit,
	})
}
