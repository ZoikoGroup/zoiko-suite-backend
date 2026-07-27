package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/banking-connector-svc/internal/domain"
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

func (m *MemoryStore) CreateConnection(ctx context.Context, c *domain.BankConnection) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if c.ConnectionID == "" {
		c.ConnectionID = uuid.New().String()
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now

	m.connections[c.ConnectionID] = c
	return nil
}

func (m *MemoryStore) GetConnectionByID(ctx context.Context, id string) (*domain.BankConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	c, ok := m.connections[id]
	if !ok {
		return nil, domain.ErrConnectionNotFound
	}
	return c, nil
}

func (m *MemoryStore) ListConnections(ctx context.Context, legalEntityID string) ([]domain.BankConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	res := make([]domain.BankConnection, 0)
	for _, c := range m.connections {
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

	m.statements[stmt.ConnectionID] = append(m.statements[stmt.ConnectionID], stmt)
	return nil
}

func (m *MemoryStore) ListStatements(ctx context.Context, connectionID string) ([]domain.BankStatement, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stmts, ok := m.statements[connectionID]
	if !ok {
		return []domain.BankStatement{}, nil
	}

	res := make([]domain.BankStatement, len(stmts))
	for i, s := range stmts {
		res[i] = *s
	}
	return res, nil
}
