package store

import (
	"context"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// postSealEntriesScope is the post-seal-entries control population (ZS-CONTROL-001 FIN-CTRL-024,
// "Closed-period posting block").
//
// Sealing a period records the entity's ledger watermark (max(entry_seq) at that moment) and every kernel posting
// transaction refuses a sealed period (assertPeriodOpen). This is the DETECTIVE test of that preventive control: a ledger
// entry of a currently SEALED period whose entry_seq is above the seal's watermark was written after the close and slipped
// past it (for example through a path that does not take the period lock).
//
// A REOPENED period is not in scope: entries posted while it is reopened are authorized by the reopen authority, and the
// re-seal moves the watermark above them. One record per ledger entry; amount is the signed net (debit − credit).
//
// $1 tenant_id, $2 legal_entity_id.
const postSealEntriesScope = `
	SELECT le.ledger_entry_id::text                       AS record_id,
	       le.journal_id::text                            AS reference,
	       (le.debit_amount - le.credit_amount)           AS amount,
	       le.currency_code                               AS currency,
	       le.transaction_date                            AS txn_date,
	       le.entry_seq                                   AS max_seq,
	       jsonb_build_object(
	           'fiscal_period',          le.fiscal_period,
	           'account_code',           le.account_code,
	           'journal_id',             le.journal_id::text,
	           'entry_seq',              le.entry_seq::text,
	           'seal_watermark',         ps.watermark::text,
	           'entries_past_watermark', (le.entry_seq - ps.watermark)::text,
	           'sealed_by',              ps.sealed_by,
	           'sealed_at',              to_char(ps.sealed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	           'seal_count',             ps.seal_count::text) AS attrs
	FROM ledger_entries le
	JOIN period_seals ps
	  ON ps.tenant_id = le.tenant_id AND ps.legal_entity_id = le.legal_entity_id AND ps.fiscal_period = le.fiscal_period
	WHERE le.tenant_id = $1::uuid
	  AND le.legal_entity_id = $2::uuid
	  AND ps.status = 'SEALED'
	  AND le.entry_seq > ps.watermark`

// QueryPostSealEntries returns one page of the post-seal-entries violation population.
func (s *PgStore) QueryPostSealEntries(ctx context.Context, tenantID string, q domain.PostSealEntriesQuery) (*domain.ControlPopulationPage, error) {
	return s.queryGroupedPopulation(ctx, tenantID, groupedPopulationSpec{
		scope:      postSealEntriesScope,
		scopeArgs:  []any{tenantID, q.LegalEntityID},
		digestExpr: wave5DigestExpr,
		wmPrefix:   "gl10",
		after:      q.AfterRecordID,
		limit:      q.Limit,
	})
}
