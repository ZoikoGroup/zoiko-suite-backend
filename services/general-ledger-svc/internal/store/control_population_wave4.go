package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// Wave 4 control populations — see the "Wave 4 population definitions" section
// of docs/architecture/control-population-contract.md. Both are grouped
// populations whose record_id is composite TEXT, so paging is keyset on that
// exact text expression under COLLATE "C" (byte-wise, locale independent),
// both in ORDER BY and in the cursor comparison.

// journalAccountTotalsScope: $1 tenant_id, $2 legal_entity_id, $3 account codes,
// $4 normal balance. Two-level grouping: per (journal, currency, account) first
// so the distinct-account list can be aggregated in a stable byte order.
const journalAccountTotalsScope = `
	SELECT (t.journal_id::text || ':' || t.currency_code)      AS record_id,
	       t.journal_id::text                                   AS reference,
	       SUM(t.net)                                           AS amount,
	       t.currency_code                                      AS currency,
	       MAX(t.txn_date)                                      AS txn_date,
	       MAX(t.max_seq)                                       AS max_seq,
	       jsonb_build_object(
	           'journal_id',    t.journal_id::text,
	           'fiscal_period', MIN(t.fiscal_period),
	           'account_codes', string_agg(t.account_code, ',' ORDER BY t.account_code COLLATE "C"),
	           'entry_count',   SUM(t.cnt)::text)               AS attrs,
	       string_agg(t.account_code, ',' ORDER BY t.account_code COLLATE "C") AS acct_list,
	       SUM(t.cnt)                                           AS entry_count
	FROM (
		SELECT le.journal_id, le.currency_code, le.account_code,
		       SUM(CASE WHEN $4 = 'DEBIT' THEN le.debit_amount - le.credit_amount
		                ELSE le.credit_amount - le.debit_amount END) AS net,
		       MAX(le.transaction_date) AS txn_date,
		       MAX(le.entry_seq)        AS max_seq,
		       MIN(le.fiscal_period)    AS fiscal_period,
		       COUNT(*)                 AS cnt
		FROM ledger_entries le
		WHERE le.tenant_id = $1::uuid
		  AND le.legal_entity_id = $2::uuid
		  AND le.account_code = ANY($3::text[])
		GROUP BY le.journal_id, le.currency_code, le.account_code
	) t
	GROUP BY t.journal_id, t.currency_code`

// trialBalanceScope: $1 tenant_id, $2 legal_entity_id, $3 fiscal_period. Every
// posted ledger entry of the period — originals of reversed journals and their
// reversals alike. Reads ledger_entries only; it never touches
// trial_balance_snapshots.
const trialBalanceScope = `
	SELECT (le.account_code || ':' || le.currency_code)  AS record_id,
	       le.account_code                               AS reference,
	       SUM(le.debit_amount - le.credit_amount)       AS amount,
	       le.currency_code                              AS currency,
	       MAX(le.transaction_date)                      AS txn_date,
	       MAX(le.entry_seq)                             AS max_seq,
	       jsonb_build_object(
	           'account_code',  le.account_code,
	           'fiscal_period', $3::text,
	           'entry_count',   COUNT(*)::text,
	           'max_entry_seq', MAX(le.entry_seq)::text) AS attrs,
	       COUNT(*)                                      AS entry_count
	FROM ledger_entries le
	WHERE le.tenant_id = $1::uuid
	  AND le.legal_entity_id = $2::uuid
	  AND le.fiscal_period = $3::text
	GROUP BY le.account_code, le.currency_code`

// QueryJournalAccountTotals returns one keyset page of the
// journal-account-totals population with the whole-set watermark and declared
// totals, all from ONE REPEATABLE READ read-only transaction.
func (s *PgStore) QueryJournalAccountTotals(ctx context.Context, tenantID string, q domain.JournalAccountTotalsQuery) (*domain.ControlPopulationPage, error) {
	return s.queryGroupedPopulation(ctx, tenantID, groupedPopulationSpec{
		scope:      journalAccountTotalsScope,
		scopeArgs:  []any{tenantID, q.LegalEntityID, q.AccountCodes, q.NormalBalance},
		digestExpr: `record_id || '|' || amount::text || '|' || currency || '|' || txn_date::text || '|' || entry_count::text || '|' || acct_list`,
		wmPrefix:   "gl3",
		after:      q.AfterRecordID,
		limit:      q.Limit,
	})
}

// QueryTrialBalancePopulation returns one keyset page of the read-only
// trial-balance population.
func (s *PgStore) QueryTrialBalancePopulation(ctx context.Context, tenantID string, q domain.TrialBalancePopulationQuery) (*domain.ControlPopulationPage, error) {
	return s.queryGroupedPopulation(ctx, tenantID, groupedPopulationSpec{
		scope:      trialBalanceScope,
		scopeArgs:  []any{tenantID, q.LegalEntityID, q.FiscalPeriod},
		digestExpr: `record_id || '|' || amount::text || '|' || currency`,
		wmPrefix:   "gl2",
		after:      q.AfterRecordID,
		limit:      q.Limit,
	})
}

type groupedPopulationSpec struct {
	scope      string // yields record_id, reference, amount, currency, txn_date, max_seq, attrs (+ digest inputs)
	scopeArgs  []any
	digestExpr string
	wmPrefix   string
	after      string
	limit      int
}

func (s *PgStore) queryGroupedPopulation(ctx context.Context, tenantID string, sp groupedPopulationSpec) (*domain.ControlPopulationPage, error) {
	var after any
	if sp.after != "" {
		after = sp.after
	}
	limit := sp.limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	n := len(sp.scopeArgs)
	afterPos, limitPos := n+1, n+2

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

	var maxSeq int64
	var digest string
	err = tx.QueryRow(ctx, `
		WITH scope AS (`+sp.scope+`)
		SELECT COUNT(*),
		       COALESCE(MAX(max_seq), 0),
		       md5(COALESCE(string_agg(`+sp.digestExpr+`, ',' ORDER BY record_id COLLATE "C"), ''))
		FROM scope`, sp.scopeArgs...,
	).Scan(&page.DeclaredTotals.RowCount, &maxSeq, &digest)
	if err != nil {
		return nil, mapPgError(err)
	}
	if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
		return nil, domain.ErrControlPopulationTooLarge
	}
	page.Watermark = fmt.Sprintf("%s:n=%d;entry_seq=%d;md5=%s", sp.wmPrefix, page.DeclaredTotals.RowCount, maxSeq, digest)

	totals, err := tx.Query(ctx, `
		WITH scope AS (`+sp.scope+`)
		SELECT currency, SUM(amount)::text FROM scope GROUP BY currency ORDER BY currency COLLATE "C"`, sp.scopeArgs...)
	if err != nil {
		return nil, mapPgError(err)
	}
	for totals.Next() {
		var cur, sum string
		if err := totals.Scan(&cur, &sum); err != nil {
			totals.Close()
			return nil, mapPgError(err)
		}
		page.DeclaredTotals.Totals[cur] = sum
	}
	totals.Close()
	if err := totals.Err(); err != nil {
		return nil, mapPgError(err)
	}

	args := append(append([]any{}, sp.scopeArgs...), after, limit+1)
	rows, err := tx.Query(ctx, `
		WITH scope AS (`+sp.scope+`)
		SELECT record_id, reference, amount::text, currency, to_char(txn_date, 'YYYY-MM-DD'), attrs
		FROM scope
		WHERE ($`+itoa(afterPos)+`::text IS NULL OR record_id COLLATE "C" > $`+itoa(afterPos)+`::text COLLATE "C")
		ORDER BY record_id COLLATE "C" ASC
		LIMIT $`+itoa(limitPos), args...)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlPopulationRecord
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &r.Attributes); err != nil {
			return nil, mapPgError(err)
		}
		page.Records = append(page.Records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgError(err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextRecordID = page.Records[limit-1].RecordID
	}
	return page, nil
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
