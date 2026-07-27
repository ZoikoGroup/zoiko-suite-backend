package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

type Store interface {
	CreateJob(ctx context.Context, tenantID string, job *domain.ReconciliationJob, items []domain.UnmatchedItem) error
	GetJobByID(ctx context.Context, tenantID, id string) (*domain.ReconciliationJob, error)
	ListJobs(ctx context.Context, tenantID, legalEntityID, sourceA, status string) ([]domain.ReconciliationJob, error)
	ApplyResolution(ctx context.Context, tenantID, jobID, itemID string, status domain.ResolutionStatus, notes string) (*domain.UnmatchedItem, error)
	ArchiveJob(ctx context.Context, tenantID, id string) error
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) CreateJob(ctx context.Context, tenantID string, job *domain.ReconciliationJob, items []domain.UnmatchedItem) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return err
	}

	job.ID = uuid.New().String()
	job.TenantID = tenantID
	job.AnalyzedAt = time.Now()
	job.CreatedAt = time.Now()

	queryJob := `
		INSERT INTO reconciliation_jobs (
			id, tenant_id, legal_entity_id, job_name, source_system_a, source_system_b,
			total_processed_count, matched_count, unmatched_count, reconciliation_rate, status, analyzed_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

	_, err = tx.Exec(ctx, queryJob,
		job.ID, tenantID, job.LegalEntityID, job.JobName, string(job.SourceSystemA), string(job.SourceSystemB),
		job.TotalProcessedCount, job.MatchedCount, job.UnmatchedCount, job.ReconciliationRate, job.Status,
		job.AnalyzedAt, job.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert reconciliation_job failed: %w", err)
	}

	queryItem := `
		INSERT INTO reconciliation_unmatched_items (
			id, tenant_id, job_id, transaction_ref_a, transaction_ref_b, amount_a, amount_b,
			discrepancy_amount, discrepancy_type, confidence_score, recommendation, resolution_status, resolution_notes, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

	for i := range items {
		item := &items[i]
		item.ID = uuid.New().String()
		item.TenantID = tenantID
		item.JobID = job.ID
		item.CreatedAt = time.Now()
		item.UpdatedAt = time.Now()

		_, err := tx.Exec(ctx, queryItem,
			item.ID, tenantID, job.ID, item.TransactionRefA, item.TransactionRefB, item.AmountA, item.AmountB,
			item.DiscrepancyAmount, string(item.DiscrepancyType), item.ConfidenceScore, string(item.Recommendation),
			string(item.ResolutionStatus), item.ResolutionNotes, item.CreatedAt, item.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert reconciliation_unmatched_item failed: %w", err)
		}
	}

	job.UnmatchedItems = items
	return tx.Commit(ctx)
}

func (s *PgStore) GetJobByID(ctx context.Context, tenantID, id string) (*domain.ReconciliationJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return nil, err
	}

	var j domain.ReconciliationJob
	queryJob := `
		SELECT id, tenant_id, legal_entity_id, job_name, source_system_a, source_system_b,
		       total_processed_count, matched_count, unmatched_count, reconciliation_rate, status, analyzed_at, created_at
		FROM reconciliation_jobs WHERE id = $1`

	err = tx.QueryRow(ctx, queryJob, id).Scan(
		&j.ID, &j.TenantID, &j.LegalEntityID, &j.JobName, &j.SourceSystemA, &j.SourceSystemB,
		&j.TotalProcessedCount, &j.MatchedCount, &j.UnmatchedCount, &j.ReconciliationRate, &j.Status, &j.AnalyzedAt, &j.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("reconciliation job not found: %w", err)
	}

	queryItem := `
		SELECT id, tenant_id, job_id, transaction_ref_a, transaction_ref_b, amount_a, amount_b,
		       discrepancy_amount, discrepancy_type, confidence_score, recommendation, resolution_status, COALESCE(resolution_notes, ''), created_at, updated_at
		FROM reconciliation_unmatched_items WHERE job_id = $1`

	rows, err := tx.Query(ctx, queryItem, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var item domain.UnmatchedItem
			_ = rows.Scan(&item.ID, &item.TenantID, &item.JobID, &item.TransactionRefA, &item.TransactionRefB,
				&item.AmountA, &item.AmountB, &item.DiscrepancyAmount, &item.DiscrepancyType, &item.ConfidenceScore,
				&item.Recommendation, &item.ResolutionStatus, &item.ResolutionNotes, &item.CreatedAt, &item.UpdatedAt)
			j.UnmatchedItems = append(j.UnmatchedItems, item)
		}
	}

	return &j, nil
}

func (s *PgStore) ListJobs(ctx context.Context, tenantID, legalEntityID, sourceA, status string) ([]domain.ReconciliationJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return nil, err
	}

	query := `SELECT id, tenant_id, legal_entity_id, job_name, source_system_a, source_system_b,
	                 total_processed_count, matched_count, unmatched_count, reconciliation_rate, status, analyzed_at, created_at
	          FROM reconciliation_jobs WHERE tenant_id = $1`

	args := []interface{}{tenantID}
	if legalEntityID != "" {
		args = append(args, legalEntityID)
		query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
	}
	if sourceA != "" {
		args = append(args, sourceA)
		query += fmt.Sprintf(" AND source_system_a = $%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	query += " ORDER BY analyzed_at DESC"

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []domain.ReconciliationJob
	for rows.Next() {
		var j domain.ReconciliationJob
		err := rows.Scan(
			&j.ID, &j.TenantID, &j.LegalEntityID, &j.JobName, &j.SourceSystemA, &j.SourceSystemB,
			&j.TotalProcessedCount, &j.MatchedCount, &j.UnmatchedCount, &j.ReconciliationRate, &j.Status, &j.AnalyzedAt, &j.CreatedAt,
		)
		if err == nil {
			jobs = append(jobs, j)
		}
	}

	return jobs, nil
}

func (s *PgStore) ApplyResolution(ctx context.Context, tenantID, jobID, itemID string, status domain.ResolutionStatus, notes string) (*domain.UnmatchedItem, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return nil, err
	}

	var item domain.UnmatchedItem
	queryItem := `
		SELECT id, tenant_id, job_id, transaction_ref_a, transaction_ref_b, amount_a, amount_b,
		       discrepancy_amount, discrepancy_type, confidence_score, recommendation, resolution_status, COALESCE(resolution_notes, ''), created_at, updated_at
		FROM reconciliation_unmatched_items WHERE id = $1 AND job_id = $2`

	err = tx.QueryRow(ctx, queryItem, itemID, jobID).Scan(
		&item.ID, &item.TenantID, &item.JobID, &item.TransactionRefA, &item.TransactionRefB,
		&item.AmountA, &item.AmountB, &item.DiscrepancyAmount, &item.DiscrepancyType, &item.ConfidenceScore,
		&item.Recommendation, &item.ResolutionStatus, &item.ResolutionNotes, &item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("unmatched item not found: %w", err)
	}

	item.ResolutionStatus = status
	item.ResolutionNotes = notes
	item.UpdatedAt = time.Now()

	updateQuery := `UPDATE reconciliation_unmatched_items SET resolution_status = $1, resolution_notes = $2, updated_at = $3 WHERE id = $4`
	_, err = tx.Exec(ctx, updateQuery, string(status), notes, item.UpdatedAt, itemID)
	if err != nil {
		return nil, err
	}

	_ = tx.Commit(ctx)
	return &item, nil
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

	cmd, err := tx.Exec(ctx, "UPDATE reconciliation_jobs SET status = 'ARCHIVED' WHERE id = $1", id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("reconciliation job not found")
	}

	return tx.Commit(ctx)
}

// In-Memory Store for Testing & Fallback
type MemoryStore struct {
	mu   sync.RWMutex
	jobs map[string]*domain.ReconciliationJob
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		jobs: make(map[string]*domain.ReconciliationJob),
	}
}

func (m *MemoryStore) CreateJob(ctx context.Context, tenantID string, job *domain.ReconciliationJob, items []domain.UnmatchedItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job.ID = uuid.New().String()
	job.TenantID = tenantID
	job.AnalyzedAt = time.Now()
	job.CreatedAt = time.Now()

	for i := range items {
		items[i].ID = uuid.New().String()
		items[i].TenantID = tenantID
		items[i].JobID = job.ID
		items[i].CreatedAt = time.Now()
		items[i].UpdatedAt = time.Now()
	}

	job.UnmatchedItems = items
	m.jobs[job.ID] = job
	return nil
}

func (m *MemoryStore) GetJobByID(ctx context.Context, tenantID, id string) (*domain.ReconciliationJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	j, exists := m.jobs[id]
	if !exists || j.TenantID != tenantID {
		return nil, fmt.Errorf("reconciliation job not found")
	}
	return j, nil
}

func (m *MemoryStore) ListJobs(ctx context.Context, tenantID, legalEntityID, sourceA, status string) ([]domain.ReconciliationJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []domain.ReconciliationJob
	for _, j := range m.jobs {
		if j.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && j.LegalEntityID != legalEntityID {
			continue
		}
		if sourceA != "" && string(j.SourceSystemA) != sourceA {
			continue
		}
		if status != "" && j.Status != status {
			continue
		}
		result = append(result, *j)
	}
	return result, nil
}

func (m *MemoryStore) ApplyResolution(ctx context.Context, tenantID, jobID, itemID string, status domain.ResolutionStatus, notes string) (*domain.UnmatchedItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, exists := m.jobs[jobID]
	if !exists || j.TenantID != tenantID {
		return nil, fmt.Errorf("reconciliation job not found")
	}

	for i := range j.UnmatchedItems {
		if j.UnmatchedItems[i].ID == itemID {
			j.UnmatchedItems[i].ResolutionStatus = status
			j.UnmatchedItems[i].ResolutionNotes = notes
			j.UnmatchedItems[i].UpdatedAt = time.Now()
			return &j.UnmatchedItems[i], nil
		}
	}

	return nil, fmt.Errorf("unmatched item not found")
}

func (m *MemoryStore) ArchiveJob(ctx context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, exists := m.jobs[id]
	if !exists || j.TenantID != tenantID {
		return fmt.Errorf("reconciliation job not found")
	}

	j.Status = "ARCHIVED"
	return nil
}
