package store

import (
	"context"
	"errors"
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
	// ReplaceCertMaterial overwrites an existing certificate's crypto
	// material (serial number, fingerprint, PEM, validity window) with a
	// freshly CA-issued replacement, keeping the same record ID and
	// identity fields (service name, common name, rotation policy). The
	// caller (handler) is responsible for actually issuing the new
	// material via the CA — the store never generates cryptographic
	// material itself, real or fake.
	//
	// A REVOKED record is refused with ErrCertRevoked: rotation used to set the
	// status back to ACTIVE, so "rotate" silently un-revoked a certificate. The
	// superseded serial is kept in PriorSerialNumbers so a later revocation
	// still covers it.
	ReplaceCertMaterial(ctx context.Context, tenantID, id string, serialNumber, fingerprint, certificatePEM string, validFrom, validTo time.Time) (*domain.MtlsCertificate, error)
	// RevokeCert marks the record REVOKED (idempotently) and returns it as it
	// stands afterwards, so the caller revokes every serial it ever carried.
	RevokeCert(ctx context.Context, tenantID, id string) (*domain.MtlsCertificate, error)
	CreatePolicy(ctx context.Context, tenantID string, pol *domain.CommunicationPolicy) error
	ListPolicies(ctx context.Context, tenantID string) ([]domain.CommunicationPolicy, error)
}

// ErrCertRevoked refuses an operation that would bring a revoked
// certificate record back into use.
var ErrCertRevoked = errors.New("certificate is revoked")

// clone copies a record so callers never hold a pointer into the map: the
// handler used to receive the stored struct itself, and a concurrent rotate
// or revoke rewrote it underneath them.
func clone(c *domain.MtlsCertificate) *domain.MtlsCertificate {
	cp := *c
	cp.PriorSerialNumbers = append([]string(nil), c.PriorSerialNumbers...)
	return &cp
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
	// Stamp attribution from the verified tenant, never trust the caller to
	// have pre-populated it. This parameter was previously accepted and
	// silently dropped: the invariant held only because the handler happened
	// to set cert.TenantID itself, so any future caller that forgot would
	// create an unattributed certificate.
	cert.TenantID = tenantID
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
	return clone(c), nil
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
		result = append(result, *clone(c))
	}
	return result, nil
}

func (m *MemoryStore) ReplaceCertMaterial(ctx context.Context, tenantID, id string, serialNumber, fingerprint, certificatePEM string, validFrom, validTo time.Time) (*domain.MtlsCertificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.certs[id]
	if !ok || c.TenantID != tenantID {
		return nil, fmt.Errorf("certificate not found")
	}
	if c.Status == domain.CertStatusRevoked {
		return nil, ErrCertRevoked
	}
	if c.SerialNumber != "" {
		c.PriorSerialNumbers = append(c.PriorSerialNumbers, c.SerialNumber)
	}
	c.SerialNumber = serialNumber
	c.Fingerprint = fingerprint
	c.CertificatePEM = certificatePEM
	c.ValidFrom = validFrom
	c.ValidTo = validTo
	c.UpdatedAt = time.Now()
	return clone(c), nil
}

func (m *MemoryStore) RevokeCert(ctx context.Context, tenantID, id string) (*domain.MtlsCertificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.certs[id]
	if !ok || c.TenantID != tenantID {
		return nil, fmt.Errorf("certificate not found")
	}
	if c.Status != domain.CertStatusRevoked {
		c.Status = domain.CertStatusRevoked
		c.UpdatedAt = time.Now()
	}
	return clone(c), nil
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
