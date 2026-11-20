package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// QueryMigrationBatchTieout returns the migration-batch-tieout control
// population: exactly one record for the batch, comparing the batch's
// declared source totals (source_totals, supplied independently at Stage —
// internal/kernel/migration.go) with the SUM of the lines general-ledger-svc
// actually staged and posted for it (migration_batch_lines). Those two
// figures are asserted independently of each other at Stage time (the caller
// hands both a source trial balance total and the lines that are meant to
// sum to it), so a genuine drift between them is exactly the kind of break
// this population exists to surface — the same role financial-close-svc's
// now-redundant migration_batches/migration_crosswalk_entries pair played,
// before migration-integrity-svc started driving this kernel instead.
//
// migration-integrity-svc's own migration_identity_crosswalk table was
// considered as the "crosswalk" side, but it carries no debit/credit
// amounts at all — it is a pure (source_system, source_type, source_id) ->
// canonical_id identity map (ZS-MIG-001 MIG-INV-14), not a ledger total. The
// genuine, independently-asserted second figure for this kernel's batches is
// migration_batch_lines, summed fresh here rather than read off the batch's
// own stored total_debit/total_credit columns (which are themselves derived
// from these same lines at Stage time and would just restate the batch,
// never actually catching anything).
//
// Everything is computed by the database in ONE REPEATABLE READ, read-only
// transaction with explicit tenant_id predicates (RLS is also enabled on
// both tables, but this pool may connect as a role that bypasses it).
// Amounts are NUMERIC text, never float. migration_batches has no currency
// column, so the record's currency is 'XXX' (ISO 4217 "no currency"), same as
// financial-close-svc's version of this population.
//
// Watermark = gl12:n=1;md5=<md5 over every wire field and attribute>.
func (s *PgStore) QueryMigrationBatchTieout(ctx context.Context, tenantID string, q domain.MigrationBatchTieoutQuery) (*domain.ControlPopulationPage, error) {
	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlPopulationRecord{},
		DeclaredTotals: domain.ControlDeclaredTotals{Totals: map[string]string{}},
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, fmt.Errorf("set tenant context: %w", err)
	}

	var (
		batchID, debits, xDebits, credits, xCredits string
		rowCount, xRowCount                         int64
		status, hash, journalID, loadDate           string
		digest                                      string
	)
	err = tx.QueryRow(ctx, `
		WITH b AS (
			SELECT batch_id, (source_totals ->> 'debit') AS expected_debits, (source_totals ->> 'credit') AS expected_credits,
			       line_count, status, source_extract_hash, COALESCE(journal_id::text, '') AS journal_id,
			       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS load_date
			FROM migration_batches
			WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND batch_id = $3::uuid
		), x AS (
			SELECT COALESCE(SUM(debit_amount), 0)::numeric(18,2) AS d,
			       COALESCE(SUM(credit_amount), 0)::numeric(18,2) AS c,
			       COUNT(*) AS n
			FROM migration_batch_lines
			WHERE tenant_id = $1::uuid AND batch_id = $3::uuid
		)
		SELECT b.batch_id::text, b.expected_debits, x.d::text,
		       b.expected_credits, x.c::text,
		       b.line_count::bigint, x.n,
		       b.status, b.source_extract_hash, b.journal_id, b.load_date,
		       md5(concat_ws('|', b.batch_id::text, b.expected_debits, b.load_date,
		                     x.d::text, x.c::text, b.expected_credits,
		                     b.line_count::text, x.n::text,
		                     b.status, b.source_extract_hash, b.journal_id))
		FROM b CROSS JOIN x`,
		tenantID, q.LegalEntityID, q.BatchID,
	).Scan(&batchID, &debits, &xDebits, &credits, &xCredits, &rowCount, &xRowCount,
		&status, &hash, &journalID, &loadDate, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMigrationBatchNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	page.Watermark = "gl12:n=1;md5=" + digest
	page.DeclaredTotals.RowCount = 1
	page.DeclaredTotals.Totals["XXX"] = debits
	if q.AfterRecordID == "" || batchID > q.AfterRecordID {
		page.Records = append(page.Records, domain.ControlPopulationRecord{
			RecordID:  batchID,
			Reference: batchID,
			Amount:    debits,
			Currency:  "XXX",
			Date:      loadDate,
			Attributes: map[string]string{
				"expected_total_debits":  debits,
				"crosswalk_debits":       xDebits,
				"expected_total_credits": credits,
				"crosswalk_credits":      xCredits,
				"expected_row_count":     fmt.Sprintf("%d", rowCount),
				"crosswalk_row_count":    fmt.Sprintf("%d", xRowCount),
				"batch_status":           status,
				"source_extract_hash":    hash,
				"journal_id":             journalID,
			},
		})
	}
	return page, nil
}
