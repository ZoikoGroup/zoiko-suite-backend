package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

type MemoryStore struct {
	mu      sync.RWMutex
	bridges map[string]*domain.ApiBridge
	logs    map[string][]*domain.IngestionLog
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		bridges: make(map[string]*domain.ApiBridge),
		logs:    make(map[string][]*domain.IngestionLog),
	}
}

// Tenant scoping in this store mirrors PgStore exactly, on purpose. It is
// not "just the test double": a store that isolates tenants in Postgres
// but not in memory means every handler test asserting isolation passes
// against a fake that cannot fail, which is how the unscoped reads here
// survived. Both implementations answer the same question the same way, so
// a handler test proves something about production.
func (m *MemoryStore) CreateBridge(ctx context.Context, b *domain.ApiBridge) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if b.BridgeID == "" {
		b.BridgeID = uuid.New().String()
	}
	now := time.Now().UTC()
	b.CreatedAt = now
	b.UpdatedAt = now
	// Attribution comes from the verified context, never the caller's payload.
	b.TenantID = middleware.GetTenantID(ctx)

	m.bridges[b.BridgeID] = b
	return nil
}

// GetBridgeByID is scoped to the caller's tenant. Returns
// ErrBridgeNotFound for another tenant's id — not a distinct forbidden
// error, so a probe cannot confirm the id exists.
func (m *MemoryStore) GetBridgeByID(ctx context.Context, id string) (*domain.ApiBridge, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	b, ok := m.bridges[id]
	if !ok || b.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrBridgeNotFound
	}
	return b, nil
}

// ListBridges is scoped to the caller's tenant, with legal entity as an
// optional narrowing WITHIN that tenant.
func (m *MemoryStore) ListBridges(ctx context.Context, legalEntityID string) ([]domain.ApiBridge, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.ApiBridge, 0)
	for _, b := range m.bridges {
		if b.TenantID != tenantID {
			continue
		}
		if legalEntityID == "" || b.LegalEntityID == legalEntityID {
			res = append(res, *b)
		}
	}
	return res, nil
}

func (m *MemoryStore) RecordIngestion(ctx context.Context, log *domain.IngestionLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if log.LogID == "" {
		log.LogID = uuid.New().String()
	}
	log.IngestedAt = time.Now().UTC()
	log.TenantID = middleware.GetTenantID(ctx)

	m.logs[log.BridgeID] = append(m.logs[log.BridgeID], log)
	return nil
}

// ListIngestionLogs is scoped to the caller's tenant. Keyed by bridge_id
// alone, this returned another tenant's payload summaries and error
// messages — the contents flowing through their integration.
func (m *MemoryStore) ListIngestionLogs(ctx context.Context, bridgeID string) ([]domain.IngestionLog, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	logs, ok := m.logs[bridgeID]
	if !ok {
		return []domain.IngestionLog{}, nil
	}

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.IngestionLog, 0, len(logs))
	for _, l := range logs {
		if l.TenantID != tenantID {
			continue
		}
		res = append(res, *l)
	}
	return res, nil
}
