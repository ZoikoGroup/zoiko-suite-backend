package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/mtls-management-svc/internal/domain"
)

type Store interface {
	CreateCert(ctx context.Context, tenantID string, cert *domain.MtlsCertificate) error
	GetCertByID(ctx context.Context, tenantID, id string) (*domain.MtlsCertificate, error)
	ListCerts(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.MtlsCertificate, error)
	RotateCert(ctx context.Context, tenantID, id string) (*domain.MtlsCertificate, error)
	RevokeCert(ctx context.Context, tenantID, id string) error
	CreatePolicy(ctx context.Context, tenantID string, pol *domain.CommunicationPolicy) error
	ListPolicies(ctx context.Context, tenantID string) ([]domain.CommunicationPolicy, error)
}

type MemoryStore struct {
	mu       sync.RWMutex
	certs    map[string]*domain.MtlsCertificate
	policies map[string]*domain.CommunicationPolicy
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		certs:    make(map[string]*domain.MtlsCertificate),
		policies: make(map[string]*domain.CommunicationPolicy),
	}
}

func (m *MemoryStore) CreateCert(ctx context.Context, tenantID string, cert *domain.MtlsCertificate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cert.ID = uuid.New().String()
	m.certs[cert.ID] = cert
	return nil
}

func (m *MemoryStore) GetCertByID(ctx context.Context, tenantID, id string) (*domain.MtlsCertificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.certs[id]
	if !ok || c.TenantID != tenantID {
		return nil, fmt.Errorf("certificate not found")
	}
	return c, nil
}

func (m *MemoryStore) ListCerts(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.MtlsCertificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.MtlsCertificate
	for _, c := range m.certs {
		if c.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && c.LegalEntityID != legalEntityID {
			continue
		}
		if status != "" && string(c.Status) != status {
			continue
		}
		result = append(result, *c)
	}
	return result, nil
}

func (m *MemoryStore) RotateCert(ctx context.Context, tenantID, id string) (*domain.MtlsCertificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.certs[id]
	if !ok || c.TenantID != tenantID {
		return nil, fmt.Errorf("certificate not found")
	}
	now := time.Now()
	c.SerialNumber = fmt.Sprintf("SN-ROT-%d", now.UnixNano())
	c.Fingerprint = fmt.Sprintf("SHA256:%x", now.UnixNano()*99991)
	c.ValidFrom = now
	c.ValidTo = now.AddDate(0, 0, c.RotationDays)
	c.Status = domain.CertStatusActive
	c.UpdatedAt = now
	return c, nil
}

func (m *MemoryStore) RevokeCert(ctx context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.certs[id]
	if !ok || c.TenantID != tenantID {
		return fmt.Errorf("certificate not found")
	}
	c.Status = domain.CertStatusRevoked
	c.UpdatedAt = time.Now()
	return nil
}

func (m *MemoryStore) CreatePolicy(ctx context.Context, tenantID string, pol *domain.CommunicationPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pol.ID = uuid.New().String()
	pol.TenantID = tenantID
	pol.CreatedAt = time.Now()
	m.policies[pol.ID] = pol
	return nil
}

func (m *MemoryStore) ListPolicies(ctx context.Context, tenantID string) ([]domain.CommunicationPolicy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.CommunicationPolicy
	for _, p := range m.policies {
		if p.TenantID == tenantID {
			result = append(result, *p)
		}
	}
	return result, nil
}
