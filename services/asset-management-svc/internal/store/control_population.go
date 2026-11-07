package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/asset-management-svc/internal/domain"
)

// depreciationRunPostingsScope is the depreciation-run-postings control population (ZS-CONTROL-001 FIN-CTRL-011,
// "Depreciation run to GL"): the register side of the comparison with the ledger.
//
// The kernel path posts a run as ONE accounting event per asset category, with source_event_id
// 'ast-dep:<run_id>:<category>', and the ledger journal carries that id as its source event, so this population is
// one record per (live run, asset category) with that exact reference. A category whose depreciation rounds to zero is
// not posted by the kernel path, so it is not a record (HAVING > 0).
//
//	live runs   APPROVED or ACCOUNTING_EVENT_EMITTED. A DRAFT/FROZEN/VALIDATED run is not due to post yet; a SUPERSEDED
//	            run's journals were reversed (a superseded run therefore leaves ledger residue that this population
//	            deliberately does not try to net — see the contract doc). An APPROVED run that never emitted is still
//	            here, so the ledger side shows it missing.
//	amount      Σ period_amount of the run's lines whose asset is of the category (exact NUMERIC)
//	currency    the configured book currency ($3); the register has none of its own
//	date        the fiscal period's last day, the document date the kernel path posts with; when the period is not
//	            YYYY-MM the run's creation date, so a run is never dropped for an unusual period name
//
// $1 tenant_id, $2 legal_entity_id, $3 currency, $4 period cut-off (” = none, else YYYY-MM).
const depreciationRunPostingsScope = `
	SELECT (r.run_id::text || ':' || a.asset_category)                          AS record_id,
	       ('ast-dep:' || r.run_id::text || ':' || a.asset_category)            AS reference,
	       SUM(l.period_amount)::numeric(18,2)::text                            AS amount,
	       $3::text                                                             AS currency,
	       CASE WHEN r.fiscal_period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'
	            THEN to_char((to_date(r.fiscal_period || '-01', 'YYYY-MM-DD') + INTERVAL '1 month' - INTERVAL '1 day')::date, 'YYYY-MM-DD')
	            ELSE to_char((r.created_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') END AS txn_date,
	       jsonb_build_object(
	           'run_id',        r.run_id::text,
	           'asset_category', a.asset_category,
	           'run_status',    r.status,
	           'fiscal_period', r.fiscal_period,
	           'line_count',    COUNT(*)::text,
	           'journal_count', jsonb_array_length(r.ledger_journal_ids)::text) AS attrs
	FROM depreciation_runs r
	JOIN depreciation_lines l ON l.run_id = r.run_id AND l.tenant_id = $1::text
	JOIN fixed_assets a       ON a.asset_id = l.asset_id AND a.tenant_id = $1::text
	WHERE r.tenant_id = $1::text
	  AND r.legal_entity_id = $2::text
	  AND r.status IN ('APPROVED', 'ACCOUNTING_EVENT_EMITTED')
	  AND ($4::text = '' OR r.fiscal_period <= $4::text)
	GROUP BY r.run_id, a.asset_category, r.fiscal_period, r.status, r.created_at, r.ledger_journal_ids
	HAVING SUM(l.period_amount) > 0`

const depreciationRunPostingsDigestExpr = `record_id || '|' || reference || '|' || amount || '|' || currency || '|' || txn_date || '|' || attrs::text`

// QueryDepreciationRunPostings returns one keyset page (ordered by record_id, byte order) of the
// depreciation-run-postings population with the whole-set watermark and declared totals, all read in ONE REPEATABLE
// READ read-only transaction.
func (s *PgStore) QueryDepreciationRunPostings(ctx context.Context, tenantID string, q domain.DepreciationRunPostingsQuery) (*domain.ControlPopulationPage, error) {
	var after any
	if q.AfterRecordID != "" {
		after = q.AfterRecordID
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	args := []any{tenantID, q.LegalEntityID, q.Currency, q.PeriodID}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; rollback is the normal exit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlPopulationRecord{},
		DeclaredTotals: domain.ControlDeclaredTotals{Totals: map[string]string{}},
	}

	var digest string
	if err := tx.QueryRow(ctx, `
		WITH scope AS (`+depreciationRunPostingsScope+`)
		SELECT COUNT(*), md5(COALESCE(string_agg(`+depreciationRunPostingsDigestExpr+`, ',' ORDER BY record_id COLLATE "C"), ''))
		FROM scope`, args...).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
		return nil, err
	}
	if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
		return nil, domain.ErrControlPopulationTooLarge
	}
	page.Watermark = fmt.Sprintf("am1:n=%d;md5=%s", page.DeclaredTotals.RowCount, digest)

	totals, err := tx.Query(ctx, `
		WITH scope AS (`+depreciationRunPostingsScope+`)
		SELECT currency, SUM(amount::numeric)::numeric(18,2)::text FROM scope GROUP BY currency ORDER BY currency COLLATE "C"`, args...)
	if err != nil {
		return nil, err
	}
	for totals.Next() {
		var cur, sum string
		if err := totals.Scan(&cur, &sum); err != nil {
			totals.Close()
			return nil, err
		}
		page.DeclaredTotals.Totals[cur] = sum
	}
	totals.Close()
	if err := totals.Err(); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		WITH scope AS (`+depreciationRunPostingsScope+`)
		SELECT record_id, reference, amount, currency, txn_date, attrs
		FROM scope
		WHERE ($5::text IS NULL OR record_id COLLATE "C" > $5::text COLLATE "C")
		ORDER BY record_id COLLATE "C" ASC
		LIMIT $6`, append(args, after, limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlPopulationRecord
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &r.Attributes); err != nil {
			return nil, err
		}
		page.Records = append(page.Records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextRecordID = page.Records[limit-1].RecordID
	}
	return page, nil
}
