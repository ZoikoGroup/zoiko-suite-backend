package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payment-run-svc/internal/domain"
)

// unappliedPaymentsScope is the unapplied-supplier-payments control population
// (ZS-CONTROL-001 FIN-CTRL-030): one row per payable of an instruction that
// Banking reports SETTLED while AP-08 has not yet accepted the settlement
// application (payable_applied_at IS NULL) — cash has left the bank but the
// supplier liability has not been reduced.
//
// The settlement date is the instruction's first SETTLED reconciliation event;
// an instruction with no such event (it should not exist) falls back to its
// creation date and says so in the settled_at_source attribute, so it can never
// be silently dropped.
//
// $1 tenant_id, $2 legal_entity_id (uuid), $3 unapplied_before (date, strict <).
const unappliedPaymentsScope = `
	SELECT (ri.instruction_id::text || ':' || p.payable_source || ':' || p.source_reference) AS record_id,
	       p.source_reference                                  AS reference,
	       p.net_amount::numeric(18,2)::text                   AS amount,
	       ri.currency                                         AS currency,
	       to_char(sd.settled_date, 'YYYY-MM-DD')              AS txn_date,
	       ri.instruction_id::text                             AS instruction_id,
	       ri.run_id::text                                     AS run_id,
	       ri.payee_ref                                        AS payee_ref,
	       p.payable_source                                    AS payable_source,
	       p.gross_amount::numeric(18,2)::text                 AS gross_amount,
	       p.withholding_amount::numeric(18,2)::text           AS withholding_amount,
	       ri.bnk07_payment_id                                 AS bnk07_payment_id,
	       ri.status                                           AS instruction_status,
	       sd.source                                           AS settled_at_source,
	       ($3::date - sd.settled_date)::text                  AS age_days
	FROM run_instruction_payables p
	JOIN run_instructions ri ON ri.instruction_id = p.instruction_id AND ri.tenant_id = $1::uuid
	JOIN payment_runs r      ON r.run_id = ri.run_id AND r.tenant_id = $1::uuid
	CROSS JOIN LATERAL (
		SELECT COALESCE((SELECT (MIN(e.created_at) AT TIME ZONE 'UTC')::date
		                   FROM instruction_reconciliation_events e
		                  WHERE e.tenant_id = $1::uuid AND e.instruction_id = ri.instruction_id
		                    AND e.external_status = 'SETTLED'),
		                (ri.created_at AT TIME ZONE 'UTC')::date) AS settled_date,
		       CASE WHEN EXISTS (SELECT 1 FROM instruction_reconciliation_events e
		                          WHERE e.tenant_id = $1::uuid AND e.instruction_id = ri.instruction_id
		                            AND e.external_status = 'SETTLED')
		            THEN 'EVENT' ELSE 'INSTRUCTION_CREATED' END AS source
	) sd
	WHERE p.tenant_id = $1::uuid
	  AND r.legal_entity_id = $2::uuid
	  AND ri.status = 'SETTLED'
	  AND p.payable_applied_at IS NULL
	  AND sd.settled_date < $3::date`

// unappliedPaymentsDigestExpr covers every wire field and attribute.
const unappliedPaymentsDigestExpr = `record_id || '|' || reference || '|' || amount || '|' || currency || '|' || txn_date || '|' ||
	instruction_id || '|' || run_id || '|' || payee_ref || '|' || payable_source || '|' || gross_amount || '|' ||
	withholding_amount || '|' || bnk07_payment_id || '|' || instruction_status || '|' || settled_at_source || '|' || age_days`

// QueryUnappliedSupplierPayments returns one keyset page (ordered by record_id,
// byte order) of the unapplied-supplier-payments population with the whole-set
// watermark and declared totals, all read in ONE REPEATABLE READ read-only
// transaction.
func (s *PgStore) QueryUnappliedSupplierPayments(ctx context.Context, tenantID string, q domain.UnappliedPaymentsQuery) (*domain.ControlPopulationPage, error) {
	var after any
	if q.AfterRecordID != "" {
		after = q.AfterRecordID
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	args := []any{tenantID, q.LegalEntityID, q.UnappliedBefore}

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
		WITH scope AS (`+unappliedPaymentsScope+`)
		SELECT COUNT(*),
		       md5(COALESCE(string_agg(`+unappliedPaymentsDigestExpr+`, ',' ORDER BY record_id COLLATE "C"), ''))
		FROM scope`, args...).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
		return nil, err
	}
	if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
		return nil, domain.ErrControlPopulationTooLarge
	}
	page.Watermark = fmt.Sprintf("pr1:n=%d;md5=%s", page.DeclaredTotals.RowCount, digest)

	totals, err := tx.Query(ctx, `
		WITH scope AS (`+unappliedPaymentsScope+`)
		SELECT currency, SUM(amount::numeric)::numeric(18,2)::text FROM scope GROUP BY currency ORDER BY currency`, args...)
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
		WITH scope AS (`+unappliedPaymentsScope+`)
		SELECT record_id, reference, amount, currency, txn_date, instruction_id, run_id, payee_ref, payable_source,
		       gross_amount, withholding_amount, bnk07_payment_id, instruction_status, settled_at_source, age_days
		FROM scope
		WHERE ($4::text IS NULL OR record_id COLLATE "C" > $4::text COLLATE "C")
		ORDER BY record_id COLLATE "C" ASC
		LIMIT $5`, append(args, after, limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlPopulationRecord
		var instructionID, runID, payee, source, gross, withholding, bnk07, status, settledSrc, age string
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date,
			&instructionID, &runID, &payee, &source, &gross, &withholding, &bnk07, &status, &settledSrc, &age); err != nil {
			return nil, err
		}
		r.Attributes = map[string]string{
			"instruction_id": instructionID, "run_id": runID, "payee_ref": payee, "payable_source": source,
			"gross_amount": gross, "withholding_amount": withholding, "bnk07_payment_id": bnk07,
			"instruction_status": status, "settled_at_source": settledSrc, "age_days": age,
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
