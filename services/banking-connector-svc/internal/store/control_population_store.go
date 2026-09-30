package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
)

// Shared WHERE tail for both populations. $1 tenant, $2 legal entity,
// $3 period-end cut-off (” = none), $4 bank account filter (” = none).
// The tenant predicate is explicit on every table in the join chain — RLS
// (set via app.tenant_id below) is defence in depth, not the only guard.
const controlScopeWhere = `
	c.tenant_id = $1 AND s.tenant_id = $1
	AND c.legal_entity_id = $2
	AND ($3::text = '' OR %s <= $3::date)
	AND ($4::text = '' OR COALESCE(NULLIF(c.bank_account_id, ''), c.connection_id) = $4::text)`

// bank-transactions: one row per canonical transaction (every status,
// including SUPERSEDED / QUARANTINED — marked in attributes, never dropped).
//
//	reference  = bank_statement_lines.raw_reference (the bank's own reference
//	             captured as evidence at ingest); when the bank sent none,
//	             'txn:<transaction_id>' so an unreferenced line can never
//	             falsely pair with a ledger record.
//	amount     = bank_transactions_canonical.amount, SIGNED. Statement-line
//	             amounts are signed from the account's point of view — the
//	             ValidateStatement invariant is opening + sum(lines) = closing,
//	             so credits (money in) are positive and debits negative — and
//	             normalization carries the amount across unchanged.
//	date       = canonical transaction_date (booking date), UTC calendar day.
//	wm_status  = canonical status + '/' + statement status, so a quarantine of
//	             the parent statement (exposed as attributes.statement_status)
//	             also moves the watermark.
var controlTransactionsScope = `
	SELECT t.transaction_id AS id,
	       COALESCE(NULLIF(l.raw_reference, ''), 'txn:' || t.transaction_id) AS reference,
	       t.amount AS amount,
	       t.currency AS currency,
	       (t.transaction_date AT TIME ZONE 'UTC')::date AS bdate,
	       COALESCE(NULLIF(c.bank_account_id, ''), c.connection_id) AS acct,
	       s.statement_id AS statement_id,
	       t.status AS status,
	       s.status AS statement_status,
	       t.status || '/' || s.status AS wm_status
	FROM bank_transactions_canonical t
	JOIN bank_statement_lines l ON l.line_id = t.statement_line_id AND l.tenant_id = t.tenant_id
	JOIN bank_statements s ON s.statement_id = l.statement_id
	JOIN bank_connections c ON c.connection_id = s.connection_id
	WHERE t.tenant_id = $1 AND l.tenant_id = $1 AND ` +
	fmt.Sprintf(controlScopeWhere, `(t.transaction_date AT TIME ZONE 'UTC')::date`)

// bank-statements: one row per ingested statement, every status.
//
//	reference = '<bank_account_id>|<statement_date YYYY-MM-DD>'
//	amount    = closing_balance (column exists, NOT NULL)
//	currency  = bank_connections.currency (statements carry none)
var controlStatementsScope = `
	SELECT s.statement_id AS id,
	       COALESCE(NULLIF(c.bank_account_id, ''), c.connection_id) || '|' ||
	           to_char((s.statement_date AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') AS reference,
	       s.closing_balance AS amount,
	       c.currency AS currency,
	       (s.statement_date AT TIME ZONE 'UTC')::date AS bdate,
	       COALESCE(NULLIF(c.bank_account_id, ''), c.connection_id) AS acct,
	       s.statement_id AS statement_id,
	       s.status AS status,
	       s.status AS statement_status,
	       s.status AS wm_status
	FROM bank_statements s
	JOIN bank_connections c ON c.connection_id = s.connection_id
	WHERE ` + fmt.Sprintf(controlScopeWhere, `(s.statement_date AT TIME ZONE 'UTC')::date`)

// ControlPopulation returns one keyset page of a population together with the
// whole-set watermark and declared totals, all read in ONE REPEATABLE READ,
// READ ONLY transaction so the three are mutually consistent.
//
// Watermark = '<count>:<md5(string_agg(id|status|amount ORDER BY id))>' over
// the WHOLE in-scope set, computed by Postgres.
func (p *PgStore) ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	var scope string
	switch q.Population {
	case domain.PopulationBankTransactions:
		scope = controlTransactionsScope
	case domain.PopulationBankStatements:
		scope = controlStatementsScope
	default:
		return nil, fmt.Errorf("unknown control population %q", q.Population)
	}
	if q.TenantID == "" {
		return nil, fmt.Errorf("tenant_id is required")
	}
	// Tenant comes from the verified context for RLS; it must agree with the
	// explicit predicate value.
	if ctxTenant := middleware.GetTenantID(ctx); ctxTenant != q.TenantID {
		return nil, fmt.Errorf("tenant mismatch between context and query")
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; nothing to commit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", q.TenantID); err != nil {
		return nil, err
	}

	args := []any{q.TenantID, q.LegalEntityID, q.PeriodEnd, q.BankAccountID}
	out := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.ControlDeclaredTotals{Totals: map[string]string{}},
	}

	var count int
	var digest string
	if err := tx.QueryRow(ctx, `WITH scope AS (`+scope+`)
		SELECT count(*),
		       COALESCE(md5(string_agg(id || '|' || wm_status || '|' || amount::text, ',' ORDER BY id COLLATE "C")), '')
		FROM scope`, args...).Scan(&count, &digest); err != nil {
		return nil, err
	}
	if count > domain.ControlPopulationMaxRecords {
		return nil, domain.ErrPopulationTooLarge
	}
	out.DeclaredTotals.RowCount = count
	out.Watermark = fmt.Sprintf("%d:%s", count, digest)

	trows, err := tx.Query(ctx, `WITH scope AS (`+scope+`)
		SELECT currency, sum(amount)::text FROM scope GROUP BY currency ORDER BY currency`, args...)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var cur, total string
		if err := trows.Scan(&cur, &total); err != nil {
			trows.Close()
			return nil, err
		}
		out.DeclaredTotals.Totals[cur] = total
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return nil, err
	}

	// Fetch limit+1 to learn whether another page exists without OFFSET.
	pageArgs := append(append([]any{}, args...), q.AfterRecordID, q.Limit+1)
	rows, err := tx.Query(ctx, `WITH scope AS (`+scope+`)
		SELECT id, reference, amount::text, currency, to_char(bdate, 'YYYY-MM-DD'), acct, statement_id, status, statement_status
		FROM scope
		WHERE ($5::text = '' OR id COLLATE "C" > $5::text COLLATE "C")
		ORDER BY id COLLATE "C"
		LIMIT $6`, pageArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlRecord
		var acct, stmtID, status, stmtStatus string
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &acct, &stmtID, &status, &stmtStatus); err != nil {
			return nil, err
		}
		if q.Population == domain.PopulationBankTransactions {
			r.Attributes = map[string]string{
				"bank_account_id":  acct,
				"statement_id":     stmtID,
				"status":           status,
				"statement_status": stmtStatus,
			}
		} else {
			r.Attributes = map[string]string{
				"bank_account_id":           acct,
				"status":                    status,
				"closing_balance_available": "true",
			}
		}
		out.Records = append(out.Records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Records) > q.Limit {
		out.Records = out.Records[:q.Limit]
		out.NextCursor = out.Records[q.Limit-1].RecordID // raw id; the handler makes it opaque
	}
	return out, nil
}
