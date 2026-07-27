package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/key-management-svc/internal/domain"
)

type Store interface {
	CreateKey(ctx context.Context, tenantID string, key *domain.CustomerKey) error
	GetKeyByID(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error)
	ListKeys(ctx context.Context, tenantID, legalEntityID string) ([]domain.CustomerKey, error)
	RotateKey(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error)
	DisableKey(ctx context.Context, tenantID, id string) error
}

type MemoryStore struct {
	mu   sync.RWMutex
	keys map[string]*domain.CustomerKey
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{keys: make(map[string]*domain.CustomerKey)}
}

func (m *MemoryStore) CreateKey(ctx context.Context, tenantID string, key *domain.CustomerKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key.ID = uuid.New().String()
	key.TenantID = tenantID
	key.KeyVersion = 1
	key.State = domain.StateEnabled
	key.CreatedAt = time.Now()
	key.UpdatedAt = time.Now()
	m.keys[key.ID] = key
	return nil
}

func (m *MemoryStore) GetKeyByID(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID {
		return nil, fmt.Errorf("key not found")
	}
	return k, nil
}

func (m *MemoryStore) ListKeys(ctx context.Context, tenantID, legalEntityID string) ([]domain.CustomerKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.CustomerKey
	for _, k := range m.keys {
		if k.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && k.LegalEntityID != legalEntityID {
			continue
		}
		result = append(result, *k)
	}
	return result, nil
}

func (m *MemoryStore) RotateKey(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID {
		return nil, fmt.Errorf("key not found")
	}
	now := time.Now()
	k.KeyVersion++
	k.RotationCount++
	k.LastRotatedAt = &now
	k.UpdatedAt = now
	return k, nil
}

func (m *MemoryStore) DisableKey(ctx context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID {
		return fmt.Errorf("key not found")
	}
	k.State = domain.StateDisabled
	k.UpdatedAt = time.Now()
	return nil
}
