package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

// ReciprocalBalances is GetReciprocalBalances' read (SPEC_DEVIATIONS A1,
// B10): every intercompany position between one pair of legal entities in one
// fiscal period, grouped by direction
// (source_legal_entity_id, target_legal_entity_id), currency_code and
// match_status, each group summed exactly.
//
// The period is exact: only rows carrying this fiscal_period take part. A
// pair with a NULL fiscal_period (created before migration 000005, or by a
// caller that sends none) belongs to no period and is not attributed to one;
// it is excluded, not guessed at.
//
// The sum happens in Go, with domain.SumAmounts (big.Rat), not in SQL: the
// amounts are read as decimal text (amount::text, ZS-DATA-001 D06) and the
// total must not pass through a type that can round.
func (s *PgStore) ReciprocalBalances(ctx context.Context, entityA, entityB, period string) ([]domain.ReciprocalBalanceGroup, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	out := []domain.ReciprocalBalanceGroup{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT source_legal_entity_id, target_legal_entity_id, currency_code, match_status,
			       array_agg(amount::text ORDER BY intercompany_entry_id)
			FROM intercompany_entries
			WHERE tenant_id = $1
			  AND fiscal_period = $2
			  AND ((source_legal_entity_id = $3 AND target_legal_entity_id = $4)
			    OR (source_legal_entity_id = $4 AND target_legal_entity_id = $3))
			GROUP BY source_legal_entity_id, target_legal_entity_id, currency_code, match_status
			ORDER BY match_status, source_legal_entity_id, target_legal_entity_id, currency_code
		`, tenantID, period, entityA, entityB)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var g domain.ReciprocalBalanceGroup
			var amounts []string
			if err := rows.Scan(&g.SourceLegalEntityID, &g.TargetLegalEntityID, &g.CurrencyCode, &g.MatchStatus, &amounts); err != nil {
				return err
			}
			total, err := domain.SumAmounts(amounts)
			if err != nil {
				return fmt.Errorf("sum reciprocal balance amounts: %w", err)
			}
			amount, err := domain.ExactDecimal(total)
			if err != nil {
				return err
			}
			g.Amount = amount
			out = append(out, g)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
