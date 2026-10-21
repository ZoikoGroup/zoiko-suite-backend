package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/data-ingestion-svc/internal/imports"
	"zoiko.io/data-ingestion-svc/internal/outbox"
)

// This file is the PostgreSQL implementation of imports.Repo (INT-06). Every method except the
// two discovery methods runs in a tenant-scoped transaction under row-level security.

var _ imports.Repo = (*PgStore)(nil)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// importErr translates the database's own refusals into the engine's typed errors.
func importErr(err error) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case "23505":
		if strings.Contains(pe.ConstraintName, "import_batches_no_duplicate") {
			return imports.ErrDuplicateBatch
		}
	case "23514":
		switch {
		case strings.Contains(pe.ConstraintName, "import_batch_material_sod"):
			return imports.ErrSelfApproval
		case strings.Contains(pe.ConstraintName, "import_profile_certified_by_other"):
			return imports.ErrProfileSelfCertify
		}
		return fmt.Errorf("%w: %s", imports.ErrInvalidState, pe.ConstraintName)
	case "23000":
		return fmt.Errorf("%w: %s", imports.ErrInvalidState, pe.Message)
	}
	return err
}

func (s *PgStore) imp(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error { return importErr(fn(tx)) })
}

func jsonOrNil(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func emitEvents(ctx context.Context, tx pgx.Tx, tenantID, batchID string, events []imports.Event) error {
	for _, e := range events {
		tid := tenantID
		if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "import_batch", AggregateID: batchID, EventType: e.Type, Payload: e.Payload, TenantID: &tid}); err != nil {
			return err
		}
	}
	return nil
}

func insertImportLog(ctx context.Context, tx pgx.Tx, e imports.LogEntry) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	d, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx, `
		INSERT INTO import_batch_log (tenant_id, batch_id, kind, actor, from_status, to_status, reason, detail, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.TenantID, e.BatchID, e.Kind, e.Actor, nilIfEmpty(string(e.FromStatus)), nilIfEmpty(string(e.ToStatus)), clip(e.Reason, 200), d, e.CorrelationID)
	return err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── profiles ────────────────────────────────────────────────────────────────

const profileCols = `profile_key, version, status, spec, spec_hash, created_by, COALESCE(certified_by, ''), certified_at, created_at`

func scanProfile(r pgx.Row) (*imports.Profile, error) {
	var p imports.Profile
	var st string
	var raw []byte
	if err := r.Scan(&p.Key, &p.Version, &st, &raw, &p.SpecHash, &p.CreatedBy, &p.CertifiedBy, &p.CertifiedAt, &p.CreatedAt); err != nil {
		return nil, err
	}
	p.Status = imports.ProfileStatus(st)
	if err := json.Unmarshal(raw, &p.Spec); err != nil {
		return nil, fmt.Errorf("stored profile spec: %w", err)
	}
	return &p, nil
}

func (s *PgStore) PutProfile(ctx context.Context, p *imports.Profile) (*imports.Profile, error) {
	spec, _ := json.Marshal(p.Spec)
	var out *imports.Profile
	err := s.imp(ctx, imports.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `
			INSERT INTO import_profiles (profile_key, version, import_type, status, material, spec, spec_hash, created_by)
			VALUES ($1, $2, $3, 'DRAFT', $4, $5, $6, $7) RETURNING `+profileCols,
			p.Key, p.Version, p.Spec.ImportType, p.Spec.Material, spec, p.SpecHash, p.CreatedBy))
		return err
	})
	if pgCode(err) == "23505" {
		return nil, fmt.Errorf("%w: that profile version already exists; retry", imports.ErrValidation)
	}
	return out, err
}

func (s *PgStore) GetProfile(ctx context.Context, key string, version int) (*imports.Profile, error) {
	var out *imports.Profile
	err := s.imp(ctx, imports.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		if version == 0 {
			out, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM import_profiles
				WHERE profile_key = $1 AND status = 'CERTIFIED' ORDER BY version DESC LIMIT 1`, key))
		} else {
			out, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM import_profiles WHERE profile_key = $1 AND version = $2`, key, version))
		}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, imports.ErrProfileNotFound
	}
	return out, err
}

func (s *PgStore) CertifyProfile(ctx context.Context, key string, version int, by string, at time.Time) (*imports.Profile, error) {
	var out *imports.Profile
	err := s.imp(ctx, imports.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `
			UPDATE import_profiles SET status = 'CERTIFIED', certified_by = $3, certified_at = $4
			WHERE profile_key = $1 AND version = $2 AND status = 'DRAFT' RETURNING `+profileCols, key, version, by, at))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, imports.ErrInvalidState
	}
	return out, err
}

func (s *PgStore) SetProfileStatus(ctx context.Context, key string, version int, to imports.ProfileStatus) (*imports.Profile, error) {
	var out *imports.Profile
	err := s.imp(ctx, imports.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `UPDATE import_profiles SET status = $3 WHERE profile_key = $1 AND version = $2 RETURNING `+profileCols, key, version, string(to)))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, imports.ErrProfileNotFound
	}
	return out, err
}

func (s *PgStore) ListProfiles(ctx context.Context, key string) ([]imports.Profile, error) {
	var out []imports.Profile
	err := s.imp(ctx, imports.PlatformScopeID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+profileCols+` FROM import_profiles WHERE ($1 = '' OR profile_key = $1) ORDER BY profile_key, version`, key)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProfile(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

// ── batches ─────────────────────────────────────────────────────────────────

const batchCols = `batch_id, tenant_id, legal_entity_id, profile_key, profile_version, material, source_system, jurisdiction_context, file_name,
	declared_content_type, declared_sha256, declared_rows, COALESCE(declared_control_total, ''), status, status_reason, version,
	chunk_count, size_bytes, COALESCE(content_sha256, ''), COALESCE(chain_hash, ''), validate_requested_at,
	COALESCE(scan_engine, ''), COALESCE(scan_verdict, ''), COALESCE(scan_signature, ''), scanned_at,
	row_count, valid_rows, invalid_rows, COALESCE(computed_control_total, ''), COALESCE(mapping_hash, ''),
	preview, COALESCE(preview_fingerprint, ''), previewed_at, COALESCE(approved_fingerprint, ''), COALESCE(approved_by, ''), approved_at,
	COALESCE(executed_by, ''), executed_at, completed_at, imported_items, quarantined_items, failed_items, skipped_items,
	reconciliation, reconciled_at, created_by, correlation_id, created_at, updated_at`

func scanBatch(r pgx.Row) (*imports.Batch, error) {
	var b imports.Batch
	var st string
	var preview, rec []byte
	if err := r.Scan(&b.BatchID, &b.TenantID, &b.LegalEntityID, &b.ProfileKey, &b.ProfileVersion, &b.Material, &b.SourceSystem, &b.JurisdictionContext, &b.FileName,
		&b.DeclaredContentType, &b.DeclaredSHA256, &b.DeclaredRows, &b.DeclaredControl, &st, &b.StatusReason, &b.Version,
		&b.ChunkCount, &b.SizeBytes, &b.ContentSHA256, &b.ChainHash, &b.ValidateRequestedAt,
		&b.ScanEngine, &b.ScanVerdict, &b.ScanSignature, &b.ScannedAt,
		&b.RowCount, &b.ValidRows, &b.InvalidRows, &b.ComputedControl, &b.MappingHash,
		&preview, &b.PreviewFingerprint, &b.PreviewedAt, &b.ApprovedFingerprint, &b.ApprovedBy, &b.ApprovedAt,
		&b.ExecutedBy, &b.ExecutedAt, &b.CompletedAt, &b.ImportedItems, &b.QuarantinedItems, &b.FailedItems, &b.SkippedItems,
		&rec, &b.ReconciledAt, &b.CreatedBy, &b.CorrelationID, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	b.Status = imports.Status(st)
	if len(preview) > 0 {
		b.Preview = &imports.Preview{}
		if err := json.Unmarshal(preview, b.Preview); err != nil {
			return nil, fmt.Errorf("stored preview: %w", err)
		}
	}
	if len(rec) > 0 {
		b.Reconciliation = &imports.Reconciliation{}
		if err := json.Unmarshal(rec, b.Reconciliation); err != nil {
			return nil, fmt.Errorf("stored reconciliation: %w", err)
		}
	}
	return &b, nil
}

func (s *PgStore) CreateBatch(ctx context.Context, b *imports.Batch, claim domain.IdempotencyClaim) (*imports.Batch, error) {
	var out *imports.Batch
	err := s.imp(ctx, b.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		var err error
		out, err = scanBatch(tx.QueryRow(ctx, `
			INSERT INTO import_batches (batch_id, tenant_id, legal_entity_id, profile_key, profile_version, material, source_system, file_name,
				declared_content_type, declared_sha256, declared_rows, declared_control_total, status, created_by, correlation_id, jurisdiction_context)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'CREATED', $13, $14, $15) RETURNING `+batchCols,
			b.BatchID, b.TenantID, b.LegalEntityID, b.ProfileKey, b.ProfileVersion, b.Material, b.SourceSystem, b.FileName,
			b.DeclaredContentType, b.DeclaredSHA256, b.DeclaredRows, nilIfEmpty(b.DeclaredControl), b.CreatedBy, b.CorrelationID, b.JurisdictionContext))
		if err != nil {
			return err
		}
		return insertImportLog(ctx, tx, imports.LogEntry{TenantID: b.TenantID, BatchID: out.BatchID, Kind: "CREATED", Actor: b.CreatedBy,
			ToStatus: imports.StatusCreated, CorrelationID: b.CorrelationID})
	})
	return out, err
}

func (s *PgStore) GetBatch(ctx context.Context, tenantID, batchID string) (*imports.Batch, error) {
	var out *imports.Batch
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM import_batches WHERE batch_id = $1 AND tenant_id = $2`, batchID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, imports.ErrNotFound
	}
	return out, err
}

func (s *PgStore) ListBatches(ctx context.Context, tenantID string, status imports.Status, limit int) ([]imports.Batch, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []imports.Batch
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+batchCols+` FROM import_batches WHERE tenant_id = $1 AND ($2 = '' OR status = $2)
			ORDER BY created_at DESC LIMIT $3`, tenantID, string(status), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBatch(rows)
			if err != nil {
				return err
			}
			out = append(out, *b)
		}
		return rows.Err()
	})
	return out, err
}

// Transition applies one atomic batch change.
func (s *PgStore) Transition(ctx context.Context, t imports.Transition) (*imports.Batch, error) {
	var out *imports.Batch
	err := s.imp(ctx, t.TenantID, func(tx pgx.Tx) error {
		cur, err := scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM import_batches WHERE batch_id = $1 AND tenant_id = $2 FOR UPDATE`, t.BatchID, t.TenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return imports.ErrNotFound
		}
		if err != nil {
			return err
		}
		if len(t.From) > 0 {
			ok := false
			for _, f := range t.From {
				if cur.Status == f {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("%w: the batch is %s", imports.ErrInvalidState, cur.Status)
			}
		}
		if t.ExpectedVersion > 0 && cur.Version != t.ExpectedVersion {
			return imports.ErrVersionConflict
		}
		sets := []string{"version = version + 1"}
		args := []any{t.BatchID, t.TenantID}
		add := func(col string, v any) {
			args = append(args, v)
			sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
		}
		if t.To != "" {
			add("status", string(t.To))
			add("status_reason", clip(t.Reason, 80))
		}
		p := t.Patch
		if p.ChunkCount != nil {
			add("chunk_count", *p.ChunkCount)
		}
		if p.SizeBytes != nil {
			add("size_bytes", *p.SizeBytes)
		}
		if p.ContentSHA256 != nil {
			add("content_sha256", *p.ContentSHA256)
		}
		if p.ChainHash != nil {
			add("chain_hash", *p.ChainHash)
		}
		if p.ValidateRequested != nil {
			if *p.ValidateRequested {
				sets = append(sets, "validate_requested_at = now()")
			} else {
				sets = append(sets, "validate_requested_at = NULL")
			}
		}
		if p.ScanEngine != nil {
			add("scan_engine", clip(*p.ScanEngine, 120))
		}
		if p.ScanVerdict != nil {
			add("scan_verdict", *p.ScanVerdict)
		}
		if p.ScanSignature != nil {
			add("scan_signature", clip(*p.ScanSignature, 200))
		}
		if p.ScannedAt != nil {
			add("scanned_at", *p.ScannedAt)
		}
		if p.Rows != nil {
			add("row_count", *p.Rows)
		}
		if p.Valid != nil {
			add("valid_rows", *p.Valid)
		}
		if p.Invalid != nil {
			add("invalid_rows", *p.Invalid)
		}
		if p.ComputedControl != nil {
			add("computed_control_total", *p.ComputedControl)
		}
		if p.MappingHash != nil {
			add("mapping_hash", *p.MappingHash)
		}
		if p.Preview != nil {
			add("preview", jsonOrNil(p.Preview))
		}
		if p.PreviewFingerprint != nil {
			add("preview_fingerprint", *p.PreviewFingerprint)
		}
		if p.PreviewedAt != nil {
			add("previewed_at", *p.PreviewedAt)
		}
		if p.ApprovedFingerprint != nil {
			add("approved_fingerprint", *p.ApprovedFingerprint)
		}
		if p.ApprovedBy != nil {
			add("approved_by", *p.ApprovedBy)
		}
		if p.ApprovedAt != nil {
			add("approved_at", *p.ApprovedAt)
		}
		if p.ExecutedBy != nil {
			add("executed_by", *p.ExecutedBy)
		}
		if p.ExecutedAt != nil {
			add("executed_at", *p.ExecutedAt)
		}
		if p.CompletedAt != nil {
			add("completed_at", *p.CompletedAt)
		}
		if p.Imported != nil {
			add("imported_items", *p.Imported)
		}
		if p.Quarantined != nil {
			add("quarantined_items", *p.Quarantined)
		}
		if p.Failed != nil {
			add("failed_items", *p.Failed)
		}
		if p.Skipped != nil {
			add("skipped_items", *p.Skipped)
		}
		if p.Reconciliation != nil {
			add("reconciliation", jsonOrNil(p.Reconciliation))
		}
		if p.ReconciledAt != nil {
			add("reconciled_at", *p.ReconciledAt)
		}
		out, err = scanBatch(tx.QueryRow(ctx, `UPDATE import_batches SET `+strings.Join(sets, ", ")+
			` WHERE batch_id = $1 AND tenant_id = $2 RETURNING `+batchCols, args...))
		if err != nil {
			return err
		}
		t.Log.TenantID, t.Log.BatchID, t.Log.FromStatus, t.Log.ToStatus = t.TenantID, t.BatchID, cur.Status, out.Status
		if err := insertImportLog(ctx, tx, t.Log); err != nil {
			return err
		}
		return emitEvents(ctx, tx, t.TenantID, t.BatchID, withStatus(t.Events, out))
	})
	return out, err
}

// withStatus stamps each event with the status the batch has after the change.
func withStatus(events []imports.Event, b *imports.Batch) []imports.Event {
	out := make([]imports.Event, len(events))
	for i, e := range events {
		p := make(map[string]any, len(e.Payload)+1)
		for k, v := range e.Payload {
			p[k] = v
		}
		p["status"] = string(b.Status)
		out[i] = imports.Event{Type: e.Type, Payload: p}
	}
	return out
}

func (s *PgStore) ListLog(ctx context.Context, tenantID, batchID string, limit int) ([]imports.LogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []imports.LogEntry
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT log_id, tenant_id, batch_id, kind, actor, COALESCE(from_status, ''), COALESCE(to_status, ''), reason, detail,
			correlation_id, created_at FROM import_batch_log WHERE tenant_id = $1 AND batch_id = $2 ORDER BY log_id LIMIT $3`, tenantID, batchID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e imports.LogEntry
			var from, to string
			var detail []byte
			if err := rows.Scan(&e.LogID, &e.TenantID, &e.BatchID, &e.Kind, &e.Actor, &from, &to, &e.Reason, &detail, &e.CorrelationID, &e.CreatedAt); err != nil {
				return err
			}
			e.FromStatus, e.ToStatus = imports.Status(from), imports.Status(to)
			_ = json.Unmarshal(detail, &e.Detail)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ── content ─────────────────────────────────────────────────────────────────

func (s *PgStore) PutChunk(ctx context.Context, tenantID, batchID string, index int, content []byte, sha string) (bool, error) {
	var stored bool
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO import_chunks (batch_id, chunk_index, tenant_id, content, size_bytes, chunk_sha256)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (batch_id, chunk_index) DO NOTHING`, batchID, index, tenantID, content, len(content), sha)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			stored = true
			return nil
		}
		var existing string
		if err := tx.QueryRow(ctx, `SELECT chunk_sha256 FROM import_chunks WHERE batch_id = $1 AND chunk_index = $2`, batchID, index).Scan(&existing); err != nil {
			return err
		}
		if existing != sha {
			return imports.ErrChunkConflict
		}
		return nil // the same bytes again: an upload that resumed
	})
	return stored, err
}

func (s *PgStore) Chunks(ctx context.Context, tenantID, batchID string) ([]imports.ChunkInfo, error) {
	var out []imports.ChunkInfo
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT chunk_index, size_bytes, chunk_sha256 FROM import_chunks WHERE batch_id = $1 AND tenant_id = $2 ORDER BY chunk_index`, batchID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c imports.ChunkInfo
			if err := rows.Scan(&c.Index, &c.Size, &c.SHA256); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// chunkReader streams a batch's content one chunk at a time, each in its own short transaction,
// so reading a large file never holds a connection or a transaction open.
type chunkReader struct {
	ctx      context.Context
	s        *PgStore
	tenantID string
	batchID  string
	indexes  []int
	next     int
	cur      []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.next >= len(r.indexes) {
			return 0, io.EOF
		}
		idx := r.indexes[r.next]
		r.next++
		var content []byte
		err := r.s.imp(r.ctx, r.tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(r.ctx, `SELECT content FROM import_chunks WHERE batch_id = $1 AND chunk_index = $2 AND tenant_id = $3`, r.batchID, idx, r.tenantID).Scan(&content)
		})
		if err != nil {
			return 0, err
		}
		r.cur = content
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

func (r *chunkReader) Close() error { return nil }

func (s *PgStore) OpenContent(ctx context.Context, tenantID, batchID string) (io.ReadCloser, error) {
	chunks, err := s.Chunks(ctx, tenantID, batchID)
	if err != nil {
		return nil, err
	}
	idx := make([]int, len(chunks))
	for i, c := range chunks {
		idx[i] = c.Index
	}
	return &chunkReader{ctx: ctx, s: s, tenantID: tenantID, batchID: batchID, indexes: idx}, nil
}

// ── items ───────────────────────────────────────────────────────────────────

const itemCols = `batch_id, row_no, tenant_id, row_hash, item_values, status, error_codes, idempotency_key, attempts, next_attempt_at,
	locked_until, domain_status, COALESCE(result_ref, ''), COALESCE(last_error_class, ''), COALESCE(quarantine_reason, ''),
	COALESCE(quarantine_owner, ''), COALESCE(quarantined_by, ''), updated_at`

func scanItem(r pgx.Row) (*imports.Item, error) {
	var it imports.Item
	var st string
	var vals, errs []byte
	if err := r.Scan(&it.BatchID, &it.RowNo, &it.TenantID, &it.RowHash, &vals, &st, &errs, &it.IdempotencyKey, &it.Attempts, &it.NextAttemptAt,
		&it.LockedUntil, &it.DomainStatus, &it.ResultRef, &it.LastErrorClass, &it.QuarantineReason, &it.QuarantineOwner, &it.QuarantinedBy, &it.UpdatedAt); err != nil {
		return nil, err
	}
	it.Status = imports.ItemStatus(st)
	d := json.NewDecoder(strings.NewReader(string(vals)))
	d.UseNumber()
	if err := d.Decode(&it.Values); err != nil {
		return nil, fmt.Errorf("stored item values: %w", err)
	}
	if err := json.Unmarshal(errs, &it.ErrorCodes); err != nil {
		return nil, fmt.Errorf("stored item errors: %w", err)
	}
	return &it, nil
}

func (s *PgStore) ResetItems(ctx context.Context, tenantID, batchID string) error {
	return s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM import_items WHERE batch_id = $1 AND tenant_id = $2`, batchID, tenantID)
		return err
	})
}

func (s *PgStore) InsertItems(ctx context.Context, tenantID, batchID string, items []imports.Item) error {
	return s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, it := range items {
			vals, _ := json.Marshal(it.Values)
			errs := it.ErrorCodes
			if errs == nil {
				errs = []imports.RowError{}
			}
			ej, _ := json.Marshal(errs)
			b.Queue(`INSERT INTO import_items (batch_id, row_no, tenant_id, row_hash, item_values, status, error_codes, idempotency_key)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, batchID, it.RowNo, tenantID, it.RowHash, vals, string(it.Status), ej, it.IdempotencyKey)
		}
		br := tx.SendBatch(ctx, b)
		defer br.Close()
		for range items {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PgStore) GetItem(ctx context.Context, tenantID, batchID string, rowNo int) (*imports.Item, error) {
	var out *imports.Item
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM import_items WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3`, batchID, rowNo, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, imports.ErrItemNotFound
	}
	return out, err
}

func (s *PgStore) ListItems(ctx context.Context, tenantID, batchID string, status imports.ItemStatus, afterRow, limit int) ([]imports.Item, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var out []imports.Item
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+itemCols+` FROM import_items WHERE batch_id = $1 AND tenant_id = $2 AND row_no > $3 AND ($4 = '' OR status = $4)
			ORDER BY row_no LIMIT $5`, batchID, tenantID, afterRow, string(status), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *it)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ItemCounts(ctx context.Context, tenantID, batchID string) (map[imports.ItemStatus]int, error) {
	out := map[imports.ItemStatus]int{}
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT status, count(*) FROM import_items WHERE batch_id = $1 AND tenant_id = $2 GROUP BY status`, batchID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st string
			var n int
			if err := rows.Scan(&st, &n); err != nil {
				return err
			}
			out[imports.ItemStatus(st)] = n
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ErrorCounts(ctx context.Context, tenantID, batchID string) (map[string]int, error) {
	out := map[string]int{}
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT e->>'code', count(*) FROM import_items, jsonb_array_elements(error_codes) e
			WHERE batch_id = $1 AND tenant_id = $2 GROUP BY 1`, batchID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var code string
			var n int
			if err := rows.Scan(&code, &n); err != nil {
				return err
			}
			out[code] = n
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) SumColumn(ctx context.Context, tenantID, batchID, column string, status imports.ItemStatus) (string, error) {
	var sum string
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(sum((item_values->>$3)::numeric), 0)::text FROM import_items
			WHERE batch_id = $1 AND tenant_id = $2 AND status = $4`, batchID, tenantID, column, string(status)).Scan(&sum)
	})
	if err != nil {
		return "", err
	}
	r, perr := imports.ParseDecimal(trimZeros(sum))
	if perr != nil {
		return sum, nil
	}
	return imports.CanonicalDecimal(r), nil
}

// trimZeros normalises numeric text ("100.500000" -> "100.5") so it parses as a plain decimal.
func trimZeros(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func (s *PgStore) ActivateItems(ctx context.Context, tenantID, batchID string) (int, int, error) {
	var pending, skipped int
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE import_items SET status = 'PENDING', next_attempt_at = now() WHERE batch_id = $1 AND tenant_id = $2 AND status = 'VALID'`, batchID, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE import_items SET status = 'SKIPPED' WHERE batch_id = $1 AND tenant_id = $2 AND status = 'INVALID'`, batchID, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'PENDING'), count(*) FILTER (WHERE status = 'SKIPPED')
			FROM import_items WHERE batch_id = $1 AND tenant_id = $2`, batchID, tenantID).Scan(&pending, &skipped)
	})
	return pending, skipped, err
}

func (s *PgStore) ClaimItems(ctx context.Context, tenantID, batchID string, now time.Time, limit int, lease time.Duration) ([]imports.Item, error) {
	var out []imports.Item
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH c AS (
				SELECT row_no FROM import_items
				WHERE batch_id = $1 AND tenant_id = $5 AND status = 'PENDING' AND next_attempt_at <= $2 AND (locked_until IS NULL OR locked_until < $2)
				ORDER BY row_no LIMIT $3 FOR UPDATE SKIP LOCKED)
			UPDATE import_items i SET attempts = i.attempts + 1, locked_until = $2::timestamptz + make_interval(secs => $4)
			FROM c WHERE i.batch_id = $1 AND i.row_no = c.row_no
			RETURNING i.batch_id, i.row_no, i.tenant_id, i.row_hash, i.item_values, i.status, i.error_codes, i.idempotency_key, i.attempts,
				i.next_attempt_at, i.locked_until, i.domain_status, COALESCE(i.result_ref, ''), COALESCE(i.last_error_class, ''),
				COALESCE(i.quarantine_reason, ''), COALESCE(i.quarantine_owner, ''), COALESCE(i.quarantined_by, ''), i.updated_at`,
			batchID, now, limit, lease.Seconds(), tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *it)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) FinishItem(ctx context.Context, tenantID, batchID string, rowNo int, res imports.ItemResult, events []imports.Event) error {
	return s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE import_items SET status = $4, domain_status = $5, result_ref = $6, last_error_class = $7, next_attempt_at = $8,
				locked_until = NULL, quarantine_reason = $9, quarantine_owner = $10
			WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3 AND status = 'PENDING'`,
			batchID, rowNo, tenantID, string(res.Status), res.DomainStatus, nilIfEmpty(res.ResultRef), nilIfEmpty(res.ErrorClass), res.NextAttemptAt,
			nilIfEmpty(res.QuarantineReason), nilIfEmpty(res.QuarantineOwner))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // a person moved the item meanwhile (quarantined it); their decision stands
		}
		return emitEvents(ctx, tx, tenantID, batchID, events)
	})
}

func (s *PgStore) QuarantineItem(ctx context.Context, tenantID, batchID string, rowNo int, reason, owner, by string, events []imports.Event) error {
	return s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE import_items SET status = 'QUARANTINED', quarantine_reason = $4, quarantine_owner = $5, quarantined_by = $6, locked_until = NULL
			WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3 AND status = 'PENDING'`, batchID, rowNo, tenantID, clip(reason, 80), clip(owner, 255), by)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM import_items WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3)`, batchID, rowNo, tenantID).Scan(&exists)
			if !exists {
				return imports.ErrItemNotFound
			}
			return fmt.Errorf("%w: only a pending item can be quarantined", imports.ErrInvalidState)
		}
		return emitEvents(ctx, tx, tenantID, batchID, events)
	})
}

func (s *PgStore) RetryItem(ctx context.Context, tenantID, batchID string, rowNo int, now time.Time, maxRetries int) error {
	return s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		var retries int
		var st string
		err := tx.QueryRow(ctx, `SELECT retries, status FROM import_items WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3 FOR UPDATE`, batchID, rowNo, tenantID).Scan(&retries, &st)
		if errors.Is(err, pgx.ErrNoRows) {
			return imports.ErrItemNotFound
		}
		if err != nil {
			return err
		}
		if st != "QUARANTINED" && st != "FAILED" {
			return fmt.Errorf("%w: only a quarantined or failed item can be retried (%s)", imports.ErrRetryNotAllowed, st)
		}
		if retries >= maxRetries {
			return fmt.Errorf("%w: it has been retried %d times already", imports.ErrRetryNotAllowed, retries)
		}
		_, err = tx.Exec(ctx, `UPDATE import_items SET status = 'PENDING', attempts = 0, retries = retries + 1, next_attempt_at = $4, locked_until = NULL,
			quarantine_reason = NULL, quarantine_owner = NULL, quarantined_by = NULL, last_error_class = NULL
			WHERE batch_id = $1 AND row_no = $2 AND tenant_id = $3`, batchID, rowNo, tenantID, now)
		return err
	})
}

func (s *PgStore) Pending(ctx context.Context, tenantID, batchID string) (int, error) {
	var n int
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM import_items WHERE batch_id = $1 AND tenant_id = $2 AND status = 'PENDING'`, batchID, tenantID).Scan(&n)
	})
	return n, err
}

func (s *PgStore) CountMissingRefs(ctx context.Context, tenantID, batchID string) (int, error) {
	var n int
	err := s.imp(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM import_items WHERE batch_id = $1 AND tenant_id = $2 AND status = 'IMPORTED' AND (result_ref IS NULL OR result_ref = '')`,
			batchID, tenantID).Scan(&n)
	})
	return n, err
}

// ── discovery (the worker names itself; it never runs unscoped) ─────────────

func (s *PgStore) workerQuery(ctx context.Context, q string, args ...any) ([]imports.Ref, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.import_worker', 'true', true)"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []imports.Ref
	for rows.Next() {
		var r imports.Ref
		if err := rows.Scan(&r.TenantID, &r.BatchID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgStore) DueValidations(ctx context.Context, limit int) ([]imports.Ref, error) {
	return s.workerQuery(ctx, `SELECT tenant_id, batch_id FROM import_batches
		WHERE validate_requested_at IS NOT NULL AND status IN ('UPLOADED', 'SCANNED', 'PARSED')
		ORDER BY validate_requested_at LIMIT $1`, limit)
}

func (s *PgStore) DueExecutions(ctx context.Context, now time.Time, limit int) ([]imports.Ref, error) {
	return s.workerQuery(ctx, `
		SELECT DISTINCT b.tenant_id, b.batch_id FROM import_batches b
		JOIN import_items i ON i.batch_id = b.batch_id
		WHERE b.status = 'IMPORTING' AND i.status = 'PENDING' AND i.next_attempt_at <= $1 AND (i.locked_until IS NULL OR i.locked_until < $1)
		UNION
		SELECT b.tenant_id, b.batch_id FROM import_batches b
		WHERE b.status = 'IMPORTING' AND NOT EXISTS (SELECT 1 FROM import_items p WHERE p.batch_id = b.batch_id AND p.status IN ('PENDING', 'VALID'))
		LIMIT $2`, now, limit)
}
