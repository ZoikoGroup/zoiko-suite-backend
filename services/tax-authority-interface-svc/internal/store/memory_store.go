package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
)

type MemoryStore struct {
	mu          sync.RWMutex
	interfaces  map[string]*domain.TaxInterface
	submissions map[string]*domain.TaxFilingSubmission
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		interfaces:  make(map[string]*domain.TaxInterface),
		submissions: make(map[string]*domain.TaxFilingSubmission),
	}
}

// Tenant scoping in this store mirrors PgStore exactly, on purpose. It is
// not "just the test double": a store that isolates tenants in Postgres
// but not in memory means every handler test asserting isolation passes
// against a fake that cannot fail, which is how the unscoped reads here
// survived. Both implementations answer the same question the same way, so
// a handler test proves something about production.
func (m *MemoryStore) CreateInterface(ctx context.Context, tf *domain.TaxInterface) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if tf.InterfaceID == "" {
		tf.InterfaceID = uuid.New().String()
	}
	now := time.Now().UTC()
	tf.CreatedAt = now
	tf.UpdatedAt = now
	// Attribution comes from the verified context, never the caller's payload.
	tf.TenantID = middleware.GetTenantID(ctx)

	m.interfaces[tf.InterfaceID] = tf
	return nil
}

// GetInterfaceByID is scoped to the caller's tenant. Returns
// ErrInterfaceNotFound for another tenant's id — not a distinct forbidden
// error, so a probe cannot confirm the id exists.
func (m *MemoryStore) GetInterfaceByID(ctx context.Context, id string) (*domain.TaxInterface, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tf, ok := m.interfaces[id]
	if !ok || tf.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrInterfaceNotFound
	}
	return tf, nil
}

// ListInterfaces is scoped to the caller's tenant, with legal entity as an
// optional narrowing WITHIN that tenant.
func (m *MemoryStore) ListInterfaces(ctx context.Context, legalEntityID string) ([]domain.TaxInterface, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.TaxInterface, 0)
	for _, tf := range m.interfaces {
		if tf.TenantID != tenantID {
			continue
		}
		if legalEntityID == "" || tf.LegalEntityID == legalEntityID {
			res = append(res, *tf)
		}
	}
	return res, nil
}

func (m *MemoryStore) CreateSubmission(ctx context.Context, sub *domain.TaxFilingSubmission) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sub.SubmissionID == "" {
		sub.SubmissionID = uuid.New().String()
	}
	sub.SubmittedAt = time.Now().UTC()
	sub.TenantID = middleware.GetTenantID(ctx)

	m.submissions[sub.SubmissionID] = sub
	return nil
}

// GetSubmissionByID is scoped to the caller's tenant — the most sensitive
// read in this service. Unscoped, any caller holding another tenant's
// submission_id could read its tax_amount, tax_period, filing_type and
// ack_reference: that tenant's actual tax filing figures.
func (m *MemoryStore) GetSubmissionByID(ctx context.Context, id string) (*domain.TaxFilingSubmission, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sub, ok := m.submissions[id]
	if !ok || sub.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrFilingNotFound
	}
	return sub, nil
}

// ListSubmissions is scoped to the caller's tenant, with interface as an
// optional narrowing WITHIN that tenant. Unscoped, the interface filter
// was the ONLY predicate, so calling this with no interface_id returned
// every tenant's tax filings, amounts included.
func (m *MemoryStore) ListSubmissions(ctx context.Context, interfaceID string) ([]domain.TaxFilingSubmission, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.TaxFilingSubmission, 0)
	for _, sub := range m.submissions {
		if sub.TenantID != tenantID {
			continue
		}
		if interfaceID == "" || sub.InterfaceID == interfaceID {
			res = append(res, *sub)
		}
	}
	return res, nil
}
