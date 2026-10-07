package store

import (
	"context"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// Wave 5 control populations — see the "Wave 5 population definitions" section
// of docs/architecture/control-population-contract.md. All reuse
// queryGroupedPopulation (REPEATABLE READ read-only tx, whole-set watermark and
// declared totals, keyset paging on record_id under COLLATE "C").

// journalBalancesScope: $1 tenant_id, $2 legal_entity_id, $3 fiscal_period.
// One record per (journal, currency) over the period's ledger_entries. amount is
// the debit total; the credit total is an attribute so the consuming control can
// check debit_total = credit_total per journal. record_id carries the currency
// so a two-currency journal never yields a duplicate record_id.
const journalBalancesScope = `
	SELECT (le.journal_id::text || ':' || le.currency_code) AS record_id,
	       le.journal_id::text                              AS reference,
	       SUM(le.debit_amount)                             AS amount,
	       le.currency_code                                 AS currency,
	       MAX(le.transaction_date)                         AS txn_date,
	       MAX(le.entry_seq)                                AS max_seq,
	       jsonb_build_object(
	           'debit_total',  SUM(le.debit_amount)::text,
	           'credit_total', SUM(le.credit_amount)::text,
	           'entry_count',  COUNT(*)::text)              AS attrs
	FROM ledger_entries le
	WHERE le.tenant_id = $1::uuid
	  AND le.legal_entity_id = $2::uuid
	  AND le.fiscal_period = $3::text
	GROUP BY le.journal_id, le.currency_code`

// controlAccountPostingsScope: $1 tenant_id, $2 legal_entity_id, $3 fiscal_period.
// One record per ledger entry on a control account with direct posting
// restricted, whose journal has no source event (NULL or blank) — a manual
// posting that bypassed the subledger. The header join is a LEFT JOIN so an
// entry without a header is still reported (as manual) rather than dropped.
// amount = debit - credit (signed net; positive = net debit).
const controlAccountPostingsScope = `
	SELECT le.ledger_entry_id::text                          AS record_id,
	       le.journal_id::text                               AS reference,
	       (le.debit_amount - le.credit_amount)              AS amount,
	       le.currency_code                                  AS currency,
	       le.transaction_date                               AS txn_date,
	       le.entry_seq                                      AS max_seq,
	       jsonb_build_object(
	           'account_code', le.account_code,
	           'journal_id',   le.journal_id::text,
	           'journal_type', COALESCE(jh.journal_type, ''),
	           'created_by',   COALESCE(jh.created_by_principal_id, ''),
	           'approved_by',  COALESCE(jh.approved_by_principal_id, '')) AS attrs
	FROM ledger_entries le
	JOIN chart_of_accounts coa
	  ON coa.tenant_id = le.tenant_id AND coa.account_code = le.account_code
	 AND coa.is_control_account AND coa.direct_posting_restricted
	LEFT JOIN journal_headers jh
	  ON jh.tenant_id = le.tenant_id AND jh.journal_id = le.journal_id
	WHERE le.tenant_id = $1::uuid
	  AND le.legal_entity_id = $2::uuid
	  AND le.fiscal_period = $3::text
	  AND (jh.source_event_id IS NULL OR BTRIM(jh.source_event_id) = '')`

// unpostedEventsScope: $1 tenant_id, $2 legal_entity_id, $3 created_before
// (timestamptz). posting_executions carries no amount or currency: amount is
// 0 and currency is the ISO 4217 "no currency" code XXX (the consumer requires
// ^[A-Z]{3}$). age_days is derived from $3, never from now().
const unpostedEventsScope = `
	SELECT pe.execution_id::text                                            AS record_id,
	       COALESCE(NULLIF(BTRIM(pe.source_event_id), ''), pe.execution_id::text) AS reference,
	       0::numeric                                                       AS amount,
	       'XXX'::text                                                      AS currency,
	       (pe.created_at AT TIME ZONE 'UTC')::date                         AS txn_date,
	       0::bigint                                                        AS max_seq,
	       jsonb_build_object(
	           'status',         pe.status,
	           'kind',           pe.kind,
	           'failure_reason', LEFT(COALESCE(pe.failure_reason, ''), 200),
	           'age_days',       floor(extract(epoch FROM ($3::timestamptz - pe.created_at))::numeric / 86400)::bigint::text) AS attrs
	FROM posting_executions pe
	WHERE pe.tenant_id = $1::uuid
	  AND pe.legal_entity_id = $2::uuid
	  AND pe.status NOT IN ('COMMITTED')
	  AND pe.created_at < $3::timestamptz`

// manualJournalsScope: $1 tenant_id, $2 legal_entity_id, $3 fiscal_period.
// FINALIZED and REVERSED journals of the period with no source event (NULL or
// blank). amount = total debit of the journal's lines (exact NUMERIC).
const manualJournalsScope = `
	SELECT jh.journal_id::text                                AS record_id,
	       jh.journal_id::text                                AS reference,
	       COALESCE((SELECT SUM(jl.debit_amount) FROM journal_lines jl
	                  WHERE jl.tenant_id = jh.tenant_id AND jl.journal_id = jh.journal_id), 0) AS amount,
	       jh.currency_code::text                             AS currency,
	       jh.transaction_date                                AS txn_date,
	       jh.journal_seq                                     AS max_seq,
	       jsonb_build_object(
	           'status',          jh.status,
	           'fiscal_period',   jh.fiscal_period,
	           'journal_type',    jh.journal_type,
	           'created_by',      jh.created_by_principal_id,
	           'approved_by',     COALESCE(jh.approved_by_principal_id, ''),
	           'approval_status', jh.approval_status,
	           'review_gap',      CASE
	               WHEN NULLIF(BTRIM(jh.approved_by_principal_id), '') IS NULL THEN 'NO_APPROVER'
	               WHEN BTRIM(jh.approved_by_principal_id) = BTRIM(jh.created_by_principal_id) THEN 'SELF_APPROVED'
	               ELSE '' END) AS attrs
	FROM journal_headers jh
	WHERE jh.tenant_id = $1::uuid
	  AND jh.legal_entity_id = $2::uuid
	  AND jh.fiscal_period = $3::text
	  AND jh.status IN ('FINALIZED', 'REVERSED')
	  AND (jh.source_event_id IS NULL OR BTRIM(jh.source_event_id) = '')`

// wave5DigestExpr covers every wire field including all attributes, so any
// change to a record changes the watermark.
const wave5DigestExpr = `record_id || '|' || reference || '|' || amount::text || '|' || currency || '|' || txn_date::text || '|' || attrs::text`

func (s *PgStore) wave5FiscalPeriod(ctx context.Context, tenantID string, q domain.FiscalPeriodPopulationQuery, scope, prefix string) (*domain.ControlPopulationPage, error) {
	return s.queryGroupedPopulation(ctx, tenantID, groupedPopulationSpec{
		scope:      scope,
		scopeArgs:  []any{tenantID, q.LegalEntityID, q.FiscalPeriod},
		digestExpr: wave5DigestExpr,
		wmPrefix:   prefix,
		after:      q.AfterRecordID,
		limit:      q.Limit,
	})
}

// QueryJournalBalances returns one page of the journal-balances population.
func (s *PgStore) QueryJournalBalances(ctx context.Context, tenantID string, q domain.FiscalPeriodPopulationQuery) (*domain.ControlPopulationPage, error) {
	return s.wave5FiscalPeriod(ctx, tenantID, q, journalBalancesScope, "gl4")
}

// QueryControlAccountPostings returns one page of the control-account-postings
// violation population.
func (s *PgStore) QueryControlAccountPostings(ctx context.Context, tenantID string, q domain.FiscalPeriodPopulationQuery) (*domain.ControlPopulationPage, error) {
	return s.wave5FiscalPeriod(ctx, tenantID, q, controlAccountPostingsScope, "gl5")
}

// QueryManualJournals returns one page of the manual-journals population.
func (s *PgStore) QueryManualJournals(ctx context.Context, tenantID string, q domain.FiscalPeriodPopulationQuery) (*domain.ControlPopulationPage, error) {
	return s.wave5FiscalPeriod(ctx, tenantID, q, manualJournalsScope, "gl7")
}

// QueryUnpostedEvents returns one page of the unposted-events population.
func (s *PgStore) QueryUnpostedEvents(ctx context.Context, tenantID string, q domain.UnpostedEventsQuery) (*domain.ControlPopulationPage, error) {
	return s.queryGroupedPopulation(ctx, tenantID, groupedPopulationSpec{
		scope:      unpostedEventsScope,
		scopeArgs:  []any{tenantID, q.LegalEntityID, q.CreatedBefore.UTC()},
		digestExpr: wave5DigestExpr,
		wmPrefix:   "gl6",
		after:      q.AfterRecordID,
		limit:      q.Limit,
	})
}
