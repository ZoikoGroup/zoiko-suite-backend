package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/data-ingestion-svc/internal/domain"
)

// importBatchTieoutScope is the import-batch-tieout control population (ZS-CONTROL-001 FIN-CTRL-033, "Import batch
// completeness"): exactly one record for the requested batch, carrying the raw facts the control re-performs its
// ties from — never a verdict.
//
//	declared_*   what the SOURCE said about the file (NULL → '' so absence is visible, never a made-up zero)
//	row_count, valid_rows, invalid_rows, computed_control_total   what the platform parsed from the file
//	imported/quarantined/failed/skipped_items                      what the owning domain did with each row
//
// amount is the computed control total when it is a decimal, else 0; currency is XXX (a control total of an
// arbitrary column has no currency). The file name is evidence and never exposed; neither is any row content.
//
// $1 tenant_id, $2 legal_entity_id, $3 batch_id.
const importBatchTieoutScope = `
	SELECT b.batch_id                                       AS record_id,
	       b.batch_id                                       AS reference,
	       CASE WHEN b.computed_control_total ~ '^-?[0-9]+(\.[0-9]+)?$' THEN b.computed_control_total ELSE '0' END AS amount,
	       'XXX'::text                                      AS currency,
	       to_char((b.created_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') AS txn_date,
	       jsonb_build_object(
	           'batch_status',           b.status,
	           'profile_key',            b.profile_key,
	           'profile_version',        b.profile_version::text,
	           'source_system',          b.source_system,
	           'declared_rows',          COALESCE(b.declared_rows::text, ''),
	           'row_count',              b.row_count::text,
	           'valid_rows',             b.valid_rows::text,
	           'invalid_rows',           b.invalid_rows::text,
	           'imported_items',         b.imported_items::text,
	           'quarantined_items',      b.quarantined_items::text,
	           'failed_items',           b.failed_items::text,
	           'skipped_items',          b.skipped_items::text,
	           'declared_control_total', COALESCE(b.declared_control_total, ''),
	           'computed_control_total', COALESCE(b.computed_control_total, ''),
	           'quarantine_state',       CASE WHEN b.status = 'QUARANTINED' THEN 'FULL'
	                                          WHEN q.held = 0 THEN 'NONE'
	                                          WHEN q.open_requests > 0 THEN 'RELEASE_PENDING'
	                                          WHEN q.total > 0 AND q.held >= q.total THEN 'FULL'
	                                          ELSE 'PARTIAL' END) AS attrs
	FROM import_batches b
	CROSS JOIN LATERAL (
	    SELECT (SELECT count(*) FROM import_items i WHERE i.tenant_id = b.tenant_id AND i.batch_id = b.batch_id AND i.status = 'QUARANTINED') AS held,
	           (SELECT count(*) FROM import_items i WHERE i.tenant_id = b.tenant_id AND i.batch_id = b.batch_id AND i.status NOT IN ('INVALID', 'SKIPPED')) AS total,
	           (SELECT count(*) FROM import_quarantine_ledger r
	              JOIN import_items i ON i.batch_id = r.batch_id AND i.row_no = r.row_no AND i.status = 'QUARANTINED'
	             WHERE r.tenant_id = b.tenant_id AND r.batch_id = b.batch_id AND r.kind = 'RELEASE_REQUESTED'
	               AND NOT EXISTS (SELECT 1 FROM import_quarantine_ledger c WHERE c.answers_ledger_id = r.ledger_id)) AS open_requests
	) q
	WHERE b.tenant_id = $1::text
	  AND b.legal_entity_id = $2::text
	  AND b.batch_id = $3::text`

// QueryImportBatchTieout returns the import-batch-tieout population of one batch, with its watermark and declared
// totals, read in ONE REPEATABLE READ read-only transaction. An unknown batch is ErrImportBatchNotFound.
func (s *PgStore) QueryImportBatchTieout(ctx context.Context, tenantID string, q domain.ImportBatchTieoutQuery) (*domain.ControlPopulationPage, error) {
	if tenantID == "" {
		return nil, errors.New("tenant_id is required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; rollback is the normal exit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	var r domain.ControlPopulationRecord
	var digest string
	err = tx.QueryRow(ctx, `
		WITH scope AS (`+importBatchTieoutScope+`)
		SELECT record_id, reference, amount, currency, txn_date, attrs,
		       md5(record_id || '|' || reference || '|' || amount || '|' || currency || '|' || txn_date || '|' || attrs::text)
		FROM scope`, tenantID, q.LegalEntityID, q.BatchID).Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &r.Attributes, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrImportBatchNotFound
	}
	if err != nil {
		return nil, err
	}

	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlPopulationRecord{},
		Watermark:      fmt.Sprintf("di1:n=1;md5=%s", digest),
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"XXX": r.Amount}},
	}
	// One record: a cursor at or past it leaves nothing, and never repeats it.
	if q.AfterRecordID == "" || q.AfterRecordID < r.RecordID {
		page.Records = append(page.Records, r)
	}
	return page, nil
}
