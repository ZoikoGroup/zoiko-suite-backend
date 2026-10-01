package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// accountPostingsScope is the shared FROM/WHERE for the account-postings
// control population. Every entry is reported as-is (reversed journals'
// entries and their offsetting reversal entries alike); the consuming control
// nets per reference.
//
// reference is the journal header's source_event_id; when NULL/blank it is
// 'journal:'||journal_id so an unattributable posting can never falsely match a
// subledger record. The header join is a LEFT JOIN so a posting is never
// silently dropped from the population.
//
// $1 tenant_id, $2 legal_entity_id, $3 account codes, $4 period-end date (NULL
// for no cut-off), $5 normal balance.
const accountPostingsScope = `
	SELECT le.journal_line_id                                    AS record_id,
	       COALESCE(NULLIF(BTRIM(jh.source_event_id), ''), 'journal:' || le.journal_id::text) AS reference,
	       (CASE WHEN $5 = 'DEBIT' THEN le.debit_amount - le.credit_amount
	             ELSE le.credit_amount - le.debit_amount END)     AS amount,
	       le.currency_code                                       AS currency,
	       le.transaction_date                                    AS txn_date,
	       le.account_code, le.journal_id, le.fiscal_period, le.book_id,
	       le.entry_seq, COALESCE(jh.journal_seq, 0)              AS journal_seq
	FROM ledger_entries le
	LEFT JOIN journal_headers jh
	       ON jh.tenant_id = le.tenant_id AND jh.journal_id = le.journal_id
	WHERE le.tenant_id = $1::uuid
	  AND le.legal_entity_id = $2::uuid
	  AND le.account_code = ANY($3::text[])
	  AND ($4::date IS NULL OR le.transaction_date <= $4::date)`

// QueryAccountPostings returns one keyset page (ordered by journal_line_id) of
// the account-postings control population together with the whole-set
// watermark and declared totals. All three are read inside ONE REPEATABLE READ
// read-only transaction, so they describe the same snapshot as the page.
func (s *PgStore) QueryAccountPostings(ctx context.Context, tenantID string, q domain.AccountPostingsQuery) (*domain.AccountPostingsPage, error) {
	var periodEnd any // NULL when there is no cut-off
	if q.PeriodEnd != "" {
		periodEnd = q.PeriodEnd
	}
	var after any
	if q.AfterRecordID != "" {
		after = q.AfterRecordID
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; rollback is the normal exit

	// Same tenant scoping as withRLS (transaction-local), in addition to the
	// explicit tenant_id predicate in the SQL.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	page := &domain.AccountPostingsPage{
		Records:        []domain.ControlPopulationRecord{},
		DeclaredTotals: domain.ControlDeclaredTotals{Totals: map[string]string{}},
	}

	// Whole-set aggregate: count, commit sequences and a content digest over
	// the ordered population. Any added/changed/removed record changes it.
	var maxEntrySeq, maxJournalSeq int64
	var digest string
	err = tx.QueryRow(ctx, `
		WITH scope AS (`+accountPostingsScope+`)
		SELECT COUNT(*),
		       COALESCE(MAX(entry_seq), 0),
		       COALESCE(MAX(journal_seq), 0),
		       md5(COALESCE(string_agg(
		           record_id::text || '|' || reference || '|' || amount::text || '|' || currency || '|' || txn_date::text,
		           ',' ORDER BY record_id), ''))
		FROM scope`,
		tenantID, q.LegalEntityID, q.AccountCodes, periodEnd, q.NormalBalance,
	).Scan(&page.DeclaredTotals.RowCount, &maxEntrySeq, &maxJournalSeq, &digest)
	if err != nil {
		return nil, mapPgError(err)
	}
	if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
		return nil, domain.ErrControlPopulationTooLarge
	}
	page.Watermark = fmt.Sprintf("gl1:n=%d;entry_seq=%d;journal_seq=%d;md5=%s",
		page.DeclaredTotals.RowCount, maxEntrySeq, maxJournalSeq, digest)

	totals, err := tx.Query(ctx, `
		WITH scope AS (`+accountPostingsScope+`)
		SELECT currency, SUM(amount)::text FROM scope GROUP BY currency ORDER BY currency`,
		tenantID, q.LegalEntityID, q.AccountCodes, periodEnd, q.NormalBalance)
	if err != nil {
		return nil, mapPgError(err)
	}
	for totals.Next() {
		var cur, sum string
		if err := totals.Scan(&cur, &sum); err != nil {
			totals.Close()
			return nil, mapPgError(err)
		}
		page.DeclaredTotals.Totals[cur] = sum
	}
	totals.Close()
	if err := totals.Err(); err != nil {
		return nil, mapPgError(err)
	}

	// The page itself: keyset on journal_line_id, one extra row to learn
	// whether more remain. $6 = cursor, $7 = limit+1.
	rows, err := tx.Query(ctx, `
		WITH scope AS (`+accountPostingsScope+`)
		SELECT record_id::text, reference, amount::text, currency,
		       to_char(txn_date, 'YYYY-MM-DD'),
		       account_code, journal_id::text, fiscal_period, book_id
		FROM scope
		WHERE ($6::uuid IS NULL OR record_id > $6::uuid)
		ORDER BY record_id ASC
		LIMIT $7`,
		tenantID, q.LegalEntityID, q.AccountCodes, periodEnd, q.NormalBalance, after, limit+1)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlPopulationRecord
		var account, journalID, fiscalPeriod, bookID string
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date,
			&account, &journalID, &fiscalPeriod, &bookID); err != nil {
			return nil, mapPgError(err)
		}
		r.Attributes = map[string]string{
			"account_code":  account,
			"journal_id":    journalID,
			"fiscal_period": fiscalPeriod,
			"book_id":       bookID,
		}
		page.Records = append(page.Records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgError(err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextRecordID = page.Records[limit-1].RecordID
	}
	return page, nil
}
