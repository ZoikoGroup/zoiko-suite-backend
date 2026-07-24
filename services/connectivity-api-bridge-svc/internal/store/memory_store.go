package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
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

func (m *MemoryStore) CreateBridge(ctx context.Context, b *domain.ApiBridge) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if b.BridgeID == "" {
		b.BridgeID = uuid.New().String()
	}
	now := time.Now().UTC()
	b.CreatedAt = now
	b.UpdatedAt = now

	m.bridges[b.BridgeID] = b
	return nil
}

func (m *MemoryStore) GetBridgeByID(ctx context.Context, id string) (*domain.ApiBridge, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	b, ok := m.bridges[id]
	if !ok {
		return nil, domain.ErrBridgeNotFound
	}
	return b, nil
}

func (m *MemoryStore) ListBridges(ctx context.Context, legalEntityID string) ([]domain.ApiBridge, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	res := make([]domain.ApiBridge, 0)
	for _, b := range m.bridges {
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

	m.logs[log.BridgeID] = append(m.logs[log.BridgeID], log)
	return nil
}

func (m *MemoryStore) ListIngestionLogs(ctx context.Context, bridgeID string) ([]domain.IngestionLog, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	logs, ok := m.logs[bridgeID]
	if !ok {
		return []domain.IngestionLog{}, nil
	}

	res := make([]domain.IngestionLog, len(logs))
	for i, l := range logs {
		res[i] = *l
	}
	return res, nil
}
