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

// Vault is the token-vault contract behind POST /v1/tokenize and
// /v1/detokenize. It is a separate interface from Store so the five key
// routes keep exactly the dependency they had.
//
// The vault stores and returns ciphertext only: encryption and decryption
// happen in the handler (internal/crypto), so nothing below this interface
// ever sees a raw value.
type Vault interface {
	// CreateToken inserts v under tenantID. If tenantID already has a row
	// with v.IdempotencyKey, nothing is inserted and that existing row is
	// returned with created=false, for the caller to compare against the
	// retried request.
	CreateToken(ctx context.Context, tenantID string, v *domain.TokenizedValue) (existing *domain.TokenizedValue, created bool, err error)
	// GetToken returns tenantID's row for tokenID, or domain.ErrTokenNotFound
	// when there is none visible to that tenant.
	GetToken(ctx context.Context, tenantID, tokenID string) (*domain.TokenizedValue, error)
}

// MemoryStore is the in-process Store and Vault. It is no longer what
// cmd/server runs — that is PgStore — and is kept as the fast test double the
// handler tests have always used. It loses everything on restart, which is
// exactly why it cannot be the production store.
type MemoryStore struct {
	mu     sync.RWMutex
	keys   map[string]*domain.CustomerKey
	tokens map[string]*domain.TokenizedValue
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{keys: make(map[string]*domain.CustomerKey), tokens: make(map[string]*domain.TokenizedValue)}
}

var (
	_ Store = (*MemoryStore)(nil)
	_ Vault = (*MemoryStore)(nil)
)

func (m *MemoryStore) CreateToken(_ context.Context, tenantID string, v *domain.TokenizedValue) (*domain.TokenizedValue, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if t.TenantID == tenantID && t.IdempotencyKey == v.IdempotencyKey {
			cp := *t
			return &cp, false, nil
		}
	}
	row := *v
	row.TenantID = tenantID
	row.CreatedAt = time.Now()
	m.tokens[row.TokenID] = &row
	return nil, true, nil
}

func (m *MemoryStore) GetToken(_ context.Context, tenantID, tokenID string) (*domain.TokenizedValue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tokens[tokenID]
	if !ok || t.TenantID != tenantID {
		return nil, domain.ErrTokenNotFound
	}
	cp := *t
	return &cp, nil
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
