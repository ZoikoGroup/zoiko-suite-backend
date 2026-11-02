package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/key-management-svc/internal/domain"
)

// ErrNotFound: no key with that id in the caller's tenant. Another tenant's
// key is indistinguishable from a missing one on purpose.
var ErrNotFound = errors.New("key not found")

// Store persists customer key METADATA and its version history. No
// implementation stores key material. PgStore is the production
// implementation; MemoryStore is for unit tests.
type Store interface {
	CreateKey(ctx context.Context, tenantID string, key *domain.CustomerKey) error
	GetKeyByID(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error)
	ListKeys(ctx context.Context, tenantID, legalEntityID string) ([]domain.CustomerKey, error)
	// RotateKey atomically retires the current version and records the next
	// one as current, advancing key_version and rotation_count.
	RotateKey(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error)
	DisableKey(ctx context.Context, tenantID, id string) error
	// ListKeyVersions returns a key's version history, oldest first.
	ListKeyVersions(ctx context.Context, tenantID, id string) ([]domain.KeyVersion, error)
}

// MemoryStore keeps key metadata in process memory. Unit tests only: it
// loses every key and its lifecycle on restart, so cmd/server never uses it.
type MemoryStore struct {
	mu       sync.RWMutex
	keys     map[string]*domain.CustomerKey
	versions map[string][]domain.KeyVersion
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{keys: make(map[string]*domain.CustomerKey), versions: make(map[string][]domain.KeyVersion)}
}

func (m *MemoryStore) CreateKey(ctx context.Context, tenantID string, key *domain.CustomerKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	key.ID = uuid.New().String()
	key.TenantID = tenantID
	key.KeyVersion = 1
	key.RotationCount = 0
	key.State = domain.StateEnabled
	key.CreatedAt = now
	key.UpdatedAt = now
	stored := *key
	m.keys[key.ID] = &stored
	m.versions[key.ID] = []domain.KeyVersion{{KeyID: key.ID, Version: 1, Status: domain.VersionCurrent, ActivatedAt: now}}
	return nil
}

func (m *MemoryStore) GetKeyByID(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID {
		return nil, ErrNotFound
	}
	out := *k
	return &out, nil
}

// ListKeys: tenant and legal entity are both mandatory equality filters —
// legalEntityID is the authorization scope, so an empty one matches nothing
// rather than everything.
func (m *MemoryStore) ListKeys(ctx context.Context, tenantID, legalEntityID string) ([]domain.CustomerKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := []domain.CustomerKey{}
	for _, k := range m.keys {
		if k.TenantID != tenantID || k.LegalEntityID != legalEntityID {
			continue
		}
		result = append(result, *k)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (m *MemoryStore) RotateKey(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID {
		return nil, ErrNotFound
	}
	now := time.Now()
	vs := m.versions[id]
	for i := range vs {
		if vs[i].Status == domain.VersionCurrent {
			vs[i].Status = domain.VersionRetired
			retired := now
			vs[i].RetiredAt = &retired
		}
	}
	k.KeyVersion++
	k.RotationCount++
	k.LastRotatedAt = &now
	k.UpdatedAt = now
	m.versions[id] = append(vs, domain.KeyVersion{KeyID: id, Version: k.KeyVersion, Status: domain.VersionCurrent, ActivatedAt: now})
	out := *k
	return &out, nil
}

func (m *MemoryStore) DisableKey(ctx context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID {
		return ErrNotFound
	}
	k.State = domain.StateDisabled
	k.UpdatedAt = time.Now()
	return nil
}

func (m *MemoryStore) ListKeyVersions(ctx context.Context, tenantID, id string) ([]domain.KeyVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return append([]domain.KeyVersion(nil), m.versions[id]...), nil
}
