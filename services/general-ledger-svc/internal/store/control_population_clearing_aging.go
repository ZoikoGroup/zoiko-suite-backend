package store

import (
	"context"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// clearingBalanceAgingScope is the clearing-balance-aging control population
// (ZS-CONTROL-001 FIN-CTRL-031, "Suspense account aging").
//
// Clearing and suspense accounts are the chart accounts whose control_type is
// CLEARING; the type is copied onto each ledger entry when it is posted, so a
// later change to the chart cannot move history. There is no item-level clearing
// in the ledger, so the age of the open balance is derived by FIFO: later
// postings in the opposite direction clear the OLDEST open amounts first, which
// means the youngest part of an open balance is whatever was added in the same
// direction on or after the cut-off. Everything else is aged.
//
//	net     = Σ(debit − credit) over all posted entries of the account/currency/book
//	open    = |net|, in the direction of net (DEBIT when net > 0, else CREDIT)
//	recent  = Σ of that same direction dated on or after the cut-off
//	aged    = max(0, open − recent)
//
// One record per (account, currency, book) with aged > 0; amount is the aged
// amount (positive, in the direction named by the `direction` attribute). The
// age is derived from the cut-off parameter, never from now().
//
// $1 tenant_id, $2 legal_entity_id, $3 aged_before (date).
const clearingBalanceAgingScope = `
	SELECT (t.account_code || ':' || t.currency_code || ':' || t.book_id) AS record_id,
	       t.account_code                                                 AS reference,
	       t.aged                                                         AS amount,
	       t.currency_code                                                AS currency,
	       t.last_aged_date                                               AS txn_date,
	       t.max_seq                                                      AS max_seq,
	       jsonb_build_object(
	           'account_code',  t.account_code,
	           'book_id',       t.book_id,
	           'direction',     CASE WHEN t.net > 0 THEN 'DEBIT' ELSE 'CREDIT' END,
	           'open_balance',  ABS(t.net)::text,
	           'recent_amount', t.recent_same::text,
	           'aged_amount',   t.aged::text,
	           'oldest_posting_date', to_char(t.oldest_date, 'YYYY-MM-DD'),
	           'age_days',      ($3::date - t.oldest_date)::text,
	           'entry_count',   t.cnt::text) AS attrs
	FROM (
		SELECT g.*,
		       GREATEST(0, ABS(g.net) - g.recent_same) AS aged
		FROM (
			SELECT le.account_code, le.currency_code, le.book_id,
			       SUM(le.debit_amount - le.credit_amount) AS net,
			       COALESCE(CASE WHEN SUM(le.debit_amount - le.credit_amount) > 0
			                     THEN SUM(le.debit_amount)  FILTER (WHERE le.transaction_date >= $3::date)
			                     ELSE SUM(le.credit_amount) FILTER (WHERE le.transaction_date >= $3::date) END, 0) AS recent_same,
			       MAX(le.transaction_date) FILTER (WHERE le.transaction_date <  $3::date) AS last_aged_date,
			       MIN(le.transaction_date) AS oldest_date,
			       MAX(le.entry_seq)        AS max_seq,
			       COUNT(*)                 AS cnt
			FROM ledger_entries le
			WHERE le.tenant_id = $1::uuid
			  AND le.legal_entity_id = $2::uuid
			  AND le.control_type = 'CLEARING'
			GROUP BY le.account_code, le.currency_code, le.book_id
		) g
		WHERE g.net <> 0
	) t
	WHERE t.aged > 0`

// QueryClearingBalanceAging returns one page of the clearing-balance-aging
// violation population.
func (s *PgStore) QueryClearingBalanceAging(ctx context.Context, tenantID string, q domain.ClearingBalanceAgingQuery) (*domain.ControlPopulationPage, error) {
	return s.queryGroupedPopulation(ctx, tenantID, groupedPopulationSpec{
		scope:      clearingBalanceAgingScope,
		scopeArgs:  []any{tenantID, q.LegalEntityID, q.AgedBefore},
		digestExpr: wave5DigestExpr,
		wmPrefix:   "gl9",
		after:      q.AfterRecordID,
		limit:      q.Limit,
	})
}
