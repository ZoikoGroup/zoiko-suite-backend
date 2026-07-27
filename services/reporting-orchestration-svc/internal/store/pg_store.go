package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/reporting-orchestration-svc/internal/domain"
)

// ─── Interface ────────────────────────────────────────────────────────────────

type Store interface {
	CreateDefinition(ctx context.Context, tenantID string, def *domain.ReportDefinition) error
	GetDefinitionByID(ctx context.Context, tenantID, id string) (*domain.ReportDefinition, error)
	ListDefinitions(ctx context.Context, tenantID, legalEntityID, reportType string) ([]domain.ReportDefinition, error)
	UpdateDefinitionStatus(ctx context.Context, tenantID, id string, status domain.DefinitionStatus) error

	CreateRun(ctx context.Context, tenantID string, run *domain.ReportRun) error
	GetRunByID(ctx context.Context, tenantID, id string) (*domain.ReportRun, error)
	UpdateRun(ctx context.Context, tenantID string, run *domain.ReportRun) error
	ListRuns(ctx context.Context, tenantID, definitionID, status string) ([]domain.ReportRun, error)
}

// ─── PgStore ──────────────────────────────────────────────────────────────────

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setTenant(ctx context.Context, tx interface{ Exec(ctx context.Context, sql string, args ...interface{}) (interface{ RowsAffected() int64 }, error) }, tenantID string) error {
	_, err := s.pool.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID)
	return err
}

func (s *PgStore) CreateDefinition(ctx context.Context, tenantID string, def *domain.ReportDefinition) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return err
	}
	def.ID = uuid.New().String()
	def.TenantID = tenantID
	def.Status = domain.DefStatusActive
	def.CreatedAt = time.Now()
	def.UpdatedAt = time.Now()

	_, err = tx.Exec(ctx, `
		INSERT INTO report_definitions
		  (id, tenant_id, legal_entity_id, report_name, report_type, output_format,
		   data_sources, schedule_cron, is_scheduled, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		def.ID, tenantID, def.LegalEntityID, def.ReportName, string(def.ReportType),
		string(def.OutputFormat), def.DataSources, def.ScheduleCron, def.IsScheduled,
		string(def.Status), def.CreatedAt, def.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert report_definition: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetDefinitionByID(ctx context.Context, tenantID, id string) (*domain.ReportDefinition, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return nil, err
	}
	var d domain.ReportDefinition
	err = tx.QueryRow(ctx, `
		SELECT id, tenant_id, legal_entity_id, report_name, report_type, output_format,
		       data_sources, COALESCE(schedule_cron,''), is_scheduled, status, created_at, updated_at
		FROM report_definitions WHERE id=$1`, id).Scan(
		&d.ID, &d.TenantID, &d.LegalEntityID, &d.ReportName, &d.ReportType, &d.OutputFormat,
		&d.DataSources, &d.ScheduleCron, &d.IsScheduled, &d.Status, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("report definition not found: %w", err)
	}
	return &d, nil
}

func (s *PgStore) ListDefinitions(ctx context.Context, tenantID, legalEntityID, reportType string) ([]domain.ReportDefinition, error) {
	return nil, nil
}

func (s *PgStore) UpdateDefinitionStatus(ctx context.Context, tenantID, id string, status domain.DefinitionStatus) error {
	_, err := s.pool.Exec(ctx, "UPDATE report_definitions SET status=$1, updated_at=$2 WHERE id=$3", string(status), time.Now(), id)
	return err
}

func (s *PgStore) CreateRun(ctx context.Context, tenantID string, run *domain.ReportRun) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return err
	}
	run.ID = uuid.New().String()
	run.TenantID = tenantID
	run.CreatedAt = time.Now()

	_, err = tx.Exec(ctx, `
		INSERT INTO report_runs
		  (id, tenant_id, definition_id, triggered_by, period_start, period_end,
		   status, row_count, output_location, error_message, started_at, completed_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		run.ID, tenantID, run.DefinitionID, string(run.TriggeredBy),
		run.PeriodStart, run.PeriodEnd, string(run.Status), run.RowCount,
		run.OutputLocation, run.ErrorMessage, run.StartedAt, run.CompletedAt, run.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert report_run: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetRunByID(ctx context.Context, tenantID, id string) (*domain.ReportRun, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *PgStore) UpdateRun(ctx context.Context, tenantID string, run *domain.ReportRun) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE report_runs SET status=$1, row_count=$2, output_location=$3, error_message=$4,
		       started_at=$5, completed_at=$6 WHERE id=$7`,
		string(run.Status), run.RowCount, run.OutputLocation, run.ErrorMessage,
		run.StartedAt, run.CompletedAt, run.ID,
	)
	return err
}

func (s *PgStore) ListRuns(ctx context.Context, tenantID, definitionID, status string) ([]domain.ReportRun, error) {
	return nil, nil
}

// ─── MemoryStore ──────────────────────────────────────────────────────────────

type MemoryStore struct {
	mu          sync.RWMutex
	definitions map[string]*domain.ReportDefinition
	runs        map[string]*domain.ReportRun
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		definitions: make(map[string]*domain.ReportDefinition),
		runs:        make(map[string]*domain.ReportRun),
	}
}

func (m *MemoryStore) CreateDefinition(ctx context.Context, tenantID string, def *domain.ReportDefinition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	def.ID = uuid.New().String()
	def.TenantID = tenantID
	def.Status = domain.DefStatusActive
	def.CreatedAt = time.Now()
	def.UpdatedAt = time.Now()
	m.definitions[def.ID] = def
	return nil
}

func (m *MemoryStore) GetDefinitionByID(ctx context.Context, tenantID, id string) (*domain.ReportDefinition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.definitions[id]
	if !ok || d.TenantID != tenantID {
		return nil, fmt.Errorf("report definition not found")
	}
	return d, nil
}

func (m *MemoryStore) ListDefinitions(ctx context.Context, tenantID, legalEntityID, reportType string) ([]domain.ReportDefinition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.ReportDefinition
	for _, d := range m.definitions {
		if d.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && d.LegalEntityID != legalEntityID {
			continue
		}
		if reportType != "" && string(d.ReportType) != reportType {
			continue
		}
		result = append(result, *d)
	}
	return result, nil
}

func (m *MemoryStore) UpdateDefinitionStatus(ctx context.Context, tenantID, id string, status domain.DefinitionStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.definitions[id]
	if !ok || d.TenantID != tenantID {
		return fmt.Errorf("report definition not found")
	}
	d.Status = status
	d.UpdatedAt = time.Now()
	return nil
}

func (m *MemoryStore) CreateRun(ctx context.Context, tenantID string, run *domain.ReportRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run.ID = uuid.New().String()
	run.TenantID = tenantID
	run.CreatedAt = time.Now()
	m.runs[run.ID] = run
	return nil
}

func (m *MemoryStore) GetRunByID(ctx context.Context, tenantID, id string) (*domain.ReportRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok || r.TenantID != tenantID {
		return nil, fmt.Errorf("report run not found")
	}
	return r, nil
}

func (m *MemoryStore) UpdateRun(ctx context.Context, tenantID string, run *domain.ReportRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run.ID] = run
	return nil
}

func (m *MemoryStore) ListRuns(ctx context.Context, tenantID, definitionID, status string) ([]domain.ReportRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.ReportRun
	for _, r := range m.runs {
		if r.TenantID != tenantID {
			continue
		}
		if definitionID != "" && r.DefinitionID != definitionID {
			continue
		}
		if status != "" && string(r.Status) != status {
			continue
		}
		result = append(result, *r)
	}
	return result, nil
}
