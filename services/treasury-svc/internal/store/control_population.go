package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/treasury-svc/internal/domain"
)

// Control populations of treasury-svc — see docs/architecture/control-population-contract.md
// ("treasury-svc"). Every scope yields the fixed columns
//
//	record_id (uuid text), reference, amount (decimal text), currency, txn_date (YYYY-MM-DD), attrs (jsonb of strings)
//
// and is paged by queryUUIDPopulation: one REPEATABLE READ read-only transaction, the whole-set watermark and
// declared totals read from the same snapshot as the page, keyset on record_id, explicit tenant_id predicates.

// fxRateObservationsScope: FIN-CTRL-026 "FX rate source completeness". One record per fx_rates row whose UTC
// effective date lies in the month. amount is the rate (exact NUMERIC text), currency is XXX (a rate is not money),
// and the currency pair is both the reference and the group attribute the coverage test uses.
// $1 tenant_id, $2 first day of the month, $3 last day of the month.
const fxRateObservationsScope = `
	SELECT f.rate_id::text                                        AS record_id,
	       f.currency_pair                                        AS reference,
	       f.rate::text                                           AS amount,
	       'XXX'::text                                            AS currency,
	       to_char((f.effective_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') AS txn_date,
	       jsonb_build_object(
	           'currency_pair', f.currency_pair,
	           'recorded_by',   f.recorded_by_principal_id,
	           'effective_at',  to_char(f.effective_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')) AS attrs
	FROM fx_rates f
	WHERE f.tenant_id = $1::text
	  AND (f.effective_at AT TIME ZONE 'UTC')::date BETWEEN $2::date AND $3::date`

// bankAccountOwnershipScope: FIN-CTRL-044 "Cash account ownership/entity validation". One record per bank account of
// the legal entity, every status (nothing is pre-filtered). ownership_gap is computed in SQL, first match wins:
//
//	OWNERSHIP_UNVERIFIED  ACTIVE or SUSPENDED account with no current (non-superseded) ownership evidence
//	SELF_VERIFIED         the current evidence was recorded by the principal who created the account
//	''                    no gap (also: DRAFT / PENDING_VERIFICATION / CLOSED accounts are not yet or no longer in use)
//
// STRICT PRIVACY: no account number, bank identifier, branch or account name is selected.
// $1 tenant_id, $2 legal_entity_id.
const bankAccountOwnershipScope = `
	SELECT a.bank_account_id::text                                AS record_id,
	       a.bank_account_id::text                                AS reference,
	       '0'::text                                              AS amount,
	       'XXX'::text                                            AS currency,
	       to_char((a.created_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') AS txn_date,
	       jsonb_build_object(
	           'account_status',     a.account_status,
	           'account_currency',   a.currency_code,
	           'account_type',       a.account_type,
	           'ownership_verified', (ev.evidence_id IS NOT NULL)::text,
	           'verified_by',        COALESCE(ev.verified_by_principal_id, ''),
	           'created_by',         a.created_by_principal_id,
	           'ownership_gap',      CASE
	               WHEN a.account_status IN ('ACTIVE', 'SUSPENDED') AND ev.evidence_id IS NULL THEN 'OWNERSHIP_UNVERIFIED'
	               WHEN a.account_status IN ('ACTIVE', 'SUSPENDED')
	                    AND NULLIF(BTRIM(a.created_by_principal_id), '') IS NOT NULL
	                    AND BTRIM(ev.verified_by_principal_id) = BTRIM(a.created_by_principal_id) THEN 'SELF_VERIFIED'
	               ELSE '' END) AS attrs
	FROM bank_accounts a
	LEFT JOIN LATERAL (
		SELECT e.evidence_id, e.verified_by_principal_id
		  FROM bank_account_ownership_evidence e
		 WHERE e.tenant_id = $1::text AND e.bank_account_id = a.bank_account_id AND e.superseded_by IS NULL
		 ORDER BY e.verified_at DESC LIMIT 1) ev ON TRUE
	WHERE a.tenant_id = $1::text
	  AND a.legal_entity_id = $2::text`

const populationDigestExpr = `record_id || '|' || reference || '|' || amount || '|' || currency || '|' || txn_date || '|' || attrs::text`

// QueryFXRateObservations returns one keyset page of the fx-rate-observations population.
func (s *PgStore) QueryFXRateObservations(ctx context.Context, tenantID string, q domain.FXRateObservationsQuery) (*domain.ControlPopulationPage, error) {
	first, err := time.Parse("2006-01", q.PeriodID)
	if err != nil {
		return nil, fmt.Errorf("period_id must be YYYY-MM: %w", err)
	}
	last := first.AddDate(0, 1, -1)
	return s.queryUUIDPopulation(ctx, tenantID, fxRateObservationsScope,
		[]any{tenantID, first.Format("2006-01-02"), last.Format("2006-01-02")}, "tr1", q.AfterRecordID, q.Limit)
}

// QueryBankAccountOwnership returns one keyset page of the bank-account-ownership population.
func (s *PgStore) QueryBankAccountOwnership(ctx context.Context, tenantID string, q domain.BankAccountOwnershipQuery) (*domain.ControlPopulationPage, error) {
	return s.queryUUIDPopulation(ctx, tenantID, bankAccountOwnershipScope,
		[]any{tenantID, q.LegalEntityID}, "tr2", q.AfterRecordID, q.Limit)
}

func (s *PgStore) queryUUIDPopulation(ctx context.Context, tenantID, scope string, scopeArgs []any, wmPrefix, afterID string, limit int) (*domain.ControlPopulationPage, error) {
	var after any
	if afterID != "" {
		after = afterID
	}
	if limit <= 0 {
		limit = 1000
	}
	afterPos, limitPos := len(scopeArgs)+1, len(scopeArgs)+2

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
		WITH scope AS (`+scope+`)
		SELECT COUNT(*), md5(COALESCE(string_agg(`+populationDigestExpr+`, ',' ORDER BY record_id COLLATE "C"), ''))
		FROM scope`, scopeArgs...).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
		return nil, err
	}
	if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
		return nil, domain.ErrControlPopulationTooLarge
	}
	page.Watermark = fmt.Sprintf("%s:n=%d;md5=%s", wmPrefix, page.DeclaredTotals.RowCount, digest)

	totals, err := tx.Query(ctx, `
		WITH scope AS (`+scope+`)
		SELECT currency, SUM(amount::numeric)::text FROM scope GROUP BY currency ORDER BY currency COLLATE "C"`, scopeArgs...)
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

	args := append(append([]any{}, scopeArgs...), after, limit+1)
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		WITH scope AS (`+scope+`)
		SELECT record_id, reference, amount, currency, txn_date, attrs
		FROM scope
		WHERE ($%d::uuid IS NULL OR record_id::uuid > $%d::uuid)
		ORDER BY record_id::uuid ASC
		LIMIT $%d`, afterPos, afterPos, limitPos), args...)
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
