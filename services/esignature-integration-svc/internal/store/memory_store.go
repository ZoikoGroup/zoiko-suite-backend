package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/middleware"
)

type MemoryStore struct {
	mu        sync.RWMutex
	envelopes map[string]*domain.SignatureEnvelope
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{envelopes: make(map[string]*domain.SignatureEnvelope)}
}

// Tenant scoping in this store mirrors PgStore exactly, on purpose. It is
// not "just the test double": a store that isolates tenants in Postgres
// but not in memory means every handler test asserting isolation passes
// against a fake that cannot fail, which is how the unscoped reads and the
// unscoped status write here survived. Both implementations answer the same
// question the same way, so a handler test proves something about
// production.
func (m *MemoryStore) CreateEnvelope(ctx context.Context, env *domain.SignatureEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if env.EnvelopeID == "" {
		env.EnvelopeID = uuid.New().String()
	}
	now := time.Now().UTC()
	env.CreatedAt = now
	env.UpdatedAt = now
	// Attribution comes from the verified context, never the caller's payload.
	env.TenantID = middleware.GetTenantID(ctx)
	m.envelopes[env.EnvelopeID] = env
	return nil
}

// GetEnvelopeByID is scoped to the caller's tenant. Returns
// ErrEnvelopeNotFound for another tenant's id — not a distinct forbidden
// error, so a probe cannot confirm the id exists.
func (m *MemoryStore) GetEnvelopeByID(ctx context.Context, id string) (*domain.SignatureEnvelope, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	env, ok := m.envelopes[id]
	if !ok || env.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrEnvelopeNotFound
	}
	return env, nil
}

// ListEnvelopes is scoped to the caller's tenant, with legal entity as an
// optional narrowing WITHIN that tenant.
func (m *MemoryStore) ListEnvelopes(ctx context.Context, legalEntityID string) ([]domain.SignatureEnvelope, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.SignatureEnvelope, 0)
	for _, env := range m.envelopes {
		if env.TenantID != tenantID {
			continue
		}
		if legalEntityID == "" || env.LegalEntityID == legalEntityID {
			res = append(res, *env)
		}
	}
	return res, nil
}

// UpdateEnvelopeStatus is scoped to the caller's tenant — the most
// serious hole in this service. Unscoped, any caller holding another
// tenant's envelope_id could mark that tenant's document signed or
// completed, on the service Doc 03 §16.5 defines as the governed
// execution path for contracts, board resolutions and legal artifacts.
func (m *MemoryStore) UpdateEnvelopeStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.SignatureEnvelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envelopes[id]
	if !ok || env.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrEnvelopeNotFound
	}
	env.Status = req.Status
	env.ExternalRef = req.ExternalRef
	env.UpdatedAt = time.Now().UTC()
	return env, nil
}
