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

// ErrKeyNotFound is returned by every Store when no customer key with that ID
// is visible to the tenant. It is the ONLY error the handler answers with 404:
// anything else is the store failing (a database or connection error), which
// is 503 — never "not found", and never a false-empty list. Another tenant's
// key is indistinguishable from a missing one on purpose.
var ErrKeyNotFound = errors.New("key not found")

// ErrNotFound is ErrKeyNotFound under the name the persistence and outbox
// tests use; errors.Is matches either.
var ErrNotFound = ErrKeyNotFound

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

// MemoryStore is the in-process Store and Vault. It is not what cmd/server
// runs — that is PgStore — and is kept as the fast test double the handler
// tests use. It loses everything on restart, which is exactly why it cannot
// be the production store.
type MemoryStore struct {
	mu       sync.RWMutex
	keys     map[string]*domain.CustomerKey
	versions map[string][]domain.KeyVersion
	tokens   map[string]*domain.TokenizedValue
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		keys:     make(map[string]*domain.CustomerKey),
		versions: make(map[string][]domain.KeyVersion),
		tokens:   make(map[string]*domain.TokenizedValue),
	}
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
		return nil, ErrKeyNotFound
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
		return nil, ErrKeyNotFound
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
		return ErrKeyNotFound
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
		return nil, ErrKeyNotFound
	}
	return append([]domain.KeyVersion(nil), m.versions[id]...), nil
}
