package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/migration-integrity-svc/internal/domain"
)

// ─── Interface ────────────────────────────────────────────────────────────────

type Store interface {
	CreateJob(ctx context.Context, tenantID string, job *domain.MigrationJob, checks []domain.IntegrityCheck, entries []domain.AuditEntry) error
	GetJobByID(ctx context.Context, tenantID, id string) (*domain.MigrationJob, error)
	ListJobs(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.MigrationJob, error)
	ArchiveJob(ctx context.Context, tenantID, id string) error
	RemediateEntry(ctx context.Context, tenantID, jobID, entryID, notes string) (*domain.AuditEntry, error)
}

// ─── PgStore ──────────────────────────────────────────────────────────────────

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) CreateJob(ctx context.Context, tenantID string, job *domain.MigrationJob, checks []domain.IntegrityCheck, entries []domain.AuditEntry) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return err
	}

	now := time.Now()
	job.CreatedAt = now
	job.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		INSERT INTO migration_jobs
		  (id, tenant_id, legal_entity_id, migration_name, source_system, target_service,
		   total_records_count, valid_records_count, invalid_records_count, integrity_score,
		   status, started_at, completed_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		job.ID, tenantID, job.LegalEntityID, job.MigrationName, job.SourceSystem, job.TargetService,
		job.TotalRecordsCount, job.ValidRecordsCount, job.InvalidRecordsCount, job.IntegrityScore,
		string(job.Status), job.StartedAt, job.CompletedAt, job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert migration_job: %w", err)
	}

	for i := range checks {
		checks[i].ID = uuid.New().String()
		checks[i].TenantID = tenantID
		checks[i].JobID = job.ID
		_, err = tx.Exec(ctx, `
			INSERT INTO migration_integrity_checks
			  (id, tenant_id, job_id, check_name, check_type, records_checked, records_passed, records_failed, severity, detail, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			checks[i].ID, tenantID, job.ID, checks[i].CheckName, string(checks[i].CheckType),
			checks[i].RecordsChecked, checks[i].RecordsPassed, checks[i].RecordsFailed,
			string(checks[i].Severity), checks[i].Detail, now,
		)
		if err != nil {
			return fmt.Errorf("insert integrity_check: %w", err)
		}
	}

	for i := range entries {
		entries[i].ID = uuid.New().String()
		entries[i].TenantID = tenantID
		entries[i].JobID = job.ID
		_, err = tx.Exec(ctx, `
			INSERT INTO migration_audit_entries
			  (id, tenant_id, job_id, record_ref, field_name, source_value, target_value, violation_type, is_remediated, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			entries[i].ID, tenantID, job.ID, entries[i].RecordRef, entries[i].FieldName,
			entries[i].SourceValue, entries[i].TargetValue, string(entries[i].ViolationType),
			entries[i].IsRemediated, now,
		)
		if err != nil {
			return fmt.Errorf("insert audit_entry: %w", err)
		}
	}

	job.IntegrityChecks = checks
	job.AuditEntries = entries
	return tx.Commit(ctx)
}

func (s *PgStore) GetJobByID(ctx context.Context, tenantID, id string) (*domain.MigrationJob, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *PgStore) ListJobs(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.MigrationJob, error) {
	return nil, nil
}

func (s *PgStore) ArchiveJob(ctx context.Context, tenantID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return err
	}
	cmd, err := tx.Exec(ctx, "UPDATE migration_jobs SET status='ARCHIVED', updated_at=$1 WHERE id=$2", time.Now(), id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("migration job not found")
	}
	return tx.Commit(ctx)
}

func (s *PgStore) RemediateEntry(ctx context.Context, tenantID, jobID, entryID, notes string) (*domain.AuditEntry, error) {
	return nil, fmt.Errorf("not implemented")
}

// ─── MemoryStore ──────────────────────────────────────────────────────────────

type MemoryStore struct {
	mu   sync.RWMutex
	jobs map[string]*domain.MigrationJob
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: make(map[string]*domain.MigrationJob)}
}

func (m *MemoryStore) CreateJob(ctx context.Context, tenantID string, job *domain.MigrationJob, checks []domain.IntegrityCheck, entries []domain.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	job.TenantID = tenantID
	job.CreatedAt = now
	job.UpdatedAt = now
	for i := range checks {
		checks[i].ID = uuid.New().String()
		checks[i].TenantID = tenantID
		checks[i].JobID = job.ID
		checks[i].CreatedAt = now
	}
	for i := range entries {
		entries[i].ID = uuid.New().String()
		entries[i].TenantID = tenantID
		entries[i].JobID = job.ID
		entries[i].CreatedAt = now
	}
	job.IntegrityChecks = checks
	job.AuditEntries = entries
	m.jobs[job.ID] = job
	return nil
}

func (m *MemoryStore) GetJobByID(ctx context.Context, tenantID, id string) (*domain.MigrationJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	if !ok || j.TenantID != tenantID {
		return nil, fmt.Errorf("migration job not found")
	}
	return j, nil
}

func (m *MemoryStore) ListJobs(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.MigrationJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.MigrationJob
	for _, j := range m.jobs {
		if j.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && j.LegalEntityID != legalEntityID {
			continue
		}
		if status != "" && string(j.Status) != status {
			continue
		}
		result = append(result, *j)
	}
	return result, nil
}

func (m *MemoryStore) ArchiveJob(ctx context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || j.TenantID != tenantID {
		return fmt.Errorf("migration job not found")
	}
	j.Status = domain.JobStatusArchived
	j.UpdatedAt = time.Now()
	return nil
}

func (m *MemoryStore) RemediateEntry(ctx context.Context, tenantID, jobID, entryID, notes string) (*domain.AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[jobID]
	if !ok || j.TenantID != tenantID {
		return nil, fmt.Errorf("migration job not found")
	}
	for i := range j.AuditEntries {
		if j.AuditEntries[i].ID == entryID {
			j.AuditEntries[i].IsRemediated = true
			return &j.AuditEntries[i], nil
		}
	}
	return nil, fmt.Errorf("audit entry not found")
}
