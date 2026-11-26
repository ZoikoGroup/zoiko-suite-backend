package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

// ErrCrosswalkConflict is returned when an incoming crosswalk entry names a
// (source_system, source_type, source_id) that already maps to a DIFFERENT
// canonical_id. ZS-MIG-001 MIG-INV-14 requires the mapping stay stable, so
// this is refused rather than silently overwritten -- a legacy key changing
// canonical identity is a fact that needs its own evidenced decision, not an
// upsert.
var ErrCrosswalkConflict = errors.New("identity crosswalk conflict: source identifier already maps to a different canonical_id")

// ErrBatchRefNotFound / ErrCertificateExists mirror the "not found"/"already
// exists" shape the rest of this store uses for Pg errors.
var (
	ErrBatchRefNotFound  = errors.New("migration batch ref not found")
	ErrCertificateExists = errors.New("a certificate already exists for this batch")
	ErrProgramNotFound   = errors.New("migration program not found")
	ErrExtractNotFound   = errors.New("source extract not found")
)

// ControlPlaneStore is the persistence boundary for the batch-ref,
// crosswalk and certificate objects (SPEC: the "program/tracking layer" on
// top of the field-level integrity engine). Implemented by *PgStore; no
// MemoryStore equivalent exists for these three because, like the rest of
// this service's writes, their evidence must survive a restart -- see
// MemoryStore's own doc comment on why it must never back the running
// service.
type ControlPlaneStore interface {
	CreateBatchRef(ctx context.Context, tenantID string, ref *domain.MigrationBatchRef, actorID, correlationID string) (*domain.MigrationBatchRef, error)
	GetBatchRefByJobID(ctx context.Context, tenantID, jobID string) (*domain.MigrationBatchRef, error)
	GetBatchRefByID(ctx context.Context, tenantID, id string) (*domain.MigrationBatchRef, error)
	UpdateBatchRef(ctx context.Context, tenantID, id string, status domain.BatchRefStatus, glBatchID, glJournalID, glJournalNumber, reconciliationStatus, failureReason, actorID, correlationID string) (*domain.MigrationBatchRef, error)
	// ResetBatchRefForRetry resets a FAILED batch ref to PENDING for a retried
	// promotion, updating batchCode/glBookID to the retry's (possibly
	// corrected) values -- a retry is allowed to name a different batch_code
	// (SPEC: the extract was corrected) and this is where that correction is
	// recorded, not left on the original failed attempt's values.
	ResetBatchRefForRetry(ctx context.Context, tenantID, id, batchCode, glBookID, actorID, correlationID string) (*domain.MigrationBatchRef, error)

	UpsertCrosswalkEntries(ctx context.Context, tenantID string, entries []domain.IdentityCrosswalk, correlationID string) ([]domain.IdentityCrosswalk, error)
	GetCrosswalkBySource(ctx context.Context, tenantID, sourceSystem, sourceType, sourceID string) (*domain.IdentityCrosswalk, error)
	ListCrosswalkBySourceIDs(ctx context.Context, tenantID, sourceSystem, sourceType string, sourceIDs []string) (map[string]domain.IdentityCrosswalk, error)

	CreateCertificate(ctx context.Context, tenantID string, cert *domain.MigrationCertificate, correlationID string) (*domain.MigrationCertificate, error)
	GetCertificateByBatchRefID(ctx context.Context, tenantID, batchRefID string) (*domain.MigrationCertificate, error)

	CreateProgram(ctx context.Context, tenantID string, p *domain.MigrationProgram, correlationID string) (*domain.MigrationProgram, error)
	GetProgramByBusinessID(ctx context.Context, tenantID, programID string) (*domain.MigrationProgram, error)
	GetProgramByID(ctx context.Context, tenantID, id string) (*domain.MigrationProgram, error)
	UpdateProgramStatus(ctx context.Context, tenantID, id string, status domain.ProgramStatus, actorID, correlationID string) (*domain.MigrationProgram, error)
	GetProgramBatchSummary(ctx context.Context, tenantID, programInternalID string) (*domain.ProgramBatchSummary, error)

	CreateExtract(ctx context.Context, tenantID string, e *domain.SourceExtract, actorID, correlationID string) (*domain.SourceExtract, error)
	GetExtractByID(ctx context.Context, tenantID, id string) (*domain.SourceExtract, error)
	GetLatestExtractByBusinessID(ctx context.Context, tenantID, extractID string) (*domain.SourceExtract, error)
	ListExtractVersions(ctx context.Context, tenantID, extractID string) ([]domain.SourceExtract, error)
}

var _ ControlPlaneStore = (*PgStore)(nil)

const (
	aggregateBatchRef    = "migration-batch-ref"
	aggregateCrosswalk   = "migration-identity-crosswalk"
	aggregateCertificate = "migration-certificate"
	aggregateProgram     = "migration-program"
	aggregateExtract     = "migration-source-extract"
)

func (s *PgStore) CreateBatchRef(ctx context.Context, tenantID string, ref *domain.MigrationBatchRef, actorID, correlationID string) (*domain.MigrationBatchRef, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	now := time.Now()
	ref.ID = uuid.New().String()
	ref.TenantID = tenantID
	ref.Status = domain.BatchRefPending
	ref.CreatedAt = now
	ref.UpdatedAt = now

	var programID any
	if ref.ProgramID != "" {
		programID = ref.ProgramID
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO migration_batch_refs (id, tenant_id, legal_entity_id, program_id, job_id, batch_code, gl_book_id, status, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT ON CONSTRAINT migration_batch_refs_job_unique DO UPDATE SET updated_at = migration_batch_refs.updated_at
		RETURNING id, status, gl_batch_id, gl_journal_id, gl_journal_number, reconciliation_status, created_at, updated_at, COALESCE(program_id::text, ''), (xmax = 0) AS inserted`,
		ref.ID, tenantID, ref.LegalEntityID, programID, ref.JobID, ref.BatchCode, ref.GLBookID, string(ref.Status), actorID, now, now,
	).Scan(&ref.ID, &ref.Status, nullStr(&ref.GLBatchID), nullStr(&ref.GLJournalID), nullStr(&ref.GLJournalNumber), nullStr(&ref.ReconciliationStatus), &ref.CreatedAt, &ref.UpdatedAt, &ref.ProgramID, new(bool))
	if err != nil {
		return nil, fmt.Errorf("insert migration_batch_ref: %w", err)
	}
	ref.CreatedBy = actorID

	if err := s.enqueueMigrationEvent(ctx, tx, "batch-ref", "created", "migration.batch_ref_created", aggregateBatchRef,
		tenantID, ref.LegalEntityID, ref.ID, actorID, correlationID, map[string]any{"batch_ref_id": ref.ID, "job_id": ref.JobID, "batch_code": ref.BatchCode}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ref, nil
}

// nullStr is a scan destination adapter: Pg NULL -> leaves *s untouched ("").
func nullStr(dst *string) *nullableStringScanner { return &nullableStringScanner{dst: dst} }

type nullableStringScanner struct{ dst *string }

func (n *nullableStringScanner) Scan(src any) error {
	if src == nil {
		*n.dst = ""
		return nil
	}
	switch v := src.(type) {
	case string:
		*n.dst = v
	case []byte:
		*n.dst = string(v)
	default:
		*n.dst = fmt.Sprintf("%v", v)
	}
	return nil
}

func (s *PgStore) GetBatchRefByJobID(ctx context.Context, tenantID, jobID string) (*domain.MigrationBatchRef, error) {
	return s.getBatchRef(ctx, tenantID, "job_id = $2", jobID)
}

func (s *PgStore) GetBatchRefByID(ctx context.Context, tenantID, id string) (*domain.MigrationBatchRef, error) {
	return s.getBatchRef(ctx, tenantID, "id = $2", id)
}

func (s *PgStore) getBatchRef(ctx context.Context, tenantID, pred, arg string) (*domain.MigrationBatchRef, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}
	var ref domain.MigrationBatchRef
	err = tx.QueryRow(ctx, `
		SELECT id, tenant_id, legal_entity_id, COALESCE(program_id::text, ''), job_id, batch_code, gl_book_id, gl_batch_id, gl_journal_id, gl_journal_number,
		       status, reconciliation_status, failure_reason, created_by, created_at, updated_at
		FROM migration_batch_refs WHERE tenant_id = $1 AND `+pred, tenantID, arg).Scan(
		&ref.ID, &ref.TenantID, &ref.LegalEntityID, &ref.ProgramID, &ref.JobID, &ref.BatchCode, &ref.GLBookID,
		nullStr(&ref.GLBatchID), nullStr(&ref.GLJournalID), nullStr(&ref.GLJournalNumber),
		&ref.Status, nullStr(&ref.ReconciliationStatus), nullStr(&ref.FailureReason), &ref.CreatedBy, &ref.CreatedAt, &ref.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBatchRefNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return &ref, nil
}

func (s *PgStore) UpdateBatchRef(ctx context.Context, tenantID, id string, status domain.BatchRefStatus, glBatchID, glJournalID, glJournalNumber, reconciliationStatus, failureReason, actorID, correlationID string) (*domain.MigrationBatchRef, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	var ref domain.MigrationBatchRef
	err = tx.QueryRow(ctx, `
		UPDATE migration_batch_refs SET
			status = $3,
			gl_batch_id = COALESCE(NULLIF($4,''), gl_batch_id),
			gl_journal_id = COALESCE(NULLIF($5,''), gl_journal_id),
			gl_journal_number = COALESCE(NULLIF($6,''), gl_journal_number),
			reconciliation_status = COALESCE(NULLIF($7,''), reconciliation_status),
			failure_reason = NULLIF($8,''),
			updated_at = $9
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, legal_entity_id, job_id, batch_code, gl_book_id, gl_batch_id, gl_journal_id, gl_journal_number,
		          status, reconciliation_status, failure_reason, created_by, created_at, updated_at, COALESCE(program_id::text, '')`,
		tenantID, id, string(status), glBatchID, glJournalID, glJournalNumber, reconciliationStatus, failureReason, time.Now(),
	).Scan(&ref.ID, &ref.TenantID, &ref.LegalEntityID, &ref.JobID, &ref.BatchCode, &ref.GLBookID,
		nullStr(&ref.GLBatchID), nullStr(&ref.GLJournalID), nullStr(&ref.GLJournalNumber),
		&ref.Status, nullStr(&ref.ReconciliationStatus), nullStr(&ref.FailureReason), &ref.CreatedBy, &ref.CreatedAt, &ref.UpdatedAt, &ref.ProgramID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBatchRefNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update migration_batch_ref: %w", err)
	}

	if err := s.enqueueMigrationEvent(ctx, tx, "batch-ref", "status-changed", "migration.batch_ref_status_changed", aggregateBatchRef,
		tenantID, ref.LegalEntityID, ref.ID, actorID, correlationID, map[string]any{
			"batch_ref_id": ref.ID, "status": string(ref.Status), "gl_batch_id": ref.GLBatchID, "gl_journal_id": ref.GLJournalID,
		}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &ref, nil
}

func (s *PgStore) ResetBatchRefForRetry(ctx context.Context, tenantID, id, batchCode, glBookID, actorID, correlationID string) (*domain.MigrationBatchRef, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	var ref domain.MigrationBatchRef
	err = tx.QueryRow(ctx, `
		UPDATE migration_batch_refs SET
			status = 'PENDING', batch_code = $3, gl_book_id = $4,
			gl_batch_id = NULL, gl_journal_id = NULL, gl_journal_number = NULL,
			reconciliation_status = NULL, failure_reason = NULL, updated_at = $5
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, legal_entity_id, job_id, batch_code, gl_book_id, gl_batch_id, gl_journal_id, gl_journal_number,
		          status, reconciliation_status, failure_reason, created_by, created_at, updated_at, COALESCE(program_id::text, '')`,
		tenantID, id, batchCode, glBookID, time.Now(),
	).Scan(&ref.ID, &ref.TenantID, &ref.LegalEntityID, &ref.JobID, &ref.BatchCode, &ref.GLBookID,
		nullStr(&ref.GLBatchID), nullStr(&ref.GLJournalID), nullStr(&ref.GLJournalNumber),
		&ref.Status, nullStr(&ref.ReconciliationStatus), nullStr(&ref.FailureReason), &ref.CreatedBy, &ref.CreatedAt, &ref.UpdatedAt, &ref.ProgramID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBatchRefNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reset migration_batch_ref for retry: %w", err)
	}

	if err := s.enqueueMigrationEvent(ctx, tx, "batch-ref", "retry-reset", "migration.batch_ref_retry_reset", aggregateBatchRef,
		tenantID, ref.LegalEntityID, ref.ID, actorID, correlationID, map[string]any{"batch_ref_id": ref.ID, "batch_code": ref.BatchCode}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &ref, nil
}

func (s *PgStore) UpsertCrosswalkEntries(ctx context.Context, tenantID string, entries []domain.IdentityCrosswalk, correlationID string) ([]domain.IdentityCrosswalk, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	now := time.Now()
	out := make([]domain.IdentityCrosswalk, 0, len(entries))
	for _, e := range entries {
		e.TenantID = tenantID
		var existingCanonical string
		err := tx.QueryRow(ctx, `SELECT canonical_id FROM migration_identity_crosswalk
			WHERE tenant_id = $1 AND source_system = $2 AND source_type = $3 AND source_id = $4`,
			tenantID, e.SourceSystem, e.SourceType, e.SourceID).Scan(&existingCanonical)
		if err == nil && existingCanonical != e.CanonicalID {
			return nil, fmt.Errorf("%w: %s/%s/%s already maps to %s, not %s",
				ErrCrosswalkConflict, e.SourceSystem, e.SourceType, e.SourceID, existingCanonical, e.CanonicalID)
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}

		e.ID = uuid.New().String()
		e.CreatedAt, e.UpdatedAt = now, now
		var firstJobID any
		if e.FirstJobID != "" {
			firstJobID = e.FirstJobID
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO migration_identity_crosswalk
			  (id, tenant_id, source_system, source_type, source_id, canonical_id, match_method, match_evidence, matched_by, first_job_id, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (tenant_id, source_system, source_type, source_id)
			DO UPDATE SET match_method = EXCLUDED.match_method, match_evidence = EXCLUDED.match_evidence,
			              matched_by = EXCLUDED.matched_by, updated_at = EXCLUDED.updated_at
			RETURNING id, created_at, updated_at`,
			e.ID, tenantID, e.SourceSystem, e.SourceType, e.SourceID, e.CanonicalID, e.MatchMethod, e.MatchEvidence, e.MatchedBy, firstJobID, now, now,
		).Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("upsert crosswalk entry: %w", err)
		}
		out = append(out, e)

		if err := s.enqueueMigrationEvent(ctx, tx, "identity-crosswalk", "resolved", "migration.identity_crosswalk_resolved", aggregateCrosswalk,
			tenantID, "", e.ID, e.MatchedBy, correlationID, map[string]any{
				"source_system": e.SourceSystem, "source_type": e.SourceType, "source_id": e.SourceID, "canonical_id": e.CanonicalID, "match_method": e.MatchMethod,
			}); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetCrosswalkBySource(ctx context.Context, tenantID, sourceSystem, sourceType, sourceID string) (*domain.IdentityCrosswalk, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}
	var e domain.IdentityCrosswalk
	err = tx.QueryRow(ctx, `
		SELECT id, tenant_id, source_system, source_type, source_id, canonical_id, match_method, match_evidence, matched_by,
		       COALESCE(first_job_id::text, ''), created_at, updated_at
		FROM migration_identity_crosswalk
		WHERE tenant_id = $1 AND source_system = $2 AND source_type = $3 AND source_id = $4`,
		tenantID, sourceSystem, sourceType, sourceID).Scan(
		&e.ID, &e.TenantID, &e.SourceSystem, &e.SourceType, &e.SourceID, &e.CanonicalID, &e.MatchMethod,
		nullStr(&e.MatchEvidence), &e.MatchedBy, &e.FirstJobID, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("identity crosswalk entry not found for %s/%s/%s", sourceSystem, sourceType, sourceID)
	}
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return &e, nil
}

func (s *PgStore) ListCrosswalkBySourceIDs(ctx context.Context, tenantID, sourceSystem, sourceType string, sourceIDs []string) (map[string]domain.IdentityCrosswalk, error) {
	out := make(map[string]domain.IdentityCrosswalk, len(sourceIDs))
	for _, id := range sourceIDs {
		e, err := s.GetCrosswalkBySource(ctx, tenantID, sourceSystem, sourceType, id)
		if err != nil {
			continue
		}
		out[id] = *e
	}
	return out, nil
}

func (s *PgStore) CreateCertificate(ctx context.Context, tenantID string, cert *domain.MigrationCertificate, correlationID string) (*domain.MigrationCertificate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	now := time.Now()
	cert.ID = uuid.New().String()
	cert.TenantID = tenantID
	cert.IssuedAt = now

	_, err = tx.Exec(ctx, `
		INSERT INTO migration_certificates
		  (id, tenant_id, legal_entity_id, scope, batch_ref_id, job_id, unresolved_exceptions,
		   gl_batch_id, gl_journal_id, gl_journal_number, gl_reconciliation_status, gl_evidence_hash, gl_evidence_verified,
		   signer_principal_id, issued_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		cert.ID, tenantID, cert.LegalEntityID, cert.Scope, cert.BatchRefID, cert.JobID, cert.UnresolvedExceptions,
		cert.GLBatchID, cert.GLJournalID, cert.GLJournalNumber, cert.GLReconciliationStatus, cert.GLEvidenceHash, cert.GLEvidenceVerified,
		cert.SignerPrincipalID, cert.IssuedAt,
	)
	// A duplicate batch_ref_id hits migration_certificates_batch_ref_unique:
	// one certificate per promoted batch (SPEC: issued once, not re-issuable).
	// The handler checks GetCertificateByBatchRefID first and this constraint
	// is the race-safe backstop, not the primary check.
	if err != nil {
		return nil, fmt.Errorf("insert migration_certificate: %w", err)
	}

	if err := s.enqueueMigrationEvent(ctx, tx, "certificate", "issued", "migration.certificate_issued", aggregateCertificate,
		tenantID, cert.LegalEntityID, cert.ID, cert.SignerPrincipalID, correlationID, map[string]any{
			"certificate_id": cert.ID, "batch_ref_id": cert.BatchRefID, "gl_journal_id": cert.GLJournalID, "gl_journal_number": cert.GLJournalNumber,
			"gl_evidence_hash": cert.GLEvidenceHash,
		}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return cert, nil
}

func (s *PgStore) GetCertificateByBatchRefID(ctx context.Context, tenantID, batchRefID string) (*domain.MigrationCertificate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}
	var c domain.MigrationCertificate
	err = tx.QueryRow(ctx, `
		SELECT id, tenant_id, legal_entity_id, scope, batch_ref_id, job_id, unresolved_exceptions,
		       gl_batch_id, gl_journal_id, gl_journal_number, gl_reconciliation_status, gl_evidence_hash, gl_evidence_verified,
		       signer_principal_id, issued_at
		FROM migration_certificates WHERE tenant_id = $1 AND batch_ref_id = $2`, tenantID, batchRefID).Scan(
		&c.ID, &c.TenantID, &c.LegalEntityID, &c.Scope, &c.BatchRefID, &c.JobID, &c.UnresolvedExceptions,
		&c.GLBatchID, &c.GLJournalID, &c.GLJournalNumber, &c.GLReconciliationStatus, &c.GLEvidenceHash, &c.GLEvidenceVerified,
		&c.SignerPrincipalID, &c.IssuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("no certificate issued yet for batch ref %s", batchRefID)
	}
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return &c, nil
}

// ─── Migration Program ─────────────────────────────────────────────────────

func (s *PgStore) CreateProgram(ctx context.Context, tenantID string, p *domain.MigrationProgram, correlationID string) (*domain.MigrationProgram, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	now := time.Now()
	p.ID = uuid.New().String()
	p.TenantID = tenantID
	p.Status = domain.ProgramOpen
	p.CreatedAt, p.UpdatedAt = now, now

	err = tx.QueryRow(ctx, `
		INSERT INTO migration_programs (id, tenant_id, program_id, source_estate, target_scope, owner_principal_id, cutover_strategy, status, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, created_at, updated_at`,
		p.ID, tenantID, p.ProgramID, p.SourceEstate, p.TargetScope, p.OwnerPrincipalID, p.CutoverStrategy, string(p.Status), p.CreatedBy, now, now,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert migration_program: %w", err)
	}

	if err := s.enqueueMigrationEvent(ctx, tx, "program", "created", "migration.program_created", aggregateProgram,
		tenantID, "", p.ID, p.CreatedBy, correlationID, map[string]any{"program_id": p.ProgramID, "source_estate": p.SourceEstate, "cutover_strategy": p.CutoverStrategy}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PgStore) GetProgramByBusinessID(ctx context.Context, tenantID, programID string) (*domain.MigrationProgram, error) {
	return s.getProgram(ctx, tenantID, "program_id = $2", programID)
}

func (s *PgStore) GetProgramByID(ctx context.Context, tenantID, id string) (*domain.MigrationProgram, error) {
	return s.getProgram(ctx, tenantID, "id = $2", id)
}

func (s *PgStore) getProgram(ctx context.Context, tenantID, pred, arg string) (*domain.MigrationProgram, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}
	var p domain.MigrationProgram
	err = tx.QueryRow(ctx, `
		SELECT id, tenant_id, program_id, source_estate, target_scope, owner_principal_id, cutover_strategy, status, created_by, created_at, updated_at
		FROM migration_programs WHERE tenant_id = $1 AND `+pred, tenantID, arg).Scan(
		&p.ID, &p.TenantID, &p.ProgramID, &p.SourceEstate, &p.TargetScope, &p.OwnerPrincipalID, &p.CutoverStrategy, &p.Status, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProgramNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return &p, nil
}

func (s *PgStore) UpdateProgramStatus(ctx context.Context, tenantID, id string, status domain.ProgramStatus, actorID, correlationID string) (*domain.MigrationProgram, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	var p domain.MigrationProgram
	err = tx.QueryRow(ctx, `
		UPDATE migration_programs SET status = $3, updated_at = $4
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, program_id, source_estate, target_scope, owner_principal_id, cutover_strategy, status, created_by, created_at, updated_at`,
		tenantID, id, string(status), time.Now(),
	).Scan(&p.ID, &p.TenantID, &p.ProgramID, &p.SourceEstate, &p.TargetScope, &p.OwnerPrincipalID, &p.CutoverStrategy, &p.Status, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProgramNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update migration_program status: %w", err)
	}

	if err := s.enqueueMigrationEvent(ctx, tx, "program", "status-changed", "migration.program_status_changed", aggregateProgram,
		tenantID, "", p.ID, actorID, correlationID, map[string]any{"program_id": p.ProgramID, "status": string(p.Status)}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PgStore) GetProgramBatchSummary(ctx context.Context, tenantID, programInternalID string) (*domain.ProgramBatchSummary, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	summary := &domain.ProgramBatchSummary{StatusCounts: make(map[string]int)}
	rows, err := tx.Query(ctx, `
		SELECT status, COUNT(*) FROM migration_batch_refs
		WHERE tenant_id = $1 AND program_id = $2 GROUP BY status`, tenantID, programInternalID)
	if err != nil {
		return nil, fmt.Errorf("query program batch summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		summary.StatusCounts[status] = count
		summary.TotalBatches += count
		if status == string(domain.BatchRefReconciled) {
			summary.ReconciledCount = count
		}
	}
	_ = tx.Commit(ctx)
	return summary, nil
}

// ─── Source Extract ─────────────────────────────────────────────────────────

// CreateExtract registers an immutable landing. If e.ExtractID already names
// an extract for this tenant, this call registers the NEXT version (a
// correction, MIG-INV-03); a blank e.ExtractID mints a fresh business id at
// version 1. pg_advisory_xact_lock on the (tenant, extract_id) pair, held for
// the transaction, serializes two concurrent registrations of the same
// extract_id so they can never compute the same "next version" twice --
// the table's own unique constraint is the backstop, not the primary guard.
func (s *PgStore) CreateExtract(ctx context.Context, tenantID string, e *domain.SourceExtract, actorID, correlationID string) (*domain.SourceExtract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	if e.ExtractID == "" {
		e.ExtractID = uuid.New().String()
	}

	lockKey := lockKeyFor(tenantID, e.ExtractID)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
		return nil, fmt.Errorf("acquire extract registration lock: %w", err)
	}

	var maxVersion int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM migration_source_extracts WHERE tenant_id = $1 AND extract_id = $2`,
		tenantID, e.ExtractID).Scan(&maxVersion); err != nil {
		return nil, fmt.Errorf("compute next extract version: %w", err)
	}

	filesJSON, err := json.Marshal(e.Files)
	if err != nil {
		return nil, err
	}
	if e.AmountControls == nil {
		e.AmountControls = map[string]domain.ExtractAmountControl{}
	}
	controlsJSON, err := json.Marshal(e.AmountControls)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	e.ID = uuid.New().String()
	e.TenantID = tenantID
	e.Version = maxVersion + 1
	e.CreatedBy = actorID
	e.CreatedAt = now

	var programID any
	if e.ProgramID != "" {
		programID = e.ProgramID
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO migration_source_extracts
		  (id, tenant_id, legal_entity_id, program_id, extract_id, version, source_watermark, generated_at,
		   manifest_hash, record_count, files, amount_controls, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id, created_at`,
		e.ID, tenantID, e.LegalEntityID, programID, e.ExtractID, e.Version, e.SourceWatermark, e.GeneratedAt,
		e.ManifestHash, e.RecordCount, filesJSON, controlsJSON, e.CreatedBy, now,
	).Scan(&e.ID, &e.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert migration_source_extract: %w", err)
	}

	if err := s.enqueueMigrationEvent(ctx, tx, "source-extract", "registered", "migration.source_extract_registered", aggregateExtract,
		tenantID, e.LegalEntityID, e.ID, actorID, correlationID, map[string]any{
			"extract_id": e.ExtractID, "version": e.Version, "manifest_hash": e.ManifestHash, "record_count": e.RecordCount,
		}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// lockKeyFor derives a stable int64 advisory-lock key from a (tenant,
// extract_id) pair. FNV-1a rather than hashtext() so the key is computed the
// same way whether reasoned about in Go or in SQL, and is stable across
// Postgres versions (hashtext's algorithm is not a stable contract).
func lockKeyFor(tenantID, extractID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(tenantID + "|" + extractID))
	return int64(h.Sum64())
}

func (s *PgStore) GetExtractByID(ctx context.Context, tenantID, id string) (*domain.SourceExtract, error) {
	return s.getExtract(ctx, tenantID, "id = $2", id)
}

func (s *PgStore) GetLatestExtractByBusinessID(ctx context.Context, tenantID, extractID string) (*domain.SourceExtract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}
	e, err := scanExtract(tx.QueryRow(ctx, extractSelectSQL+` WHERE tenant_id = $1 AND extract_id = $2 ORDER BY version DESC LIMIT 1`, tenantID, extractID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExtractNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return e, nil
}

func (s *PgStore) ListExtractVersions(ctx context.Context, tenantID, extractID string) ([]domain.SourceExtract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, extractSelectSQL+` WHERE tenant_id = $1 AND extract_id = $2 ORDER BY version ASC`, tenantID, extractID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SourceExtract
	for rows.Next() {
		e, err := scanExtractRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) getExtract(ctx context.Context, tenantID, pred, arg string) (*domain.SourceExtract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}
	e, err := scanExtract(tx.QueryRow(ctx, extractSelectSQL+` WHERE tenant_id = $1 AND `+pred, tenantID, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExtractNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return e, nil
}

const extractSelectSQL = `
	SELECT id, tenant_id, legal_entity_id, COALESCE(program_id::text, ''), extract_id, version, source_watermark,
	       generated_at, manifest_hash, record_count, files, amount_controls, created_by, created_at
	FROM migration_source_extracts`

// rowScanner is the subset of pgx.Row/pgx.Rows this file's two extract
// scanners need -- one Scan signature shared between QueryRow's single-row
// result and Query's multi-row iterator.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanExtract(row rowScanner) (*domain.SourceExtract, error) {
	return scanExtractRows(row)
}

func scanExtractRows(row rowScanner) (*domain.SourceExtract, error) {
	var e domain.SourceExtract
	var filesJSON, controlsJSON []byte
	if err := row.Scan(&e.ID, &e.TenantID, &e.LegalEntityID, &e.ProgramID, &e.ExtractID, &e.Version, &e.SourceWatermark,
		&e.GeneratedAt, &e.ManifestHash, &e.RecordCount, &filesJSON, &controlsJSON, &e.CreatedBy, &e.CreatedAt); err != nil {
		return nil, err
	}
	if len(filesJSON) > 0 {
		if err := json.Unmarshal(filesJSON, &e.Files); err != nil {
			return nil, fmt.Errorf("decode extract files: %w", err)
		}
	}
	if len(controlsJSON) > 0 {
		if err := json.Unmarshal(controlsJSON, &e.AmountControls); err != nil {
			return nil, fmt.Errorf("decode extract amount_controls: %w", err)
		}
	}
	return &e, nil
}
