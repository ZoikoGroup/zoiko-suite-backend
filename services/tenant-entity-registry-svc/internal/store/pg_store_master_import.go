package store

// ORG-9 master imports on Postgres (migration 000027), and the API-5 operation store the imports run as. See
// registry.MasterImportStore for the contract.
//
// An upload records the import, every row as received and its validation operation in one transaction, and an idempotency key
// that has been used returns the earlier operation and records nothing. Every command takes the import's row lock. Reconciliation
// counts the rows themselves, never the running counters, so it cannot agree with a counter that drifted.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var _ registry.MasterImportStore = (*PgStore)(nil)

// ── API-5 operations ────────────────────────────────────────────────────────

const opColumns = `id, tenant_id, principal_id, route, type, state, progress_completed, progress_total, resource_ref, problem, cancelable,
	idempotency_key, request_hash, created_at, updated_at, started_at, finished_at`

func scanOp(row pgx.Row) (apicontract.Operation, error) {
	var op apicontract.Operation
	var state string
	var problem []byte
	var completed, total int64
	if err := row.Scan(&op.ID, &op.TenantID, &op.PrincipalID, &op.Route, &op.Type, &state, &completed, &total, &op.ResourceRef, &problem, &op.Cancelable,
		&op.IdempotencyKey, &op.RequestHash, &op.CreatedAt, &op.UpdatedAt, &op.StartedAt, &op.FinishedAt); err != nil {
		return apicontract.Operation{}, err
	}
	op.State = apicontract.OperationState(state)
	if completed != 0 || total != 0 {
		op.Progress = &apicontract.OperationProgress{CompletedUnits: completed, TotalUnits: total}
	}
	if len(problem) > 0 {
		var p apicontract.Problem
		if err := json.Unmarshal(problem, &p); err != nil {
			return apicontract.Operation{}, err
		}
		op.Problem = &p
	}
	return op, nil
}

// txCreateOp records an operation, or returns the one the idempotency key already names (created false).
func txCreateOp(ctx context.Context, tx pgx.Tx, op apicontract.Operation) (apicontract.Operation, bool, error) {
	if op.State == "" {
		op.State = apicontract.OperationQueued
	}
	if op.CreatedAt.IsZero() {
		op.CreatedAt = time.Now().UTC()
	}
	op.UpdatedAt = op.CreatedAt
	if err := op.Validate(); err != nil {
		return apicontract.Operation{}, false, err
	}
	completed, total := op.ProgressValues()
	created, err := scanOp(tx.QueryRow(ctx, `INSERT INTO async_operations (`+opColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL,$10,$11,$12,$13,$14,NULL,NULL)
		ON CONFLICT (tenant_id, principal_id, route, idempotency_key) WHERE idempotency_key <> '' DO NOTHING RETURNING `+opColumns,
		op.ID, op.TenantID, op.PrincipalID, op.Route, op.Type, string(op.State), completed, total, op.ResourceRef, op.Cancelable, op.IdempotencyKey,
		op.RequestHash, op.CreatedAt, op.UpdatedAt))
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return apicontract.Operation{}, false, err
	}
	existing, err := scanOp(tx.QueryRow(ctx, `SELECT `+opColumns+` FROM async_operations WHERE tenant_id = $1 AND principal_id = $2 AND route = $3 AND idempotency_key = $4`,
		op.TenantID, op.PrincipalID, op.Route, op.IdempotencyKey))
	if err != nil {
		return apicontract.Operation{}, false, err
	}
	if existing.RequestHash != op.RequestHash {
		return apicontract.Operation{}, false, apicontract.ErrOperationConflict
	}
	return existing, false, nil
}

// pgOperations is the apicontract.OperationStore of the service: tenant-scoped under row-level security.
type pgOperations struct{ s *PgStore }

var _ apicontract.OperationStore = pgOperations{}

// Operations returns the API-5 operation store.
func (s *PgStore) Operations() apicontract.OperationStore { return pgOperations{s} }

func (o pgOperations) Create(ctx context.Context, op apicontract.Operation) (apicontract.Operation, bool, error) {
	var out apicontract.Operation
	var created bool
	err := o.s.withRLS(ctx, op.TenantID, func(tx pgx.Tx) error {
		var err error
		out, created, err = txCreateOp(ctx, tx, op)
		return err
	})
	return out, created, err
}

func (o pgOperations) Get(ctx context.Context, tenantID, operationID string) (apicontract.Operation, error) {
	var out apicontract.Operation
	err := o.s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanOp(tx.QueryRow(ctx, `SELECT `+opColumns+` FROM async_operations WHERE tenant_id = $1 AND id = $2`, tenantID, operationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return apicontract.ErrOperationNotFound
		}
		return err
	})
	return out, err
}

func (o pgOperations) FindByKey(ctx context.Context, tenantID, principalID, route, key string) (apicontract.Operation, bool, error) {
	if key == "" {
		return apicontract.Operation{}, false, nil
	}
	var out apicontract.Operation
	found := false
	err := o.s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		op, err := scanOp(tx.QueryRow(ctx, `SELECT `+opColumns+` FROM async_operations WHERE tenant_id = $1 AND principal_id = $2 AND route = $3 AND idempotency_key = $4`,
			tenantID, principalID, route, key))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out, found = op, err == nil
		return err
	})
	return out, found, err
}

func (o pgOperations) Advance(ctx context.Context, tenantID, operationID string, to apicontract.OperationState, progress *apicontract.OperationProgress,
	problem *apicontract.Problem, resourceRef string) (apicontract.Operation, error) {
	var out apicontract.Operation
	err := o.s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanOp(tx.QueryRow(ctx, `SELECT `+opColumns+` FROM async_operations WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, operationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return apicontract.ErrOperationNotFound
		}
		if err != nil {
			return err
		}
		if !apicontract.CanTransition(cur.State, to) {
			return fmt.Errorf("%w: cannot move operation from %s to %s", apicontract.ErrOperationConflict, cur.State, to)
		}
		var problemJSON []byte
		if problem != nil {
			if problemJSON, err = json.Marshal(problem); err != nil {
				return err
			}
		}
		completed, total := cur.ProgressValues()
		if progress != nil {
			completed, total = progress.CompletedUnits, progress.TotalUnits
		}
		out, err = scanOp(tx.QueryRow(ctx, `UPDATE async_operations SET state = $3, progress_completed = $4, progress_total = $5, problem = COALESCE($6::jsonb, problem),
			resource_ref = CASE WHEN $7 <> '' THEN $7 ELSE resource_ref END, updated_at = now(),
			started_at = CASE WHEN $3 = 'running' THEN COALESCE(started_at, now()) ELSE started_at END,
			finished_at = CASE WHEN $3 IN ('succeeded','failed','cancelled') THEN now() ELSE finished_at END,
			cancelable = CASE WHEN $3 IN ('succeeded','failed','cancelled') THEN false ELSE cancelable END
			WHERE tenant_id = $1 AND id = $2 RETURNING `+opColumns, tenantID, operationID, string(to), completed, total, problemJSON, resourceRef))
		return err
	})
	return out, err
}

func (o pgOperations) RequestCancel(ctx context.Context, tenantID, operationID string) (apicontract.Operation, error) {
	var out apicontract.Operation
	err := o.s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanOp(tx.QueryRow(ctx, `SELECT `+opColumns+` FROM async_operations WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, operationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return apicontract.ErrOperationNotFound
		}
		if err != nil {
			return err
		}
		if cur.State.Terminal() || !cur.Cancelable {
			return apicontract.ErrOperationNotCancelable
		}
		if cur.State == apicontract.OperationCancelRequested {
			out = cur
			return nil
		}
		out, err = scanOp(tx.QueryRow(ctx, `UPDATE async_operations SET state = 'cancel_requested', updated_at = now() WHERE tenant_id = $1 AND id = $2 RETURNING `+opColumns,
			tenantID, operationID))
		return err
	})
	return out, err
}

// ── imports ─────────────────────────────────────────────────────────────────

const importColumns = `import_id, tenant_id, kind, status, source_name, source_version, content_sha256, source_class, evidence_ref, valid_from, row_count,
	valid_rows, invalid_rows, imported_rows, conflict_rows, failed_rows, COALESCE(scope_entity_id::text,''), uploaded_by, uploaded_at, validated_at,
	COALESCE(approved_by,''), approved_at, COALESCE(approval_evidence,''), started_at, finished_at, COALESCE(validate_op_id,''), COALESCE(import_op_id,''),
	reconciliation, record_version`

func scanImport(row pgx.Row) (*domain.MasterImport, error) {
	var m domain.MasterImport
	var from time.Time
	var rec []byte
	if err := row.Scan(&m.ImportID, &m.TenantID, &m.Kind, &m.Status, &m.SourceName, &m.SourceVersion, &m.ContentSHA256, &m.SourceClass, &m.EvidenceRef, &from, &m.RowCount,
		&m.ValidRows, &m.InvalidRows, &m.ImportedRows, &m.ConflictRows, &m.FailedRows, &m.ScopeEntityID, &m.UploadedBy, &m.UploadedAt, &m.ValidatedAt,
		&m.ApprovedBy, &m.ApprovedAt, &m.ApprovalEvidence, &m.StartedAt, &m.FinishedAt, &m.ValidateOpID, &m.ImportOpID, &rec, &m.RecordVersion); err != nil {
		return nil, err
	}
	m.ValidFrom = fmtDate(from)
	if len(rec) > 0 {
		var r domain.Reconciliation
		if err := json.Unmarshal(rec, &r); err != nil {
			return nil, err
		}
		m.Reconciliation = &r
	}
	return &m, nil
}

func lockImport(ctx context.Context, tx pgx.Tx, tid, id string) (*domain.MasterImport, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrNotFound
	}
	m, err := scanImport(tx.QueryRow(ctx, `SELECT `+importColumns+` FROM master_imports WHERE tenant_id = $1 AND import_id = $2 FOR UPDATE`, tid, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return m, err
}

func reloadImport(ctx context.Context, tx pgx.Tx, tid, id string) (*domain.MasterImport, error) {
	return scanImport(tx.QueryRow(ctx, `SELECT `+importColumns+` FROM master_imports WHERE tenant_id = $1 AND import_id = $2`, tid, id))
}

// CreateMasterImport records an upload.
func (s *PgStore) CreateMasterImport(ctx context.Context, in registry.NewMasterImport) (*registry.MasterImportOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.MasterImportOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		op, created, err := txCreateOp(ctx, tx, in.Operation)
		if err != nil {
			return err
		}
		if !created {
			imp, err := scanImport(tx.QueryRow(ctx, `SELECT `+importColumns+` FROM master_imports WHERE tenant_id = $1 AND validate_op_id = $2`, tid, op.ID))
			if err != nil {
				return err
			}
			out = &registry.MasterImportOutcome{Import: imp, Operation: op}
			return nil
		}
		id := uuid.NewString()
		valid, _ := domain.ParseDate(in.Request.ValidFrom)
		var scope any
		if in.ScopeEntityID != "" {
			scope = in.ScopeEntityID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO master_imports (import_id, tenant_id, kind, status, source_name, source_version, content_sha256, manifest_sha256, source_class,
			evidence_ref, valid_from, row_count, scope_entity_id, uploaded_by, validate_op_id, correlation_id)
			VALUES ($1,$2,$3,'UPLOADED',$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12,$13,$14,NULLIF($15,''))`,
			id, tid, in.Request.Kind, strings.TrimSpace(in.Request.SourceName), strings.TrimSpace(in.Request.SourceVersion), in.Request.ContentSHA256(),
			in.Request.ManifestSHA256, string(in.Request.SourceClassOrDefault()), strings.TrimSpace(in.Request.EvidenceRef), valid, len(in.Rows), scope, in.Actor,
			op.ID, in.Request.CorrelationID); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		for i, raw := range in.Rows {
			batch.Queue(`INSERT INTO master_import_rows (import_id, tenant_id, row_no, raw) VALUES ($1,$2,$3,$4::jsonb)`, id, tid, i+1, string(raw))
		}
		br := tx.SendBatch(ctx, batch)
		for range in.Rows {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return fmt.Errorf("%w: a row is not valid JSON", registry.ErrInvalidInput)
			}
		}
		if err := br.Close(); err != nil {
			return err
		}
		imp, err := reloadImport(ctx, tx, tid, id)
		if err != nil {
			return err
		}
		out = &registry.MasterImportOutcome{Import: imp, Operation: op, Created: true}
		rec, err := events.MasterImportRecord(events.EventMasterImportUploaded, *imp, in.Actor, in.Request.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ApproveMasterImport approves a previewed import.
func (s *PgStore) ApproveMasterImport(ctx context.Context, importID string, req domain.ApproveMasterImportRequest, actor string, op apicontract.Operation, allowUnversioned bool) (*registry.MasterImportOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.MasterImportOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		imp, err := lockImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		stored, created, err := txCreateOp(ctx, tx, op)
		if err != nil {
			return err
		}
		if !created {
			// The same approval again: the same operation, nothing recorded.
			out = &registry.MasterImportOutcome{Import: imp, Operation: stored}
			return nil
		}
		if err := checkRecordVersion(req.ExpectedVersion, imp.RecordVersion, allowUnversioned, "master import"); err != nil {
			return err
		}
		if imp.Status == domain.ImportBlocked || imp.InvalidRows > 0 {
			return fmt.Errorf("%w: the file has %d invalid row(s) and cannot be approved; correct them and upload it again", registry.ErrStateConflict, imp.InvalidRows)
		}
		if imp.Status != domain.ImportPreviewReady {
			return fmt.Errorf("%w: the import is %s, not ready for approval", registry.ErrStateConflict, imp.Status)
		}
		// Whoever uploaded a file does not approve it.
		if imp.UploadedBy == actor {
			return fmt.Errorf("%w: an import is approved by someone other than the principal who uploaded it", registry.ErrSelfApproval)
		}
		if _, err := tx.Exec(ctx, `UPDATE master_imports SET status = 'APPROVED', approved_by = $3, approved_at = now(), approval_evidence = $4, import_op_id = $5,
			record_version = record_version + 1 WHERE tenant_id = $1 AND import_id = $2`, tid, importID, actor, strings.TrimSpace(req.EvidenceRef), stored.ID); err != nil {
			return err
		}
		done, err := reloadImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		out = &registry.MasterImportOutcome{Import: done, Operation: stored, Created: true}
		rec, err := events.MasterImportRecord(events.EventMasterImportApproved, *done, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// GetMasterImport returns an import, or nil.
func (s *PgStore) GetMasterImport(ctx context.Context, importID string) (*domain.MasterImport, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(importID); err != nil {
		return nil, nil
	}
	var out *domain.MasterImport
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		m, err := reloadImport(ctx, tx, tid, importID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = m
		return err
	})
	return out, err
}

// ListImportRows returns the verdicts and outcomes of an import's rows, by row number.
func (s *PgStore) ListImportRows(ctx context.Context, importID string, q domain.ImportRowQuery) ([]domain.ImportRow, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.ImportRow{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if _, err := uuid.Parse(importID); err != nil {
			return registry.ErrNotFound
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM master_imports WHERE tenant_id = $1 AND import_id = $2`, tid, importID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return registry.ErrNotFound
		}
		rows, err := tx.Query(ctx, `SELECT row_no, COALESCE(row_ref,''), status, errors, COALESCE(outcome,''), COALESCE(party_id::text,''), COALESCE(case_id::text,''), updated_at
			FROM master_import_rows WHERE import_id = $1 AND row_no > $2 AND ($3 = '' OR status = $3) ORDER BY row_no LIMIT $4`, importID, q.AfterRow, q.Status, q.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ImportRow
			var errs []byte
			if err := rows.Scan(&r.RowNo, &r.RowRef, &r.Status, &errs, &r.Outcome, &r.PartyID, &r.CaseID, &r.UpdatedAt); err != nil {
				return err
			}
			if err := json.Unmarshal(errs, &r.Errors); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func eventOf(ctx context.Context, s *PgStore, tx pgx.Tx, eventType string, imp *domain.MasterImport, actor string) error {
	rec, err := events.MasterImportRecord(eventType, *imp, actor, "")
	if err != nil {
		return err
	}
	return s.enqueue(ctx, tx, rec)
}

// BeginValidation moves an UPLOADED import to VALIDATING and returns its rows as received.
func (s *PgStore) BeginValidation(ctx context.Context, importID string) (*domain.MasterImport, []domain.StagedRow, error) {
	tid := domain.TenantFromContext(ctx)
	var imp *domain.MasterImport
	var staged []domain.StagedRow
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := lockImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		if cur.Status != domain.ImportUploaded {
			return fmt.Errorf("%w: the import is %s, not uploaded", registry.ErrStateConflict, cur.Status)
		}
		if _, err := tx.Exec(ctx, `UPDATE master_imports SET status = 'VALIDATING', record_version = record_version + 1 WHERE tenant_id = $1 AND import_id = $2`, tid, importID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT row_no, raw::text FROM master_import_rows WHERE import_id = $1 ORDER BY row_no`, importID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.StagedRow
			var raw string
			if err := rows.Scan(&r.RowNo, &raw); err != nil {
				return err
			}
			r.Raw = []byte(raw)
			staged = append(staged, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		imp, err = reloadImport(ctx, tx, tid, importID)
		return err
	})
	return imp, staged, err
}

// SaveValidation records every row's verdict and moves the import to PREVIEW_READY or BLOCKED.
func (s *PgStore) SaveValidation(ctx context.Context, importID string, verdicts []domain.RowValidation) (*domain.MasterImport, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.MasterImport
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := lockImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		if cur.Status != domain.ImportValidating {
			return fmt.Errorf("%w: the import is %s, not being validated", registry.ErrStateConflict, cur.Status)
		}
		if len(verdicts) != cur.RowCount {
			return fmt.Errorf("%w: %d verdicts for %d rows", registry.ErrInvalidInput, len(verdicts), cur.RowCount)
		}
		batch := &pgx.Batch{}
		for _, v := range verdicts {
			errs := v.Issues
			if errs == nil {
				errs = []domain.RowIssue{}
			}
			eb, _ := json.Marshal(errs)
			batch.Queue(`UPDATE master_import_rows SET status = $3, row_ref = $4, errors = $5::jsonb, updated_at = now() WHERE import_id = $1 AND row_no = $2`,
				importID, v.RowNo, v.Status, v.RowRef, string(eb))
		}
		br := tx.SendBatch(ctx, batch)
		for range verdicts {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return err
			}
		}
		if err := br.Close(); err != nil {
			return err
		}
		valid, invalid := domain.CountVerdicts(verdicts)
		status := domain.ImportPreviewReady
		if invalid > 0 {
			status = domain.ImportBlocked
		}
		if _, err := tx.Exec(ctx, `UPDATE master_imports SET status = $3, valid_rows = $4, invalid_rows = $5, validated_at = now(), record_version = record_version + 1
			WHERE tenant_id = $1 AND import_id = $2`, tid, importID, status, valid, invalid); err != nil {
			return err
		}
		out, err = reloadImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		return eventOf(ctx, s, tx, events.EventMasterImportValidated, out, out.UploadedBy)
	})
	return out, err
}

// StartImport is the point of no return.
func (s *PgStore) StartImport(ctx context.Context, importID, operationID string) (bool, error) {
	tid := domain.TenantFromContext(ctx)
	started := false
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := lockImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		op, err := scanOp(tx.QueryRow(ctx, `SELECT `+opColumns+` FROM async_operations WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tid, operationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if op.State == apicontract.OperationCancelRequested {
			if cur.Status == domain.ImportApproved {
				if _, err := tx.Exec(ctx, `UPDATE master_imports SET status = 'CANCELLED', finished_at = now(), record_version = record_version + 1 WHERE tenant_id = $1 AND import_id = $2`, tid, importID); err != nil {
					return err
				}
				done, err := reloadImport(ctx, tx, tid, importID)
				if err != nil {
					return err
				}
				return eventOf(ctx, s, tx, events.EventMasterImportCancelled, done, done.ApprovedBy)
			}
			return nil
		}
		if op.State.Terminal() {
			return fmt.Errorf("%w: the operation is already %s", registry.ErrStateConflict, op.State)
		}
		switch cur.Status {
		case domain.ImportApproved:
			if _, err := tx.Exec(ctx, `UPDATE master_imports SET status = 'IMPORTING', started_at = now(), record_version = record_version + 1 WHERE tenant_id = $1 AND import_id = $2`, tid, importID); err != nil {
				return err
			}
		case domain.ImportImporting:
			// Running again after an interruption: rows with an outcome stay as they are.
		default:
			return fmt.Errorf("%w: the import is %s, not approved", registry.ErrStateConflict, cur.Status)
		}
		// From here the import cannot be cancelled: parties are about to be created.
		if _, err := tx.Exec(ctx, `UPDATE async_operations SET state = 'running', cancelable = false, updated_at = now(), started_at = COALESCE(started_at, now())
			WHERE tenant_id = $1 AND id = $2`, tid, operationID); err != nil {
			return err
		}
		started = true
		return nil
	})
	return started, err
}

// NextImportRows returns up to limit VALID rows with no outcome yet.
func (s *PgStore) NextImportRows(ctx context.Context, importID string, limit int) ([]domain.StagedRow, error) {
	tid := domain.TenantFromContext(ctx)
	var out []domain.StagedRow
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT row_no, COALESCE(row_ref,''), raw::text FROM master_import_rows WHERE import_id = $1 AND status = 'VALID' ORDER BY row_no LIMIT $2`, importID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.StagedRow
			var raw string
			if err := rows.Scan(&r.RowNo, &r.RowRef, &raw); err != nil {
				return err
			}
			r.Raw = []byte(raw)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// RecordRowOutcome records what importing a row did.
func (s *PgStore) RecordRowOutcome(ctx context.Context, importID string, rowNo int, o domain.RowOutcome) error {
	tid := domain.TenantFromContext(ctx)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if o.Status != domain.RowImported && o.Status != domain.RowConflict && o.Status != domain.RowFailed {
			return fmt.Errorf("%w: %s is not a row outcome", registry.ErrInvalidInput, o.Status)
		}
		errs := o.Issues
		if errs == nil {
			errs = []domain.RowIssue{}
		}
		eb, _ := json.Marshal(errs)
		tag, err := tx.Exec(ctx, `UPDATE master_import_rows SET status = $3, outcome = NULLIF($4,''), party_id = NULLIF($5,'')::uuid, case_id = NULLIF($6,'')::uuid,
			errors = $7::jsonb, updated_at = now() WHERE import_id = $1 AND row_no = $2 AND status = 'VALID'`,
			importID, rowNo, o.Status, o.Outcome, o.PartyID, o.CaseID, string(eb))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: row %d has no pending outcome", registry.ErrStateConflict, rowNo)
		}
		col := map[string]string{domain.RowImported: "imported_rows", domain.RowConflict: "conflict_rows", domain.RowFailed: "failed_rows"}[o.Status]
		_, err = tx.Exec(ctx, `UPDATE master_imports SET `+col+` = `+col+` + 1 WHERE tenant_id = $1 AND import_id = $2`, tid, importID)
		return err
	})
}

// CompleteImport reconciles the import from its rows and ends it.
func (s *PgStore) CompleteImport(ctx context.Context, importID string) (*domain.MasterImport, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.MasterImport
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := lockImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		if cur.Status != domain.ImportImporting {
			return fmt.Errorf("%w: the import is %s, not importing", registry.ErrStateConflict, cur.Status)
		}
		var staged, invalid, imported, conflicts, failed, unresolved int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'INVALID'), count(*) FILTER (WHERE status = 'IMPORTED'),
			count(*) FILTER (WHERE status = 'CONFLICT'), count(*) FILTER (WHERE status = 'FAILED'), count(*) FILTER (WHERE status IN ('PENDING','VALID'))
			FROM master_import_rows WHERE import_id = $1`, importID).Scan(&staged, &invalid, &imported, &conflicts, &failed, &unresolved); err != nil {
			return err
		}
		rec := domain.Reconcile(cur.RowCount, staged, invalid, imported, conflicts, failed, unresolved)
		status := domain.ImportCompleted
		if !rec.Matched {
			status = domain.ImportFailed
		}
		rb, _ := json.Marshal(rec)
		if _, err := tx.Exec(ctx, `UPDATE master_imports SET status = $3, imported_rows = $4, conflict_rows = $5, failed_rows = $6, reconciliation = $7::jsonb, finished_at = now(),
			record_version = record_version + 1 WHERE tenant_id = $1 AND import_id = $2`, tid, importID, status, imported, conflicts, failed, string(rb)); err != nil {
			return err
		}
		out, err = reloadImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		return eventOf(ctx, s, tx, events.EventMasterImportCompleted, out, out.ApprovedBy)
	})
	return out, err
}

// CancelImport ends an import that has not started importing CANCELLED.
func (s *PgStore) CancelImport(ctx context.Context, importID string) error {
	tid := domain.TenantFromContext(ctx)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := lockImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		switch cur.Status {
		case domain.ImportUploaded, domain.ImportValidating, domain.ImportPreviewReady, domain.ImportApproved:
		default:
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE master_imports SET status = 'CANCELLED', finished_at = now(), record_version = record_version + 1 WHERE tenant_id = $1 AND import_id = $2`, tid, importID); err != nil {
			return err
		}
		done, err := reloadImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		return eventOf(ctx, s, tx, events.EventMasterImportCancelled, done, done.UploadedBy)
	})
}

// FailImport ends an import FAILED when its work could not be carried out.
func (s *PgStore) FailImport(ctx context.Context, importID, reason string) error {
	tid := domain.TenantFromContext(ctx)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := lockImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		switch cur.Status {
		case domain.ImportUploaded, domain.ImportValidating, domain.ImportApproved, domain.ImportImporting:
		default:
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE master_imports SET status = 'FAILED', finished_at = now(), record_version = record_version + 1 WHERE tenant_id = $1 AND import_id = $2`, tid, importID); err != nil {
			return err
		}
		done, err := reloadImport(ctx, tx, tid, importID)
		if err != nil {
			return err
		}
		_ = reason
		return eventOf(ctx, s, tx, events.EventMasterImportCompleted, done, done.UploadedBy)
	})
}
