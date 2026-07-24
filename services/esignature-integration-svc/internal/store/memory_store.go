package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/esignature-integration-svc/internal/domain"
)

type MemoryStore struct {
	mu        sync.RWMutex
	envelopes map[string]*domain.SignatureEnvelope
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{envelopes: make(map[string]*domain.SignatureEnvelope)}
}

func (m *MemoryStore) CreateEnvelope(ctx context.Context, env *domain.SignatureEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if env.EnvelopeID == "" {
		env.EnvelopeID = uuid.New().String()
	}
	now := time.Now().UTC()
	env.CreatedAt = now
	env.UpdatedAt = now
	m.envelopes[env.EnvelopeID] = env
	return nil
}

func (m *MemoryStore) GetEnvelopeByID(ctx context.Context, id string) (*domain.SignatureEnvelope, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	env, ok := m.envelopes[id]
	if !ok {
		return nil, domain.ErrEnvelopeNotFound
	}
	return env, nil
}

func (m *MemoryStore) ListEnvelopes(ctx context.Context, legalEntityID string) ([]domain.SignatureEnvelope, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]domain.SignatureEnvelope, 0)
	for _, env := range m.envelopes {
		if legalEntityID == "" || env.LegalEntityID == legalEntityID {
			res = append(res, *env)
		}
	}
	return res, nil
}

func (m *MemoryStore) UpdateEnvelopeStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.SignatureEnvelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envelopes[id]
	if !ok {
		return nil, domain.ErrEnvelopeNotFound
	}
	env.Status = req.Status
	env.ExternalRef = req.ExternalRef
	env.UpdatedAt = time.Now().UTC()
	return env, nil
}
