package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-control-svc/internal/domain"
)

const manualMatchColumns = `manual_match_id, tenant_id, run_id, side_a_record_ids, side_b_record_ids,
	amount_a::text, amount_b::text, difference::text, currency, reason, evidence_ref, created_by, created_at`

func scanManualMatch(row pgx.Row, m *domain.ManualMatch) error {
	return row.Scan(&m.ManualMatchID, &m.TenantID, &m.RunID, &m.SideARecordIDs, &m.SideBRecordIDs,
		&m.AmountA, &m.AmountB, &m.Difference, &m.Currency, &m.Reason, &m.EvidenceRef, &m.CreatedBy, &m.CreatedAt)
}

// CreateManualMatch records a human-asserted pairing (SPEC_DEVIATIONS A5;
// ZS-CONTROL-001 §10). It never adjusts, resolves or closes anything else —
// callers still use AssignException/ResolveException for that, citing this
// manual match's evidence_ref as their own evidence if they choose to.
//
// The amount difference is computed from the run's own frozen population
// records, compared against the run's pinned tolerance exactly as the
// automatic matching engine does (internal/engine/executor.go): only the
// absolute tolerance is applied, since the engine itself does not yet
// combine a percentage tolerance into execution (that gap is tracked
// separately, not invented here). A difference beyond that tolerance is
// refused outright — recorded nowhere — so a real difference can never be
// silently hidden inside a manual match (§10 "cannot hide amount
// difference").
func (s *PgStore) CreateManualMatch(ctx context.Context, tenantID, runID, actor string, req domain.CreateManualMatchRequest) (*domain.ManualMatch, error) {
	var out domain.ManualMatch
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var toleranceID *string
		var certState string
		if err := tx.QueryRow(ctx, `SELECT tolerance_id, certification_state FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, tenantID, runID).Scan(&toleranceID, &certState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		// Invariant 2/9: a certified run's evidence is sealed. Adding a manual
		// match afterward would introduce a fact outside what was certified.
		if domain.CertificationState(certState) == domain.CertCertified {
			return fmt.Errorf("%w: run is certified; its evidence is sealed and cannot gain a new manual match", domain.ErrInvalidTransition)
		}

		var popA, popB string
		if err := tx.QueryRow(ctx, `SELECT population_id FROM population_snapshots
			WHERE tenant_id = $1 AND run_id = $2 AND side = 'A'`, tenantID, runID).Scan(&popA); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: run has no frozen side A population yet", domain.ErrInvalidArgument)
			}
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT population_id FROM population_snapshots
			WHERE tenant_id = $1 AND run_id = $2 AND side = 'B'`, tenantID, runID).Scan(&popB); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: run has no frozen side B population yet", domain.ErrInvalidArgument)
			}
			return err
		}

		amtA, ccyA, err := sumPopulationRecords(ctx, tx, tenantID, popA, req.SideARecordIDs)
		if err != nil {
			return err
		}
		amtB, ccyB, err := sumPopulationRecords(ctx, tx, tenantID, popB, req.SideBRecordIDs)
		if err != nil {
			return err
		}
		if ccyA != ccyB {
			return fmt.Errorf("%w: side A is %s but side B is %s; no FX is applied without an approved rate basis (§10)",
				domain.ErrInvalidArgument, ccyA, ccyB)
		}

		tol := new(big.Rat)
		if toleranceID != nil {
			var absTol string
			if err := tx.QueryRow(ctx, `SELECT absolute_tolerance::text FROM tolerance_policies
				WHERE tenant_id = $1 AND tolerance_id = $2`, tenantID, *toleranceID).Scan(&absTol); err != nil {
				return err
			}
			if r, ok := new(big.Rat).SetString(absTol); ok {
				tol = r
			}
		}
		diff := new(big.Rat).Sub(amtA, amtB)
		absDiff := new(big.Rat).Abs(diff)
		if absDiff.Cmp(tol) > 0 {
			return fmt.Errorf("%w: amount difference %s exceeds the run's pinned tolerance %s; a manual match cannot hide a difference (§10)",
				domain.ErrInvalidArgument, domain.CanonicalAmount(absDiff), domain.CanonicalAmount(tol))
		}

		return scanManualMatch(tx.QueryRow(ctx, `
			INSERT INTO manual_matches (tenant_id, run_id, side_a_record_ids, side_b_record_ids,
				amount_a, amount_b, difference, currency, reason, evidence_ref, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING `+manualMatchColumns,
			tenantID, runID, nonNilStrings(req.SideARecordIDs), nonNilStrings(req.SideBRecordIDs),
			domain.CanonicalAmount(amtA), domain.CanonicalAmount(amtB), domain.CanonicalAmount(diff), ccyA,
			req.Reason, req.EvidenceRef, actor), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// sumPopulationRecords sums the amounts of exactly the named records within one
// population, failing if any requested id was not found (never silently
// matching against fewer records than asked) or if the found records mix
// currencies (one side of a manual match must be internally consistent; a
// cross-currency comparison is refused separately, by the caller, comparing
// side A's currency against side B's).
func sumPopulationRecords(ctx context.Context, tx pgx.Tx, tenantID, populationID string, recordIDs []string) (*big.Rat, string, error) {
	rows, err := tx.Query(ctx, `SELECT record_id, amount::text, currency FROM population_records
		WHERE tenant_id = $1 AND population_id = $2 AND record_id = ANY($3)`, tenantID, populationID, recordIDs)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	sum := new(big.Rat)
	currency := ""
	found := map[string]bool{}
	for rows.Next() {
		var recordID, amount, ccy string
		if err := rows.Scan(&recordID, &amount, &ccy); err != nil {
			return nil, "", err
		}
		if currency == "" {
			currency = ccy
		} else if currency != ccy {
			return nil, "", fmt.Errorf("%w: records %v mix currencies (%s and %s); a manual match's own side must be single-currency",
				domain.ErrInvalidArgument, recordIDs, currency, ccy)
		}
		amt, ok := new(big.Rat).SetString(amount)
		if !ok {
			return nil, "", fmt.Errorf("%w: record %s has a non-numeric amount", domain.ErrInvalidArgument, recordID)
		}
		sum.Add(sum, amt)
		found[recordID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	for _, id := range recordIDs {
		if !found[id] {
			return nil, "", fmt.Errorf("%w: record id %q not found in this run's frozen population", domain.ErrInvalidArgument, id)
		}
	}
	return sum, currency, nil
}

func (s *PgStore) ListManualMatches(ctx context.Context, tenantID, runID string) ([]domain.ManualMatch, error) {
	var out []domain.ManualMatch
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+manualMatchColumns+` FROM manual_matches
			WHERE tenant_id = $1 AND run_id = $2 ORDER BY created_at`, tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.ManualMatch
			if err := scanManualMatch(rows, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}
