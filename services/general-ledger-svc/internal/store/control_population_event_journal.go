package store

import (
	"context"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// event-journal-breaks control population (FIN-CTRL-019 "Accounting event to
// journal") — see the "event-journal-breaks" section of
// docs/architecture/control-population-contract.md. Reuses
// queryGroupedPopulation: REPEATABLE READ read-only tx, whole-set watermark and
// declared totals, keyset paging on record_id under COLLATE "C".

// eventJournalBreaksScope: $1 tenant_id, $2 legal_entity_id, $3 created_before
// (timestamptz). One record per break, three disjoint branches.
//
//  1. EVENT_WITHOUT_JOURNAL: a COMMITTED kind='EVENT' execution created before
//     $3 whose journal_id is NULL, dangling, or points at a journal that is
//     not FINALIZED/REVERSED.
//  2. JOURNAL_WITHOUT_EVENT: a FINALIZED/REVERSED journal with a non-blank
//     source_event_id, created and posted before $3, with no COMMITTED
//     execution of the same tenant carrying that source_event_id and that
//     journal_id.
//  3. DUPLICATE_JOURNAL_FOR_EVENT: every non-REVERSED journal beyond the first
//     (created_at, journal_id order) for one (tenant, source_event_id),
//     created before $3.
//
// amount = SUM(debit) of the journal's lines when a journal row is involved,
// else 0; currency = the journal's currency_code, else XXX.
const eventJournalBreaksScope = `
	SELECT ('EVENT_WITHOUT_JOURNAL:' || pe.execution_id::text)                     AS record_id,
	       COALESCE(NULLIF(BTRIM(pe.source_event_id), ''), pe.execution_id::text)  AS reference,
	       COALESCE((SELECT SUM(jl.debit_amount) FROM journal_lines jl
	                  WHERE jl.tenant_id = pe.tenant_id AND jl.journal_id = jh.journal_id), 0) AS amount,
	       COALESCE(jh.currency_code::text, 'XXX')                                 AS currency,
	       (pe.created_at AT TIME ZONE 'UTC')::date                                AS txn_date,
	       0::bigint                                                               AS max_seq,
	       jsonb_build_object(
	           'break_type',      'EVENT_WITHOUT_JOURNAL',
	           'status',          COALESCE(jh.status, pe.status),
	           'journal_id',      COALESCE(pe.journal_id::text, ''),
	           'execution_id',    pe.execution_id::text,
	           'source_event_id', COALESCE(pe.source_event_id, '')) AS attrs
	FROM posting_executions pe
	LEFT JOIN journal_headers jh
	  ON jh.tenant_id = pe.tenant_id AND jh.journal_id = pe.journal_id
	WHERE pe.tenant_id = $1::uuid
	  AND pe.legal_entity_id = $2::uuid
	  AND pe.kind = 'EVENT'
	  AND pe.status = 'COMMITTED'
	  AND pe.created_at < $3::timestamptz
	  AND (jh.journal_id IS NULL OR jh.status NOT IN ('FINALIZED', 'REVERSED'))
	UNION ALL
	SELECT ('JOURNAL_WITHOUT_EVENT:' || jh.journal_id::text)                       AS record_id,
	       jh.source_event_id                                                      AS reference,
	       COALESCE((SELECT SUM(jl.debit_amount) FROM journal_lines jl
	                  WHERE jl.tenant_id = jh.tenant_id AND jl.journal_id = jh.journal_id), 0) AS amount,
	       jh.currency_code::text                                                  AS currency,
	       jh.posting_date                                                         AS txn_date,
	       0::bigint                                                               AS max_seq,
	       jsonb_build_object(
	           'break_type',      'JOURNAL_WITHOUT_EVENT',
	           'status',          jh.status,
	           'journal_id',      jh.journal_id::text,
	           'execution_id',    '',
	           'source_event_id', jh.source_event_id) AS attrs
	FROM journal_headers jh
	WHERE jh.tenant_id = $1::uuid
	  AND jh.legal_entity_id = $2::uuid
	  AND jh.status IN ('FINALIZED', 'REVERSED')
	  AND NULLIF(BTRIM(jh.source_event_id), '') IS NOT NULL
	  AND jh.created_at < $3::timestamptz
	  AND COALESCE(jh.posted_at, jh.created_at) < $3::timestamptz
	  AND NOT EXISTS (
	      SELECT 1 FROM posting_executions pe
	       WHERE pe.tenant_id = jh.tenant_id
	         AND pe.source_event_id = jh.source_event_id
	         AND pe.status = 'COMMITTED'
	         AND pe.journal_id = jh.journal_id)
	UNION ALL
	SELECT ('DUPLICATE_JOURNAL_FOR_EVENT:' || d.journal_id::text)                  AS record_id,
	       d.source_event_id                                                       AS reference,
	       COALESCE((SELECT SUM(jl.debit_amount) FROM journal_lines jl
	                  WHERE jl.tenant_id = d.tenant_id AND jl.journal_id = d.journal_id), 0) AS amount,
	       d.currency_code::text                                                   AS currency,
	       d.posting_date                                                          AS txn_date,
	       0::bigint                                                               AS max_seq,
	       jsonb_build_object(
	           'break_type',      'DUPLICATE_JOURNAL_FOR_EVENT',
	           'status',          d.status,
	           'journal_id',      d.journal_id::text,
	           'execution_id',    '',
	           'source_event_id', d.source_event_id) AS attrs
	FROM (
	    SELECT jh.*,
	           ROW_NUMBER() OVER (PARTITION BY jh.source_event_id
	                              ORDER BY jh.created_at, jh.journal_id) AS rn
	    FROM journal_headers jh
	    WHERE jh.tenant_id = $1::uuid
	      AND jh.status <> 'REVERSED'
	      AND NULLIF(BTRIM(jh.source_event_id), '') IS NOT NULL
	) d
	WHERE d.rn > 1
	  AND d.legal_entity_id = $2::uuid
	  AND d.created_at < $3::timestamptz`

// QueryEventJournalBreaks returns one page of the event-journal-breaks
// violation population.
func (s *PgStore) QueryEventJournalBreaks(ctx context.Context, tenantID string, q domain.EventJournalBreaksQuery) (*domain.ControlPopulationPage, error) {
	return s.queryGroupedPopulation(ctx, tenantID, groupedPopulationSpec{
		scope:      eventJournalBreaksScope,
		scopeArgs:  []any{tenantID, q.LegalEntityID, q.CreatedBefore.UTC()},
		digestExpr: wave5DigestExpr,
		wmPrefix:   "gl8",
		after:      q.AfterRecordID,
		limit:      q.Limit,
	})
}
