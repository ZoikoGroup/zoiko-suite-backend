package store

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/consolidation-svc/internal/domain"
)

// withRLSReadOnlySnapshot is withRLS in a REPEATABLE READ, read-only
// transaction, so every statement inside reads one snapshot.
func (s *PgStore) withRLSReadOnlySnapshot(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// balanceContributionsScope is the in-scope set of the balance-contributions
// population. $1 tenant_id, $2 run_id, $3 child legal entity id. The explicit
// tenant_id predicate is deliberate: it is not left to RLS alone. gross_amount
// is NUMERIC(18,4) and is only ever read as text here — never via float64.
const balanceContributionsScope = `
	WITH scope AS (
		SELECT balance_contribution_id AS rid, balance_contribution_id::text AS record_id,
		       account_code, gross_amount::text AS amount, gross_amount AS amount_num
		FROM balance_contributions
		WHERE tenant_id = $1
		  AND consolidation_run_id = $2::uuid
		  AND source_legal_entity_id = $3
	)`

// BalanceContributionsPopulation returns one keyset page of the child entity's
// contributions to a consolidation run, plus the whole-set watermark and
// declared totals. It returns domain.ErrRunNotFound when the run does not exist
// for the tenant, and domain.ErrPopulationTooLarge above the cap.
//
// Everything runs in ONE REPEATABLE READ read-only transaction, so the run
// lookup, count, digest, totals and page come from a single snapshot.
//
// Watermark = "<count>:<md5 over ordered record_id|account_code|amount>".
// Ordering, the digest order and the cursor all use the uuid column itself
// (not its text form, which a locale collation could order differently).
// The contribution rows record no currency: the currency is the run's
// target_currency and is a label, not evidence of translation.
func (s *PgStore) BalanceContributionsPopulation(ctx context.Context, q domain.BalanceContributionsQuery) (*domain.ControlPopulationPage, error) {
	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}},
	}
	err := s.withRLSReadOnlySnapshot(ctx, q.TenantID, func(tx pgx.Tx) error {
		var currency, status, date string
		err := tx.QueryRow(ctx, `
			SELECT target_currency, status, to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')
			FROM consolidation_runs
			WHERE consolidation_run_id = $1::uuid AND tenant_id = $2`,
			q.RunID, q.TenantID).Scan(&currency, &status, &date)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		if err != nil {
			return err
		}

		var digest, sum string
		if err := tx.QueryRow(ctx, balanceContributionsScope+`
			SELECT count(*),
			       md5(COALESCE(string_agg(record_id || '|' || account_code || '|' || amount,
			                               ',' ORDER BY rid), '')),
			       COALESCE(sum(amount_num), 0)::text
			FROM scope`, q.TenantID, q.RunID, q.LegalEntityID,
		).Scan(&page.DeclaredTotals.RowCount, &digest, &sum); err != nil {
			return err
		}
		if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
			return domain.ErrPopulationTooLarge
		}
		page.Watermark = strconv.FormatInt(page.DeclaredTotals.RowCount, 10) + ":" + digest
		if page.DeclaredTotals.RowCount > 0 {
			page.DeclaredTotals.Totals[currency] = sum
		}

		var after *string
		if q.AfterRecordID != "" {
			after = &q.AfterRecordID
		}
		rows, err := tx.Query(ctx, balanceContributionsScope+`
			SELECT record_id, account_code, amount
			FROM scope
			WHERE ($4::uuid IS NULL OR rid > $4::uuid)
			ORDER BY rid
			LIMIT $5`,
			q.TenantID, q.RunID, q.LegalEntityID, after, q.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ControlRecord
			if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount); err != nil {
				return err
			}
			r.Currency = currency
			r.Date = date
			r.Attributes = map[string]string{
				"consolidation_run_id":   q.RunID,
				"source_legal_entity_id": q.LegalEntityID,
				"currency_basis":         "target_currency_label",
				"run_status":             status,
			}
			page.Records = append(page.Records, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// One row beyond the limit was read only to know whether another page exists.
	if len(page.Records) > q.Limit {
		page.Records = page.Records[:q.Limit]
		page.NextCursor = page.Records[q.Limit-1].RecordID
	}
	return page, nil
}
