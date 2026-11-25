package idempotency

import (
	"context"
	"sync"
)

// MemoryStore is an in-process Store with the same semantics as PgStore.
//
// It exists for unit tests only. It forgets every claim on restart, which is
// precisely the failure replay protection is meant to survive, so production
// wiring (cmd/server) always uses PgStore.
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]*Record
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{records: map[string]*Record{}} }

func (m *MemoryStore) Claim(_ context.Context, tenantID, endpoint, key, fingerprint string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := tenantID + "|" + endpoint + "|" + key
	if existing, ok := m.records[id]; ok {
		if existing.RequestFingerprint != fingerprint {
			return nil, ErrFingerprintMismatch
		}
		copied := *existing
		return &copied, nil
	}
	m.records[id] = &Record{RequestFingerprint: fingerprint}
	return nil, nil
}

func (m *MemoryStore) Complete(_ context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if record, ok := m.records[tenantID+"|"+endpoint+"|"+key]; ok {
		record.ResponseStatus = status
		record.ResponseBody = append([]byte(nil), body...)
	}
	return nil
}

func (m *MemoryStore) Release(_ context.Context, tenantID, endpoint, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := tenantID + "|" + endpoint + "|" + key
	if record, ok := m.records[id]; ok && record.ResponseStatus == 0 {
		delete(m.records, id)
	}
	return nil
}
