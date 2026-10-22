package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
)

// CreateEliminations records a run's per-pair eliminations (migration 000007)
// in one transaction: either every row of the run is written or none is.
func (s *PgStore) CreateEliminations(ctx context.Context, rows []domain.Elimination) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if len(rows) == 0 {
		return nil
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		for _, e := range rows {
			accounts, err := json.Marshal(e.IntercompanyAccountCodes)
			if err != nil {
				return err
			}
			lines := e.Lines
			if lines == nil {
				lines = []domain.EliminatedLine{}
			}
			linesJSON, err := json.Marshal(lines)
			if err != nil {
				return err
			}
			var counterparty *string
			if e.CounterpartyAmount != "" {
				v := string(e.CounterpartyAmount)
				counterparty = &v
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO consolidation_eliminations (
					elimination_id, tenant_id, consolidation_run_id, intercompany_entry_id,
					source_legal_entity_id, target_legal_entity_id, fiscal_period, period_basis,
					match_status, source_journal_id, target_journal_id, amount, currency_code,
					counterparty_amount, counterparty_currency_code, intercompany_account_codes,
					outcome, blocked_reason, lines, created_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::numeric, $13,
				          $14::numeric, $15, $16, $17, $18, $19, $20)
			`, e.EliminationID, tenantID, e.ConsolidationRunID, e.IntercompanyEntryID,
				e.SourceLegalEntityID, e.TargetLegalEntityID, e.FiscalPeriod, e.PeriodBasis,
				e.MatchStatus, e.SourceJournalID, e.TargetJournalID, string(e.Amount), e.CurrencyCode,
				counterparty, e.CounterpartyCurrencyCode, accounts,
				e.Outcome, e.BlockedReason, linesJSON, e.CreatedAt); err != nil {
				return fmt.Errorf("record elimination of intercompany entry %s: %w", e.IntercompanyEntryID, err)
			}
		}
		return nil
	})
}

// ListEliminationsByRun returns a run's recorded eliminations, as written.
func (s *PgStore) ListEliminationsByRun(ctx context.Context, runID string) ([]domain.Elimination, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	out := []domain.Elimination{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT elimination_id, tenant_id, consolidation_run_id, intercompany_entry_id,
			       source_legal_entity_id, target_legal_entity_id, fiscal_period, period_basis,
			       match_status, source_journal_id, target_journal_id, amount::text, currency_code,
			       counterparty_amount::text, counterparty_currency_code, intercompany_account_codes,
			       outcome, blocked_reason, lines, created_at
			FROM consolidation_eliminations
			WHERE consolidation_run_id = $1 AND tenant_id = $2
			ORDER BY intercompany_entry_id ASC
		`, runID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				e                   domain.Elimination
				amount              string
				counterparty        *string
				accounts, linesJSON []byte
			)
			if err := rows.Scan(&e.EliminationID, &e.TenantID, &e.ConsolidationRunID, &e.IntercompanyEntryID,
				&e.SourceLegalEntityID, &e.TargetLegalEntityID, &e.FiscalPeriod, &e.PeriodBasis,
				&e.MatchStatus, &e.SourceJournalID, &e.TargetJournalID, &amount, &e.CurrencyCode,
				&counterparty, &e.CounterpartyCurrencyCode, &accounts,
				&e.Outcome, &e.BlockedReason, &linesJSON, &e.CreatedAt); err != nil {
				return err
			}
			if e.Amount, err = canonicalDecimal(amount); err != nil {
				return err
			}
			if counterparty != nil {
				if e.CounterpartyAmount, err = canonicalDecimal(*counterparty); err != nil {
					return err
				}
			}
			if err := json.Unmarshal(accounts, &e.IntercompanyAccountCodes); err != nil {
				return err
			}
			if err := json.Unmarshal(linesJSON, &e.Lines); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// canonicalDecimal renders a NUMERIC's text ("500.0000") in the canonical
// form the service serves amounts in ("500").
func canonicalDecimal(s string) (domain.Decimal, error) {
	r, err := domain.ParseAmount(s)
	if err != nil {
		return "", err
	}
	return domain.Decimal(domain.CanonicalAmount(r)), nil
}
