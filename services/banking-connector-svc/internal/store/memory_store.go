package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
)

type MemoryStore struct {
	mu          sync.RWMutex
	connections map[string]*domain.BankConnection
	statements  map[string][]*domain.BankStatement
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		connections: make(map[string]*domain.BankConnection),
		statements:  make(map[string][]*domain.BankStatement),
	}
}

// Tenant scoping in this store mirrors PgStore exactly, on purpose. It is
// not "just the test double": a store that isolates tenants in Postgres
// but not in memory means every handler test asserting isolation passes
// against a fake that cannot fail, which is how the unscoped reads here
// survived in the first place. Both implementations answer the same
// question the same way, so a test proves something about production.
func (m *MemoryStore) CreateConnection(ctx context.Context, c *domain.BankConnection) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if c.ConnectionID == "" {
		c.ConnectionID = uuid.New().String()
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	// Attribution comes from the verified context, never from the caller's
	// payload — same as PgStore.CreateConnection.
	c.TenantID = middleware.GetTenantID(ctx)

	m.connections[c.ConnectionID] = c
	return nil
}

// GetConnectionByID is scoped to the caller's tenant. Returns
// ErrConnectionNotFound for another tenant's id — not a distinct
// forbidden error, so a probe cannot confirm the id exists.
func (m *MemoryStore) GetConnectionByID(ctx context.Context, id string) (*domain.BankConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	c, ok := m.connections[id]
	if !ok || c.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrConnectionNotFound
	}
	return c, nil
}

// ListConnections is scoped to the caller's tenant, with legal entity as
// an optional narrowing WITHIN that tenant. The legal-entity filter is
// legitimately optional; the tenant filter never is.
func (m *MemoryStore) ListConnections(ctx context.Context, legalEntityID string) ([]domain.BankConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.BankConnection, 0)
	for _, c := range m.connections {
		if c.TenantID != tenantID {
			continue
		}
		if legalEntityID == "" || c.LegalEntityID == legalEntityID {
			res = append(res, *c)
		}
	}
	return res, nil
}

func (m *MemoryStore) RecordStatement(ctx context.Context, stmt *domain.BankStatement) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if stmt.StatementID == "" {
		stmt.StatementID = uuid.New().String()
	}
	stmt.IngestedAt = time.Now().UTC()
	stmt.TenantID = middleware.GetTenantID(ctx)

	m.statements[stmt.ConnectionID] = append(m.statements[stmt.ConnectionID], stmt)
	return nil
}

// ListStatements is scoped to the caller's tenant. Keyed by connection_id
// alone, this returned another tenant's balances and transaction counts to
// anyone holding that id.
func (m *MemoryStore) ListStatements(ctx context.Context, connectionID string) ([]domain.BankStatement, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stmts, ok := m.statements[connectionID]
	if !ok {
		return []domain.BankStatement{}, nil
	}

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.BankStatement, 0, len(stmts))
	for _, s := range stmts {
		if s.TenantID != tenantID {
			continue
		}
		res = append(res, *s)
	}
	return res, nil
}
