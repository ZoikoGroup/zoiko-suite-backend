package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/siem-integration-svc/internal/domain"
)

type Store interface {
	CreateExporter(ctx context.Context, tenantID string, exp *domain.SIEMExporter) error
	GetExporterByID(ctx context.Context, tenantID, id string) (*domain.SIEMExporter, error)
	ListExporters(ctx context.Context, tenantID, legalEntityID string) ([]domain.SIEMExporter, error)
	StreamEvent(ctx context.Context, tenantID string, evt *domain.SIEMEvent) error
	ListEvents(ctx context.Context, tenantID, exporterID string) ([]domain.SIEMEvent, error)
	// Ping reports whether the store can serve requests; /readyz calls it.
	Ping(ctx context.Context) error
}

// ErrNotFound is an exporter that does not exist in the caller's tenant; one
// in another tenant is indistinguishable from one that does not exist.
var ErrNotFound = errors.New("exporter not found")

// ErrCredentialSealingUnavailable refuses to persist an auth_token when no
// credential key is configured: the token is stored sealed or not at all.
var ErrCredentialSealingUnavailable = errors.New("credential sealing key not configured")

// MemoryStore is the in-process Store used by unit tests only. cmd/server
// wires PgStore: exporters and security events must survive a restart.

type MemoryStore struct {
	mu        sync.RWMutex
	exporters map[string]*domain.SIEMExporter
	events    map[string]*domain.SIEMEvent
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		exporters: make(map[string]*domain.SIEMExporter),
		events:    make(map[string]*domain.SIEMEvent),
	}
}

func (m *MemoryStore) CreateExporter(ctx context.Context, tenantID string, exp *domain.SIEMExporter) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp.ID = uuid.New().String()
	exp.TenantID = tenantID
	exp.Status = domain.ExporterActive
	exp.CreatedAt = time.Now()
	exp.UpdatedAt = time.Now()
	m.exporters[exp.ID] = exp
	return nil
}

func (m *MemoryStore) GetExporterByID(ctx context.Context, tenantID, id string) (*domain.SIEMExporter, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	exp, ok := m.exporters[id]
	if !ok || exp.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return exp, nil
}

func (m *MemoryStore) ListExporters(ctx context.Context, tenantID, legalEntityID string) ([]domain.SIEMExporter, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.SIEMExporter
	for _, e := range m.exporters {
		if e.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && e.LegalEntityID != legalEntityID {
			continue
		}
		result = append(result, *e)
	}
	return result, nil
}

func (m *MemoryStore) StreamEvent(ctx context.Context, tenantID string, evt *domain.SIEMEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.exporters[evt.ExporterID]
	if !ok || exp.TenantID != tenantID {
		return ErrNotFound
	}
	now := time.Now()
	evt.ID = uuid.New().String()
	evt.TenantID = tenantID
	evt.Status = "DELIVERED"
	evt.Timestamp = now
	m.events[evt.ID] = evt

	exp.EventsSent++
	exp.LastStreamed = &now
	exp.UpdatedAt = now
	return nil
}

func (m *MemoryStore) ListEvents(ctx context.Context, tenantID, exporterID string) ([]domain.SIEMEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.SIEMEvent
	for _, evt := range m.events {
		if evt.TenantID != tenantID {
			continue
		}
		if exporterID != "" && evt.ExporterID != exporterID {
			continue
		}
		result = append(result, *evt)
	}
	return result, nil
}

// Ping always succeeds: the map is in process.
func (m *MemoryStore) Ping(context.Context) error { return nil }
