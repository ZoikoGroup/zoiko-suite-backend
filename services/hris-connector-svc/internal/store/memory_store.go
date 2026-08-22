package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/hris-connector-svc/internal/domain"
	"zoiko.io/hris-connector-svc/internal/middleware"
)

type MemoryStore struct {
	mu           sync.RWMutex
	integrations map[string]*domain.HrisIntegration
	jobs         map[string]*domain.SyncJob
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		integrations: make(map[string]*domain.HrisIntegration),
		jobs:         make(map[string]*domain.SyncJob),
	}
}

// Tenant scoping in this store mirrors PgStore exactly, on purpose. It is
// not "just the test double": a store that isolates tenants in Postgres
// but not in memory means every handler test asserting isolation passes
// against a fake that cannot fail, which is how the unscoped reads here
// survived. Both implementations answer the same question the same way, so
// a handler test proves something about production.
func (m *MemoryStore) CreateIntegration(ctx context.Context, integration *domain.HrisIntegration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if integration.IntegrationID == "" {
		integration.IntegrationID = uuid.New().String()
	}
	now := time.Now().UTC()
	integration.CreatedAt = now
	integration.UpdatedAt = now
	// Attribution comes from the verified context, never the caller's payload.
	integration.TenantID = middleware.GetTenantID(ctx)

	m.integrations[integration.IntegrationID] = integration
	return nil
}

// GetIntegrationByID is scoped to the caller's tenant. Returns
// ErrIntegrationNotFound for another tenant's id — not a distinct
// forbidden error, so a probe cannot confirm the id exists.
func (m *MemoryStore) GetIntegrationByID(ctx context.Context, id string) (*domain.HrisIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	i, ok := m.integrations[id]
	if !ok || i.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrIntegrationNotFound
	}
	return i, nil
}

// ListIntegrations is scoped to the caller's tenant, with legal entity as
// an optional narrowing WITHIN that tenant.
func (m *MemoryStore) ListIntegrations(ctx context.Context, legalEntityID string) ([]domain.HrisIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.HrisIntegration, 0)
	for _, i := range m.integrations {
		if i.TenantID != tenantID {
			continue
		}
		if legalEntityID == "" || i.LegalEntityID == legalEntityID {
			res = append(res, *i)
		}
	}
	return res, nil
}

func (m *MemoryStore) CreateSyncJob(ctx context.Context, job *domain.SyncJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if job.JobID == "" {
		job.JobID = uuid.New().String()
	}
	job.StartedAt = time.Now().UTC()
	job.TenantID = middleware.GetTenantID(ctx)

	m.jobs[job.JobID] = job
	return nil
}

// GetSyncJobByID is scoped to the caller's tenant.
func (m *MemoryStore) GetSyncJobByID(ctx context.Context, id string) (*domain.SyncJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	j, ok := m.jobs[id]
	if !ok || j.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrSyncJobNotFound
	}
	return j, nil
}

// ListSyncJobs is scoped to the caller's tenant, with integration as an
// optional narrowing WITHIN that tenant. Unscoped, the integration filter
// was the ONLY predicate, so calling this with no integration_id returned
// every tenant's sync history.
func (m *MemoryStore) ListSyncJobs(ctx context.Context, integrationID string) ([]domain.SyncJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.SyncJob, 0)
	for _, j := range m.jobs {
		if j.TenantID != tenantID {
			continue
		}
		if integrationID == "" || j.IntegrationID == integrationID {
			res = append(res, *j)
		}
	}
	return res, nil
}
