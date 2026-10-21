package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// Control populations of the Workforce & Payroll Accounting Interface, served to
// financial-control-svc (FIN-CTRL-005, PAY-CTRL-004, FIN-CTRL-006). They use the same four
// positional parameters as the legacy populations:
//
//	$1 tenant_id  $2 legal_entity_id  $3 period start (NULL = no date filter)  $4 measure ('gross' | 'net')
//
// Every record is ONE accepted payroll result's side of the comparison, paired by the result id
// (`reference`), so nothing is netted across results. Amounts are the exact stored NUMERIC(18,4)
// values, never rounded. No worker, payee or bank column is selected, ever.
//
// Only ACCEPTED results of ACCEPTED run references are in scope: a superseded result is no
// longer payroll's position, and its journal, liabilities and payments are handled by the
// correction chain.
const wfpResultsScope = `
WITH scope AS (
	SELECT res.result_id                                            AS id,
	       res.result_id::text                                      AS reference,
	       CASE WHEN $4::text = 'gross' THEN res.total_gross ELSE res.total_net_pay END AS amount,
	       res.currency                                             AS currency,
	       r.pay_date                                               AS pay_date,
	       'ACCEPTED'::text                                         AS state,
	       res.result_fingerprint                                   AS fingerprint
	FROM wfp_payroll_results res
	JOIN wfp_payroll_run_refs r ON r.run_ref_id = res.run_ref_id AND r.tenant_id = res.tenant_id
	WHERE res.tenant_id = $1 AND r.legal_entity_id = $2
	  AND res.outcome = 'ACCEPTED' AND r.acceptance_state = 'ACCEPTED'
	  AND ($3::date IS NULL OR (r.pay_date >= $3::date AND r.pay_date < ($3::date + INTERVAL '1 month')))
)`

// The POSTED payroll journal of each accepted result: gross is the debit of the gross-expense
// line, net is the credit of the net-pay payable line. The id is the subject's own id so two
// books for one result would show as two records with one reference (a duplicate, visibly).
const wfpJournalsScope = `
WITH scope AS (
	SELECT b.bridge_id                                              AS id,
	       res.result_id::text                                      AS reference,
	       CASE WHEN $4::text = 'gross'
	            THEN COALESCE((SELECT sum((l->>'debit')::numeric) FROM jsonb_array_elements(b.lines) l
	                           WHERE l->>'mapping_key' = 'PAYROLL_GROSS_EXPENSE'), 0)
	            ELSE COALESCE((SELECT sum((l->>'credit')::numeric) FROM jsonb_array_elements(b.lines) l
	                           WHERE l->>'mapping_key' = 'PAYROLL_NET_PAY_PAYABLE'), 0) END AS amount,
	       b.transaction_currency                                   AS currency,
	       r.pay_date                                               AS pay_date,
	       b.state                                                  AS state,
	       b.source_event_id                                        AS fingerprint
	FROM wfp_payroll_accounting_bridges b
	JOIN wfp_payroll_results res ON res.result_id = b.result_id AND res.tenant_id = b.tenant_id
	JOIN wfp_payroll_run_refs r ON r.run_ref_id = res.run_ref_id AND r.tenant_id = res.tenant_id
	WHERE b.tenant_id = $1 AND r.legal_entity_id = $2
	  AND b.posting_purpose = 'PAYROLL_RESULT' AND b.state = 'POSTED'
	  AND res.outcome = 'ACCEPTED' AND r.acceptance_state = 'ACCEPTED'
	  AND ($3::date IS NULL OR (r.pay_date >= $3::date AND r.pay_date < ($3::date + INTERVAL '1 month')))
)`

// The net pay each accepted result has instructed: the sum of its live NET_PAY instructions
// (cancelled, superseded and returned ones no longer stand for a payment). It is a net-pay
// population; the measure is ignored (the handler refuses any other than net).
const wfpNetInstructionsScope = `
WITH scope AS (
	SELECT res.result_id                                            AS id,
	       res.result_id::text                                      AS reference,
	       sum(i.amount)                                            AS amount,
	       i.currency                                               AS currency,
	       r.pay_date                                               AS pay_date,
	       'INSTRUCTED'::text                                       AS state,
	       ''::text                                                 AS fingerprint
	FROM wfp_payment_instructions i
	JOIN wfp_payroll_results res ON res.result_id = i.result_id AND res.tenant_id = i.tenant_id
	JOIN wfp_payroll_run_refs r ON r.run_ref_id = res.run_ref_id AND r.tenant_id = res.tenant_id
	WHERE i.tenant_id = $1 AND r.legal_entity_id = $2 AND $4::text = 'net'
	  AND i.instruction_type = 'NET_PAY' AND i.state NOT IN ('CANCELLED', 'SUPERSEDED', 'RETURNED')
	  AND res.outcome = 'ACCEPTED' AND r.acceptance_state = 'ACCEPTED'
	  AND ($3::date IS NULL OR (r.pay_date >= $3::date AND r.pay_date < ($3::date + INTERVAL '1 month')))
	GROUP BY res.result_id, i.currency, r.pay_date
)`

// One digest for all three: ids, references, state, amount, currency and the fingerprint.
const wfpDigest = `md5(COALESCE(string_agg(id::text || '|' || reference || '|' || state || '|' || amount::text || '|' || currency || '|' || fingerprint,
	',' ORDER BY id, reference), ''))`

func wfpScope(population string) (scope, digest string, ok bool) {
	switch population {
	case domain.PopulationWFPResults:
		return wfpResultsScope, wfpDigest, true
	case domain.PopulationWFPJournals:
		return wfpJournalsScope, wfpDigest, true
	case domain.PopulationWFPNetInstructions:
		return wfpNetInstructionsScope, wfpDigest, true
	}
	return "", "", false
}

func scanWFPPopulation(ctx context.Context, tx pgx.Tx, scope string, args []any, page *domain.ControlPopulationPage) error {
	rows, err := tx.Query(ctx, scope+`
		SELECT id::text, reference, amount::text, currency, to_char(pay_date, 'YYYY-MM-DD'), state, fingerprint
		FROM scope
		WHERE ($5::uuid IS NULL OR id > $5::uuid)
		ORDER BY id
		LIMIT $6`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlRecord
		var state, fingerprint string
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &state, &fingerprint); err != nil {
			return err
		}
		r.Attributes = map[string]string{"state": state}
		if fingerprint != "" {
			r.Attributes["fingerprint"] = fingerprint
		}
		page.Records = append(page.Records, r)
	}
	return rows.Err()
}
