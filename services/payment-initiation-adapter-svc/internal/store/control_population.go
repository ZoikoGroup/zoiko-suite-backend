package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

// paymentAttemptsScope is the payment-attempts control population (ZS-CONTROL-001 FIN-CTRL-004, "Payment
// provider settlement"): the internal side of the comparison with the bank's statement.
//
// One record per payment initiation attempt of the legal entity, EVERY status (nothing is pre-filtered; the
// control excludes the statuses that moved no money by an explicit, owned, evidenced rule).
//
//	reference  the payment_reference sent to the provider, which the bank/provider is expected to echo on the
//	           statement line; when blank, 'attempt:<attempt_id>' so an unreferenced attempt can never falsely
//	           pair with a bank line
//	amount     the cash movement, SIGNED from the account's point of view like a bank statement line: a payment
//	           is money OUT, so the amount is NEGATIVE
//	date       the execution date (UTC calendar day)
//
// STRICT PRIVACY: no payer or payee account reference is selected; the opaque source_reference (the caller's
// instruction id) is the only link to the payment's origin.
//
// $1 tenant_id, $2 legal_entity_id, $3 cut-off date (NULL = none).
const paymentAttemptsScope = `
	SELECT a.attempt_id::text                                                        AS record_id,
	       COALESCE(NULLIF(BTRIM(a.payment_reference), ''), 'attempt:' || a.attempt_id::text) AS reference,
	       (-a.amount)::numeric(18,2)::text                                          AS amount,
	       a.currency                                                                AS currency,
	       to_char((a.execution_date AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD')        AS txn_date,
	       jsonb_build_object(
	           'status',           a.status,
	           'source_reference', a.source_reference,
	           'provider_key',     a.provider_key,
	           'rail',             a.rail,
	           'provider_status',  a.provider_status) AS attrs
	FROM payment_initiation_attempts a
	WHERE a.tenant_id = $1::uuid
	  AND a.legal_entity_id = $2::uuid
	  AND ($3::date IS NULL OR (a.execution_date AT TIME ZONE 'UTC')::date <= $3::date)`

const paymentAttemptsDigestExpr = `record_id || '|' || reference || '|' || amount || '|' || currency || '|' || txn_date || '|' || attrs::text`

// QueryPaymentAttempts returns one keyset page (ordered by attempt_id) of the payment-attempts population with
// the whole-set watermark and declared totals, all read in ONE REPEATABLE READ read-only transaction.
func (s *PgStore) QueryPaymentAttempts(ctx context.Context, tenantID string, q domain.PaymentAttemptsQuery) (*domain.ControlPopulationPage, error) {
	var after any
	if q.AfterRecordID != "" {
		after = q.AfterRecordID
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	var cutOff any
	if q.CutOff != nil {
		cutOff = q.CutOff.Format("2006-01-02")
	}
	args := []any{tenantID, q.LegalEntityID, cutOff}

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
		WITH scope AS (`+paymentAttemptsScope+`)
		SELECT COUNT(*), md5(COALESCE(string_agg(`+paymentAttemptsDigestExpr+`, ',' ORDER BY record_id COLLATE "C"), ''))
		FROM scope`, args...).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
		return nil, err
	}
	if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
		return nil, domain.ErrControlPopulationTooLarge
	}
	page.Watermark = fmt.Sprintf("pa1:n=%d;md5=%s", page.DeclaredTotals.RowCount, digest)

	totals, err := tx.Query(ctx, `
		WITH scope AS (`+paymentAttemptsScope+`)
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
		WITH scope AS (`+paymentAttemptsScope+`)
		SELECT record_id, reference, amount, currency, txn_date, attrs
		FROM scope
		WHERE ($4::uuid IS NULL OR record_id::uuid > $4::uuid)
		ORDER BY record_id::uuid ASC
		LIMIT $5`, append(args, after, limit+1)...)
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
