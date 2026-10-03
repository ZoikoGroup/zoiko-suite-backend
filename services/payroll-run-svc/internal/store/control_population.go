package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// Scope CTEs. Both take the same four positional parameters so the aggregate,
// the totals and the page can share one argument list:
//
//	$1 tenant_id            (explicit predicate, in addition to RLS)
//	$2 legal_entity_id
//	$3 period start (date, NULL = no date filter). FLOW semantics: the run's
//	   pay_date must fall in that calendar month exactly.
//	$4 measure ('gross' | 'net')
//
// Money is NOT rounded: every amount is the exact stored NUMERIC(18,4) value, so
// the control can compare exactly and sub-cent errors stay visible. Record
// amounts, declared totals and the watermark digest all use the same values.
//
// The scopes deliberately select NO employee name and no contact/bank column.
const paySlipsScope = `
WITH scope AS (
	SELECT s.slip_id                                            AS id,
	       s.run_id                                             AS run_id,
	       r.status                                             AS run_status,
	       r.is_shadow_run                                      AS is_shadow_run,
	       s.employee_number                                    AS employee_number,
	       s.gross_pay                                AS gross_pay,
	       s.tax_withheld                             AS tax_withheld,
	       s.benefits_deductions                      AS benefits_deductions,
	       s.net_pay                                  AS net_pay,
	       CASE WHEN $4::text = 'gross' THEN s.gross_pay
	            ELSE s.net_pay END                    AS amount,
	       s.currency                                           AS currency,
	       r.pay_date                                           AS pay_date
	FROM pay_slips s
	JOIN payroll_runs r ON r.run_id = s.run_id AND r.tenant_id = s.tenant_id
	WHERE s.tenant_id = $1
	  AND r.tenant_id = $1
	  AND r.legal_entity_id = $2
	  AND ($3::date IS NULL OR (r.pay_date >= $3::date AND r.pay_date < ($3::date + INTERVAL '1 month')))
)`

const payrollRunsScope = `
WITH slip_cur AS (
	SELECT run_id, count(DISTINCT currency) AS n_cur, min(currency) AS cur
	FROM pay_slips
	WHERE tenant_id = $1
	GROUP BY run_id
), scope AS (
	SELECT r.run_id                                             AS id,
	       r.status                                             AS status,
	       r.is_shadow_run                                      AS is_shadow_run,
	       r.employee_count                                     AS employee_count,
	       COALESCE(r.snapshot_hash, '')                        AS snapshot_hash,
	       r.total_gross_pay                          AS total_gross,
	       r.total_net_pay                            AS total_net,
	       CASE WHEN $4::text = 'gross' THEN r.total_gross_pay
	            ELSE r.total_net_pay END              AS amount,
	       CASE WHEN c.run_id IS NULL OR c.n_cur > 1 THEN 'XXX' ELSE c.cur END AS currency,
	       CASE WHEN c.run_id IS NULL THEN 'no_slips'
	            WHEN c.n_cur > 1 THEN 'mixed_currencies'
	            ELSE '' END                                     AS currency_note,
	       r.pay_date                                           AS pay_date
	FROM payroll_runs r
	LEFT JOIN slip_cur c ON c.run_id = r.run_id
	WHERE r.tenant_id = $1
	  AND r.legal_entity_id = $2
	  AND ($3::date IS NULL OR (r.pay_date >= $3::date AND r.pay_date < ($3::date + INTERVAL '1 month')))
)`

// Watermark digest inputs: ids, status and every amount component only.
// Independent of `measure`, so both measures of one dataset share a watermark.
const (
	paySlipsDigest = `md5(COALESCE(string_agg(id::text || '|' || run_status || '|' || is_shadow_run::text || '|' ||
	                       gross_pay::text || '|' || tax_withheld::text || '|' ||
	                       benefits_deductions::text || '|' || net_pay::text || '|' || currency,
	                       ',' ORDER BY id), ''))`
	payrollRunsDigest = `md5(COALESCE(string_agg(id::text || '|' || status || '|' || is_shadow_run::text || '|' ||
	                       total_gross::text || '|' || total_net::text || '|' || currency || '|' ||
	                       employee_count::text || '|' || snapshot_hash,
	                       ',' ORDER BY id), ''))`
)

// withReadSnapshot is withRLS for one REPEATABLE READ, read-only transaction:
// every statement inside sees the same snapshot.
func (s *PgStore) withReadSnapshot(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if s.schema != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('search_path', $1, true)", s.schema); err != nil {
			return fmt.Errorf("set search_path: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ControlPopulation returns one keyset page of a payroll control population plus
// the whole-set watermark and declared totals.
//
// Everything runs in ONE REPEATABLE READ, read-only transaction, so the count,
// the per-currency totals, the watermark and the page come from a single
// snapshot. Watermark = "<count>:<md5 over the ordered id|status|amount
// components of the WHOLE in-scope set>".
func (s *PgStore) ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	var scope, digest string
	switch q.Population {
	case domain.PopulationPaySlips:
		scope, digest = paySlipsScope, paySlipsDigest
	case domain.PopulationPayrollRuns:
		scope, digest = payrollRunsScope, payrollRunsDigest
	default:
		return nil, errors.New("unsupported control population")
	}

	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}},
	}
	args := []any{q.TenantID, q.LegalEntityID, q.PeriodStart, q.Measure}

	err := s.withReadSnapshot(ctx, q.TenantID, func(tx pgx.Tx) error {
		var md5sum string
		if err := tx.QueryRow(ctx, scope+`SELECT count(*), `+digest+` FROM scope`, args...).
			Scan(&page.DeclaredTotals.RowCount, &md5sum); err != nil {
			return err
		}
		if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
			return domain.ErrPopulationTooLarge
		}
		page.Watermark = strconv.FormatInt(page.DeclaredTotals.RowCount, 10) + ":" + md5sum

		totals, err := tx.Query(ctx, scope+`
			SELECT currency, sum(amount)::text FROM scope GROUP BY currency ORDER BY currency`, args...)
		if err != nil {
			return err
		}
		for totals.Next() {
			var cur, sum string
			if err := totals.Scan(&cur, &sum); err != nil {
				totals.Close()
				return err
			}
			page.DeclaredTotals.Totals[cur] = sum
		}
		totals.Close()
		if err := totals.Err(); err != nil {
			return err
		}

		var after *string
		if q.AfterRecordID != "" {
			after = &q.AfterRecordID
		}
		pageArgs := append(append([]any{}, args...), after, q.Limit+1)

		if q.Population == domain.PopulationPaySlips {
			return scanPaySlips(ctx, tx, scope, pageArgs, page)
		}
		return scanPayrollRuns(ctx, tx, scope, pageArgs, page)
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

func scanPaySlips(ctx context.Context, tx pgx.Tx, scope string, args []any, page *domain.ControlPopulationPage) error {
	rows, err := tx.Query(ctx, scope+`
		SELECT id::text, run_id::text, amount::text, currency, to_char(pay_date, 'YYYY-MM-DD'),
		       run_status, is_shadow_run, employee_number,
		       gross_pay::text, tax_withheld::text, benefits_deductions::text, net_pay::text
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
		var runStatus, empNo, gross, tax, ben, net string
		var shadow bool
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date,
			&runStatus, &shadow, &empNo, &gross, &tax, &ben, &net); err != nil {
			return err
		}
		// employee_number is the only employee-identifying field. Never a name.
		r.Attributes = map[string]string{
			"run_id":              r.Reference,
			"run_status":          runStatus,
			"is_shadow_run":       strconv.FormatBool(shadow),
			"employee_number":     empNo,
			"gross_pay":           gross,
			"tax_withheld":        tax,
			"benefits_deductions": ben,
			"net_pay":             net,
		}
		page.Records = append(page.Records, r)
	}
	return rows.Err()
}

func scanPayrollRuns(ctx context.Context, tx pgx.Tx, scope string, args []any, page *domain.ControlPopulationPage) error {
	rows, err := tx.Query(ctx, scope+`
		SELECT id::text, amount::text, currency, to_char(pay_date, 'YYYY-MM-DD'),
		       status, is_shadow_run, employee_count, snapshot_hash, currency_note
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
		var status, snapshot, note string
		var shadow bool
		var empCount int
		if err := rows.Scan(&r.RecordID, &r.Amount, &r.Currency, &r.Date,
			&status, &shadow, &empCount, &snapshot, &note); err != nil {
			return err
		}
		r.Reference = r.RecordID
		r.Attributes = map[string]string{
			"status":         status,
			"is_shadow_run":  strconv.FormatBool(shadow),
			"employee_count": strconv.Itoa(empCount),
			"snapshot_hash":  snapshot,
		}
		if note != "" {
			r.Attributes["currency_note"] = note
		}
		page.Records = append(page.Records, r)
	}
	return rows.Err()
}
