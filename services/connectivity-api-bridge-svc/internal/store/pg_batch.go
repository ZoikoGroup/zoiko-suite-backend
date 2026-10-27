package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// BatchRepo is the Postgres implementation of batch.Repo (migration 009).
type BatchRepo struct{ p *PgStore }

// NewBatchRepo wraps a PgStore.
func NewBatchRepo(p *PgStore) *BatchRepo { return &BatchRepo{p: p} }

var _ batch.Repo = (*BatchRepo)(nil)

func (r *BatchRepo) enqueue(ctx context.Context, tx pgx.Tx, tenant string, e batch.OutEvent) error {
	return r.p.enqueue(ctx, tx, tenant, domain.OutboxEvent{Type: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		ActorID: e.ActorID, CorrelationID: e.CorrelationID, Data: e.Data})
}

func batchErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch pgCode(err) {
	case "23505":
		if strings.Contains(msg, "xic_b_dedupe") {
			return batch.ErrDuplicate
		}
	case "23000":
		switch {
		case strings.Contains(msg, "cannot go") || strings.Contains(msg, "is final") || strings.Contains(msg, "does not go back"):
			return batch.ErrTransition
		}
	}
	return err
}

const batchCols = `batch_id::text, tenant_id, connection_id::text, connector_id, operation_id, contract_version_id::text, COALESCE(mapping_version_id::text, ''),
	external_batch_id, source_system, source_schema, channel, state, reason_code, xic_code, manifest_hash, content_hash, COALESCE(dedupe_key, ''),
	COALESCE(original_batch_id::text, ''), manifest, file_count, declared_rows, parsed_rows, admitted_rows, accepted_rows, pending_rows, rejected_rows,
	duplicate_rows, quarantined_rows, domain_rejected_rows, variance_rows, processing_done, auth_mechanism, source_net_hash, correlation_id, lease_until,
	next_process_at, process_attempts, version, received_at, updated_at`

func scanBatch(row pgx.Row, b *batch.Batch) error {
	var man []byte
	if err := row.Scan(&b.BatchID, &b.TenantID, &b.ConnectionID, &b.ConnectorID, &b.OperationID, &b.ContractVersionID, &b.MappingVersionID, &b.ExternalBatchID,
		&b.SourceSystem, &b.SourceSchema, &b.Channel, &b.State, &b.ReasonCode, &b.XICCode, &b.ManifestHash, &b.ContentHash, &b.DedupeKey, &b.OriginalBatchID, &man,
		&b.FileCount, &b.DeclaredRows, &b.ParsedRows, &b.AdmittedRows, &b.AcceptedRows, &b.PendingRows, &b.RejectedRows, &b.DuplicateRows, &b.QuarantinedRows,
		&b.DomainRejected, &b.VarianceRows, &b.ProcessingDone, &b.AuthMechanism, &b.SourceNetHash, &b.CorrelationID, &b.LeaseUntil, &b.NextProcessAt,
		&b.ProcessAttempts, &b.Version, &b.ReceivedAt, &b.UpdatedAt); err != nil {
		return err
	}
	if len(man) > 0 && string(man) != "null" {
		b.Manifest = json.RawMessage(man)
	}
	return nil
}

func loadBatch(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*batch.Batch, error) {
	var b batch.Batch
	q := `SELECT ` + batchCols + ` FROM xic_batches WHERE batch_id=$1::uuid`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	if err := scanBatch(tx.QueryRow(ctx, q, id), &b); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, batch.ErrNotFound
		}
		return nil, err
	}
	return &b, nil
}

func nulStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func insertBatch(ctx context.Context, tx pgx.Tx, b *batch.Batch) error {
	var man any
	if len(b.Manifest) > 0 {
		man = []byte(b.Manifest)
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_batches (batch_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, mapping_version_id, external_batch_id,
		source_system, source_schema, channel, state, reason_code, xic_code, manifest_hash, content_hash, dedupe_key, original_batch_id, manifest, file_count, declared_rows,
		parsed_rows, admitted_rows, accepted_rows, pending_rows, rejected_rows, duplicate_rows, quarantined_rows, domain_rejected_rows, variance_rows, processing_done,
		auth_mechanism, source_net_hash, correlation_id, lease_until, next_process_at, process_attempts, version, received_at, updated_at)
		VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6::uuid,$7::uuid,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::uuid,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40)`,
		b.BatchID, b.TenantID, b.ConnectionID, b.ConnectorID, b.OperationID, b.ContractVersionID, nulStr(b.MappingVersionID), b.ExternalBatchID, b.SourceSystem,
		b.SourceSchema, b.Channel, b.State, b.ReasonCode, b.XICCode, b.ManifestHash, b.ContentHash, nulStr(b.DedupeKey), nulStr(b.OriginalBatchID), man, b.FileCount,
		b.DeclaredRows, b.ParsedRows, b.AdmittedRows, b.AcceptedRows, b.PendingRows, b.RejectedRows, b.DuplicateRows, b.QuarantinedRows, b.DomainRejected, b.VarianceRows,
		b.ProcessingDone, b.AuthMechanism, b.SourceNetHash, b.CorrelationID, b.LeaseUntil, b.NextProcessAt, b.ProcessAttempts, b.Version, b.ReceivedAt, b.UpdatedAt)
	return err
}

func insertFile(ctx context.Context, tx pgx.Tx, f *batch.File) error {
	var content any
	var exp any
	if len(f.Content) > 0 {
		content, exp = f.Content, f.ContentExpiresAt
	}
	if f.ActualSHA256 == "" && f.State == batch.FileReceived {
		f.ActualSHA256 = ""
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_batch_files (file_id, batch_id, tenant_id, name, ordinal, declared_sha256, declared_size, compression, actual_sha256, actual_size,
		declared_records, parsed_rows, state, reason_code, content, content_expires_at) VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		f.FileID, f.BatchID, f.TenantID, f.Name, f.Ordinal, f.DeclaredSHA256, f.DeclaredSize, dfltStr(f.Compression, "none"), f.ActualSHA256, f.ActualSize,
		f.DeclaredRecords, f.ParsedRows, f.State, f.ReasonCode, content, exp)
	return err
}

func dfltStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func insertBatchEvent(ctx context.Context, tx pgx.Tx, id, tenant string, ev batch.Event) error {
	var detail any
	if len(ev.Detail) > 0 {
		detail = []byte(ev.Detail)
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_batch_events (batch_id, tenant_id, kind, from_state, to_state, detail, actor, at) VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8)`,
		id, tenant, ev.Kind, ev.From, ev.To, detail, ev.Actor, at)
	return err
}

// Insert implements batch.Repo.
func (r *BatchRepo) Insert(ctx context.Context, b *batch.Batch, files []batch.File, ev batch.Event, out []batch.OutEvent) (*batch.Batch, error) {
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := insertBatch(ctx, tx, b); err != nil {
			return err
		}
		for i := range files {
			if err := insertFile(ctx, tx, &files[i]); err != nil {
				return err
			}
		}
		if err := insertBatchEvent(ctx, tx, b.BatchID, b.TenantID, ev); err != nil {
			return err
		}
		for _, o := range out {
			if err := r.enqueue(ctx, tx, b.TenantID, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return nil, nil
	}
	if errors.Is(batchErr(err), batch.ErrDuplicate) {
		holder, gerr := r.GetByDedupe(ctx, b.TenantID, b.DedupeKey)
		if gerr != nil {
			return nil, gerr
		}
		return holder, batch.ErrDuplicate
	}
	return nil, batchErr(err)
}

// InsertRejected implements batch.Repo.
func (r *BatchRepo) InsertRejected(ctx context.Context, b *batch.Batch, files []batch.File, ev batch.Event, out []batch.OutEvent) error {
	b.DedupeKey = ""
	return r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := insertBatch(ctx, tx, b); err != nil {
			return err
		}
		for i := range files {
			if files[i].FileID == "" {
				files[i].FileID = uuid.NewString()
			}
			files[i].BatchID, files[i].TenantID, files[i].Content = b.BatchID, b.TenantID, nil
			if err := insertFile(ctx, tx, &files[i]); err != nil {
				return err
			}
		}
		if err := insertBatchEvent(ctx, tx, b.BatchID, b.TenantID, ev); err != nil {
			return err
		}
		for _, o := range out {
			if err := r.enqueue(ctx, tx, b.TenantID, o); err != nil {
				return err
			}
		}
		return nil
	})
}

// Get implements batch.Repo.
func (r *BatchRepo) Get(ctx context.Context, id string) (*batch.Batch, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, batch.ErrNotFound
	}
	var b *batch.Batch
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		b, err = loadBatch(ctx, tx, id, false)
		return err
	})
	return b, err
}

// GetByDedupe implements batch.Repo.
func (r *BatchRepo) GetByDedupe(ctx context.Context, tenant, key string) (*batch.Batch, error) {
	var b batch.Batch
	err := r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM xic_batches WHERE tenant_id=$1 AND dedupe_key=$2`, tenant, key), &b)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, batch.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// List implements batch.Repo.
func (r *BatchRepo) List(ctx context.Context, f batch.Filter) ([]batch.Batch, error) {
	if f.Limit < 1 || f.Limit > 500 {
		f.Limit = 100
	}
	out := []batch.Batch{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var conn any
		if f.ConnectionID != "" {
			if _, err := uuid.Parse(f.ConnectionID); err != nil {
				return nil
			}
			conn = f.ConnectionID
		}
		rows, err := tx.Query(ctx, `SELECT `+batchCols+` FROM xic_batches WHERE ($1='' OR state=$1) AND ($2::uuid IS NULL OR connection_id=$2::uuid)
			ORDER BY received_at DESC, batch_id LIMIT $3`, f.State, conn, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b batch.Batch
			if err := scanBatch(rows, &b); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// Events implements batch.Repo.
func (r *BatchRepo) Events(ctx context.Context, id string) ([]batch.Event, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, batch.ErrNotFound
	}
	out := []batch.Event{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_batches WHERE batch_id=$1::uuid`, id).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return batch.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT kind, from_state, to_state, detail, actor, at FROM xic_batch_events WHERE batch_id=$1::uuid ORDER BY ord`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev batch.Event
			var d []byte
			if err := rows.Scan(&ev.Kind, &ev.From, &ev.To, &d, &ev.Actor, &ev.At); err != nil {
				return err
			}
			if len(d) > 0 {
				ev.Detail = json.RawMessage(d)
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

// AddEvent implements batch.Repo.
func (r *BatchRepo) AddEvent(ctx context.Context, id string, ev batch.Event) error {
	return r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return insertBatchEvent(ctx, tx, id, middleware.GetTenantID(ctx), ev)
	})
}

// Mutate implements batch.Repo.
func (r *BatchRepo) Mutate(ctx context.Context, id string, fn func(tx *batch.Tx) error) (*batch.Batch, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, batch.ErrNotFound
	}
	var out *batch.Batch
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		cur, err := loadBatch(ctx, pg, id, true)
		if err != nil {
			return err
		}
		w := *cur
		tx := &batch.Tx{B: &w}
		if err := fn(tx); err != nil {
			return err
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_batches SET mapping_version_id=$2::uuid, state=$3, reason_code=$4, xic_code=$5, parsed_rows=$6, admitted_rows=$7, accepted_rows=$8,
			pending_rows=$9, rejected_rows=$10, duplicate_rows=$11, quarantined_rows=$12, domain_rejected_rows=$13, variance_rows=$14, processing_done=$15, lease_until=$16,
			next_process_at=$17, process_attempts=$18, version=version+1, updated_at=now() WHERE batch_id=$1::uuid`, id, nulStr(w.MappingVersionID), w.State, w.ReasonCode,
			w.XICCode, w.ParsedRows, w.AdmittedRows, w.AcceptedRows, w.PendingRows, w.RejectedRows, w.DuplicateRows, w.QuarantinedRows, w.DomainRejected, w.VarianceRows,
			w.ProcessingDone, w.LeaseUntil, w.NextProcessAt, w.ProcessAttempts); err != nil {
			return err
		}
		if tx.Event != nil {
			if err := insertBatchEvent(ctx, pg, id, cur.TenantID, *tx.Event); err != nil {
				return err
			}
		}
		for _, o := range tx.Out {
			if err := r.enqueue(ctx, pg, cur.TenantID, o); err != nil {
				return err
			}
		}
		out, err = loadBatch(ctx, pg, id, false)
		return err
	})
	if err != nil {
		return nil, batchErr(err)
	}
	return out, nil
}

const fileCols = `file_id::text, batch_id::text, tenant_id, name, ordinal, declared_sha256, declared_size, compression, actual_sha256, actual_size, declared_records,
	parsed_rows, state, reason_code, content_expires_at, (content IS NOT NULL)`

// Files implements batch.Repo.
func (r *BatchRepo) Files(ctx context.Context, batchID string, withContent bool) ([]batch.File, error) {
	if _, err := uuid.Parse(batchID); err != nil {
		return nil, batch.ErrNotFound
	}
	var out []batch.File
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_batches WHERE batch_id=$1::uuid`, batchID).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return batch.ErrNotFound
			}
			return err
		}
		q := `SELECT ` + fileCols
		if withContent {
			q += `, content`
		}
		rows, err := tx.Query(ctx, q+` FROM xic_batch_files WHERE batch_id=$1::uuid ORDER BY ordinal, name`, batchID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f batch.File
			dest := []any{&f.FileID, &f.BatchID, &f.TenantID, &f.Name, &f.Ordinal, &f.DeclaredSHA256, &f.DeclaredSize, &f.Compression, &f.ActualSHA256, &f.ActualSize,
				&f.DeclaredRecords, &f.ParsedRows, &f.State, &f.ReasonCode, &f.ContentExpiresAt, &f.ContentRetained}
			if withContent {
				dest = append(dest, &f.Content)
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateFile implements batch.Repo: the state and reason of a file, the rows it held, and (when the file no longer
// keeps its body) the removal of that body.
func (r *BatchRepo) UpdateFile(ctx context.Context, f batch.File) error {
	return r.p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE xic_batch_files SET state=$2, reason_code=$3, parsed_rows=$4, content = CASE WHEN $5 THEN content ELSE NULL END WHERE file_id=$1::uuid`,
			f.FileID, f.State, f.ReasonCode, f.ParsedRows, f.ContentRetained)
		return err
	})
}

// AddRows implements batch.Repo.
func (r *BatchRepo) AddRows(ctx context.Context, rows []batch.Row) error {
	if len(rows) == 0 {
		return nil
	}
	return r.p.withTenant(ctx, func(tx pgx.Tx) error {
		for _, x := range rows {
			if _, err := tx.Exec(ctx, `INSERT INTO xic_batch_rows (row_id, batch_id, file_id, tenant_id, row_no, source_row_hash, exchange_id, state, reason_code, updated_at)
				VALUES ($1::uuid,$2::uuid,$3::uuid,$4,$5,$6,$7::uuid,$8,$9,$10) ON CONFLICT (file_id, row_no) DO NOTHING`,
				x.RowID, x.BatchID, x.FileID, x.TenantID, x.RowNo, x.SourceRowHash, nulStr(x.ExchangeID), x.State, x.ReasonCode, x.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// Rows implements batch.Repo.
func (r *BatchRepo) Rows(ctx context.Context, batchID string, f batch.RowFilter) ([]batch.Row, error) {
	if _, err := uuid.Parse(batchID); err != nil {
		return nil, batch.ErrNotFound
	}
	states := append([]string{}, f.States...)
	if f.State != "" {
		states = append(states, f.State)
	}
	var fileID any
	if f.FileID != "" {
		if _, err := uuid.Parse(f.FileID); err != nil {
			return nil, nil
		}
		fileID = f.FileID
	}
	limit := f.Limit
	if limit < 1 || limit > 100000 {
		limit = 100000
	}
	out := []batch.Row{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_batches WHERE batch_id=$1::uuid`, batchID).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return batch.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT row_id::text, batch_id::text, file_id::text, tenant_id, row_no, source_row_hash, COALESCE(exchange_id::text, ''), state, reason_code, updated_at
			FROM xic_batch_rows WHERE batch_id=$1::uuid AND (cardinality($2::text[]) = 0 OR state = ANY($2::text[])) AND ($3::uuid IS NULL OR file_id=$3::uuid)
			AND (cardinality($4::int[]) = 0 OR row_no = ANY($4::int[])) AND row_no > $5 ORDER BY row_no, file_id LIMIT $6`,
			batchID, states, fileID, intsOf(f.RowNos), f.After, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x batch.Row
			if err := rows.Scan(&x.RowID, &x.BatchID, &x.FileID, &x.TenantID, &x.RowNo, &x.SourceRowHash, &x.ExchangeID, &x.State, &x.ReasonCode, &x.UpdatedAt); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

func intsOf(v []int) []int32 {
	out := make([]int32, 0, len(v))
	for _, n := range v {
		out = append(out, int32(n))
	}
	return out
}

// RowCount implements batch.Repo.
func (r *BatchRepo) RowCount(ctx context.Context, batchID, fileID string) (int, error) {
	if _, err := uuid.Parse(batchID); err != nil {
		return 0, batch.ErrNotFound
	}
	n := 0
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM xic_batch_rows WHERE batch_id=$1::uuid AND file_id=$2::uuid`, batchID, fileID).Scan(&n)
	})
	return n, err
}

// RowStateCounts implements batch.Repo.
func (r *BatchRepo) RowStateCounts(ctx context.Context, batchID string) (map[string]int, error) {
	if _, err := uuid.Parse(batchID); err != nil {
		return nil, batch.ErrNotFound
	}
	out := map[string]int{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT state, count(*) FROM xic_batch_rows WHERE batch_id=$1::uuid GROUP BY state`, batchID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			var n int
			if err := rows.Scan(&s, &n); err != nil {
				return err
			}
			out[s] = n
		}
		return rows.Err()
	})
	return out, err
}

// UpdateRows implements batch.Repo.
func (r *BatchRepo) UpdateRows(ctx context.Context, rows []batch.Row) error {
	if len(rows) == 0 {
		return nil
	}
	return r.p.withTenant(ctx, func(tx pgx.Tx) error {
		for _, x := range rows {
			if _, err := tx.Exec(ctx, `UPDATE xic_batch_rows SET state=$2, reason_code=$3, updated_at=$4 WHERE row_id=$1::uuid`, x.RowID, x.State, x.ReasonCode, x.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *BatchRepo) workerTx(ctx context.Context, purge bool, fn func(pgx.Tx) error) error {
	tx, err := r.p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true), set_config('app.xic_worker', 'true', true)`); err != nil {
		return err
	}
	if purge {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.xic_purge', 'true', true)`); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DueForProcessing implements batch.Repo.
func (r *BatchRepo) DueForProcessing(ctx context.Context, now time.Time, limit int) ([]batch.DueRef, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	var out []batch.DueRef
	err := r.workerTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, batch_id::text FROM xic_batches WHERE
			(state = 'RECEIVED' AND next_process_at <= $1)
			OR (state IN ('VALIDATING', 'ACCEPTED') AND (lease_until IS NULL OR lease_until <= $1))
			OR (state IN ('PROCESSING', 'PARTIAL') AND ((next_process_at IS NOT NULL AND next_process_at <= $1) OR (NOT processing_done AND (lease_until IS NULL OR lease_until <= $1))))
			ORDER BY COALESCE(next_process_at, received_at), batch_id LIMIT $2`, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d batch.DueRef
			if err := rows.Scan(&d.TenantID, &d.ID); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// PurgeContent implements batch.Repo.
func (r *BatchRepo) PurgeContent(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 5000 {
		limit = 500
	}
	n := 0
	err := r.workerTx(ctx, true, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE xic_batch_files SET content=NULL WHERE file_id IN (
			SELECT file_id FROM xic_batch_files WHERE content IS NOT NULL AND content_expires_at <= LEAST($1::timestamptz, now()) ORDER BY content_expires_at LIMIT $2)`, now, limit)
		n = int(tag.RowsAffected())
		return err
	})
	return n, err
}

// RecordSignal implements batch.Repo.
func (r *BatchRepo) RecordSignal(ctx context.Context, tenant string, ev batch.OutEvent) error {
	return r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return r.enqueue(ctx, tx, tenant, ev)
	})
}

// ── replay jobs ──────────────────────────────────────────────────────────────

const replayCols = `replay_id::text, tenant_id, COALESCE(connection_id::text, ''), operation_id, COALESCE(batch_id::text, ''), scope_kind, scope, reason_code, reason, dry_run,
	COALESCE(dry_run_of::text, ''), use_current_mapping, state, risk, risk_reasons, contract_version_id::text, COALESCE(mapping_version_id::text, ''), selected_items, projected,
	result, requested_by, COALESCE(decided_by, ''), decided_at, decision_note, correlation_id, lease_until, version, created_at, updated_at, started_at, finished_at`

func scanReplay(row pgx.Row, j *batch.ReplayJob) error {
	var scope, rr, proj, res []byte
	if err := row.Scan(&j.ReplayID, &j.TenantID, &j.ConnectionID, &j.OperationID, &j.BatchID, &j.ScopeKind, &scope, &j.ReasonCode, &j.Reason, &j.DryRun, &j.DryRunOf,
		&j.UseCurrentMapping, &j.State, &j.Risk, &rr, &j.ContractVersionID, &j.MappingVersionID, &j.SelectedItems, &proj, &res, &j.RequestedBy, &j.DecidedBy, &j.DecidedAt,
		&j.DecisionNote, &j.CorrelationID, &j.LeaseUntil, &j.Version, &j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.FinishedAt); err != nil {
		return err
	}
	j.Scope = json.RawMessage(scope)
	if len(rr) > 0 && string(rr) != "null" {
		_ = json.Unmarshal(rr, &j.RiskReasons)
	}
	if len(proj) > 0 && string(proj) != "null" {
		j.Projected = json.RawMessage(proj)
	}
	if len(res) > 0 && string(res) != "null" {
		j.Result = json.RawMessage(res)
	}
	return nil
}

func replayErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch pgCode(err) {
	case "23000":
		switch {
		case strings.Contains(msg, "cannot go") || strings.Contains(msg, "is final"):
			return batch.ErrTransition
		}
	case "23514":
		if strings.Contains(msg, "xic_rj_distinct_people") || strings.Contains(msg, "xic_rj_high_needs_approver") {
			return batch.ErrSelfAction
		}
	}
	return err
}

// InsertReplay implements batch.Repo.
func (r *BatchRepo) InsertReplay(ctx context.Context, j *batch.ReplayJob, items []batch.ReplayItem, ev batch.OutEvent) error {
	rr, _ := json.Marshal(j.RiskReasons)
	var proj, res any
	if len(j.Projected) > 0 {
		proj = []byte(j.Projected)
	}
	if len(j.Result) > 0 {
		res = []byte(j.Result)
	}
	return r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO xic_replay_jobs (replay_id, tenant_id, connection_id, operation_id, batch_id, scope_kind, scope, reason_code, reason, dry_run, dry_run_of,
			use_current_mapping, state, risk, risk_reasons, contract_version_id, mapping_version_id, selected_items, projected, result, requested_by, decided_by, decided_at,
			decision_note, correlation_id, version, created_at, updated_at, started_at, finished_at)
			VALUES ($1::uuid,$2,$3::uuid,$4,$5::uuid,$6,$7,$8,$9,$10,$11::uuid,$12,$13,$14,$15,$16::uuid,$17::uuid,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)`,
			j.ReplayID, j.TenantID, nulStr(j.ConnectionID), j.OperationID, nulStr(j.BatchID), j.ScopeKind, []byte(j.Scope), j.ReasonCode, j.Reason, j.DryRun, nulStr(j.DryRunOf),
			j.UseCurrentMapping, j.State, j.Risk, rr, j.ContractVersionID, nulStr(j.MappingVersionID), j.SelectedItems, proj, res, j.RequestedBy, nulStr(j.DecidedBy), j.DecidedAt,
			j.DecisionNote, j.CorrelationID, j.Version, j.CreatedAt, j.UpdatedAt, j.StartedAt, j.FinishedAt); err != nil {
			return err
		}
		for _, it := range items {
			if _, err := tx.Exec(ctx, `INSERT INTO xic_replay_items (replay_id, tenant_id, exchange_id, before_state, before_class, before_reason, after_state, after_class, after_reason,
				changed, applied, skipped, done) VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, j.ReplayID, j.TenantID, it.ExchangeID, it.BeforeState,
				it.BeforeClass, it.BeforeReason, it.AfterState, it.AfterClass, it.AfterReason, it.Changed, it.Applied, it.Skipped, it.Done); err != nil {
				return err
			}
		}
		return r.enqueue(ctx, tx, j.TenantID, ev)
	})
}

// GetReplay implements batch.Repo.
func (r *BatchRepo) GetReplay(ctx context.Context, id string) (*batch.ReplayJob, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, batch.ErrReplayNotFound
	}
	var j batch.ReplayJob
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanReplay(tx.QueryRow(ctx, `SELECT `+replayCols+` FROM xic_replay_jobs WHERE replay_id=$1::uuid`, id), &j)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, batch.ErrReplayNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// ListReplays implements batch.Repo.
func (r *BatchRepo) ListReplays(ctx context.Context, f batch.ReplayFilter) ([]batch.ReplayJob, error) {
	if f.Limit < 1 || f.Limit > 500 {
		f.Limit = 100
	}
	out := []batch.ReplayJob{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var bid any
		if f.BatchID != "" {
			if _, err := uuid.Parse(f.BatchID); err != nil {
				return nil
			}
			bid = f.BatchID
		}
		rows, err := tx.Query(ctx, `SELECT `+replayCols+` FROM xic_replay_jobs WHERE ($1='' OR state=$1) AND ($2::uuid IS NULL OR batch_id=$2::uuid)
			ORDER BY created_at DESC, replay_id LIMIT $3`, f.State, bid, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j batch.ReplayJob
			if err := scanReplay(rows, &j); err != nil {
				return err
			}
			out = append(out, j)
		}
		return rows.Err()
	})
	return out, err
}

// ReplayItems implements batch.Repo.
func (r *BatchRepo) ReplayItems(ctx context.Context, id string, limit int) ([]batch.ReplayItem, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, batch.ErrReplayNotFound
	}
	if limit < 1 || limit > 200000 {
		limit = 200000
	}
	out := []batch.ReplayItem{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_replay_jobs WHERE replay_id=$1::uuid`, id).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return batch.ErrReplayNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT replay_id::text, tenant_id, exchange_id::text, before_state, before_class, before_reason, after_state, after_class, after_reason, changed,
			applied, skipped, done FROM xic_replay_items WHERE replay_id=$1::uuid ORDER BY exchange_id LIMIT $2`, id, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it batch.ReplayItem
			if err := rows.Scan(&it.ReplayID, &it.TenantID, &it.ExchangeID, &it.BeforeState, &it.BeforeClass, &it.BeforeReason, &it.AfterState, &it.AfterClass, &it.AfterReason,
				&it.Changed, &it.Applied, &it.Skipped, &it.Done); err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// MutateReplay implements batch.Repo.
func (r *BatchRepo) MutateReplay(ctx context.Context, id string, fn func(tx *batch.ReplayTx) error) (*batch.ReplayJob, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, batch.ErrReplayNotFound
	}
	var out *batch.ReplayJob
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		var cur batch.ReplayJob
		if err := scanReplay(pg.QueryRow(ctx, `SELECT `+replayCols+` FROM xic_replay_jobs WHERE replay_id=$1::uuid FOR UPDATE`, id), &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return batch.ErrReplayNotFound
			}
			return err
		}
		w := cur
		tx := &batch.ReplayTx{J: &w}
		if err := fn(tx); err != nil {
			return err
		}
		var res any
		if len(w.Result) > 0 {
			res = []byte(w.Result)
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_replay_jobs SET state=$2, decided_by=$3, decided_at=$4, decision_note=$5, result=$6, lease_until=$7, started_at=$8, finished_at=$9,
			version=version+1, updated_at=now() WHERE replay_id=$1::uuid`, id, w.State, nulStr(w.DecidedBy), w.DecidedAt, w.DecisionNote, res, w.LeaseUntil, w.StartedAt, w.FinishedAt); err != nil {
			return err
		}
		for _, o := range tx.Out {
			if err := r.enqueue(ctx, pg, cur.TenantID, o); err != nil {
				return err
			}
		}
		return scanReplay(pg.QueryRow(ctx, `SELECT `+replayCols+` FROM xic_replay_jobs WHERE replay_id=$1::uuid`, id), func() *batch.ReplayJob { out = &batch.ReplayJob{}; return out }())
	})
	if err != nil {
		return nil, replayErr(err)
	}
	return out, nil
}

// DueReplays implements batch.Repo.
func (r *BatchRepo) DueReplays(ctx context.Context, limit int) ([]batch.DueRef, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	var out []batch.DueRef
	err := r.workerTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, replay_id::text FROM xic_replay_jobs WHERE state = 'APPROVED' OR (state = 'RUNNING' AND (lease_until IS NULL OR lease_until <= now()))
			ORDER BY created_at LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d batch.DueRef
			if err := rows.Scan(&d.TenantID, &d.ID); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateReplayItems implements batch.Repo.
func (r *BatchRepo) UpdateReplayItems(ctx context.Context, items []batch.ReplayItem) error {
	if len(items) == 0 {
		return nil
	}
	return r.p.withTenant(ctx, func(tx pgx.Tx) error {
		for _, it := range items {
			if _, err := tx.Exec(ctx, `UPDATE xic_replay_items SET before_state=$3, before_class=$4, before_reason=$5, after_state=$6, after_class=$7, after_reason=$8, changed=$9,
				applied=$10, skipped=$11, done=$12 WHERE replay_id=$1::uuid AND exchange_id=$2::uuid`, it.ReplayID, it.ExchangeID, it.BeforeState, it.BeforeClass, it.BeforeReason,
				it.AfterState, it.AfterClass, it.AfterReason, it.Changed, it.Applied, it.Skipped, it.Done); err != nil {
				return err
			}
		}
		return nil
	})
}
