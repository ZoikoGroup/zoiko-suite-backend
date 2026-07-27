package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/hris-connector-svc/internal/domain"
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

func (m *MemoryStore) CreateIntegration(ctx context.Context, integration *domain.HrisIntegration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if integration.IntegrationID == "" {
		integration.IntegrationID = uuid.New().String()
	}
	now := time.Now().UTC()
	integration.CreatedAt = now
	integration.UpdatedAt = now

	m.integrations[integration.IntegrationID] = integration
	return nil
}

func (m *MemoryStore) GetIntegrationByID(ctx context.Context, id string) (*domain.HrisIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	i, ok := m.integrations[id]
	if !ok {
		return nil, domain.ErrIntegrationNotFound
	}
	return i, nil
}

func (m *MemoryStore) ListIntegrations(ctx context.Context, legalEntityID string) ([]domain.HrisIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	res := make([]domain.HrisIntegration, 0)
	for _, i := range m.integrations {
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

	m.jobs[job.JobID] = job
	return nil
}

func (m *MemoryStore) GetSyncJobByID(ctx context.Context, id string) (*domain.SyncJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	j, ok := m.jobs[id]
	if !ok {
		return nil, domain.ErrSyncJobNotFound
	}
	return j, nil
}

func (m *MemoryStore) ListSyncJobs(ctx context.Context, integrationID string) ([]domain.SyncJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	res := make([]domain.SyncJob, 0)
	for _, j := range m.jobs {
		if integrationID == "" || j.IntegrationID == integrationID {
			res = append(res, *j)
		}
	}
	return res, nil
}
