package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-close-svc/internal/domain"
)

// MigrationBatchTieout returns the migration-batch-tieout control population: exactly
// one record for the batch, comparing the batch's declared totals with the SUM of its
// crosswalk lines.
//
// Everything is computed by the database in ONE REPEATABLE READ, read-only
// transaction with explicit tenant_id predicates (not RLS alone). Amounts are NUMERIC
// text, never float. migration_batches has no currency column, so the record's
// currency is 'XXX' (ISO 4217 "no currency").
//
// Watermark = fc1:n=1;md5=<md5 over every wire field and attribute>.
func (s *PgStore) MigrationBatchTieout(ctx context.Context, q domain.MigrationBatchTieoutQuery) (*domain.ControlPopulationPage, error) {
	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}},
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", q.TenantID); err != nil {
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
			SELECT batch_id, expected_total_debits, expected_total_credits, expected_row_count,
			       status, source_extract_hash, COALESCE(journal_id, '') AS journal_id,
			       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS load_date
			FROM migration_batches
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND batch_id = $3::uuid
		), x AS (
			SELECT COALESCE(SUM(debit_amount), 0)::numeric(18,2) AS d,
			       COALESCE(SUM(credit_amount), 0)::numeric(18,2) AS c,
			       COUNT(*) AS n
			FROM migration_crosswalk_entries
			WHERE tenant_id = $1 AND batch_id = $3::uuid
		)
		SELECT b.batch_id::text, b.expected_total_debits::text, x.d::text,
		       b.expected_total_credits::text, x.c::text,
		       b.expected_row_count::bigint, x.n,
		       b.status, b.source_extract_hash, b.journal_id, b.load_date,
		       md5(concat_ws('|', b.batch_id::text, b.expected_total_debits::text, b.load_date,
		                     x.d::text, x.c::text, b.expected_total_credits::text,
		                     b.expected_row_count::text, x.n::text,
		                     b.status, b.source_extract_hash, b.journal_id))
		FROM b CROSS JOIN x`,
		q.TenantID, q.LegalEntityID, q.BatchID,
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

	page.Watermark = "fc1:n=1;md5=" + digest
	page.DeclaredTotals.RowCount = 1
	page.DeclaredTotals.Totals["XXX"] = debits
	if q.AfterRecordID == "" || batchID > q.AfterRecordID {
		page.Records = append(page.Records, domain.ControlRecord{
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
