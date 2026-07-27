package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
)

type MemoryStore struct {
	mu          sync.RWMutex
	interfaces  map[string]*domain.TaxInterface
	submissions map[string]*domain.TaxFilingSubmission
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		interfaces:  make(map[string]*domain.TaxInterface),
		submissions: make(map[string]*domain.TaxFilingSubmission),
	}
}

func (m *MemoryStore) CreateInterface(ctx context.Context, tf *domain.TaxInterface) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if tf.InterfaceID == "" {
		tf.InterfaceID = uuid.New().String()
	}
	now := time.Now().UTC()
	tf.CreatedAt = now
	tf.UpdatedAt = now

	m.interfaces[tf.InterfaceID] = tf
	return nil
}

func (m *MemoryStore) GetInterfaceByID(ctx context.Context, id string) (*domain.TaxInterface, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tf, ok := m.interfaces[id]
	if !ok {
		return nil, domain.ErrInterfaceNotFound
	}
	return tf, nil
}

func (m *MemoryStore) ListInterfaces(ctx context.Context, legalEntityID string) ([]domain.TaxInterface, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	res := make([]domain.TaxInterface, 0)
	for _, tf := range m.interfaces {
		if legalEntityID == "" || tf.LegalEntityID == legalEntityID {
			res = append(res, *tf)
		}
	}
	return res, nil
}

func (m *MemoryStore) CreateSubmission(ctx context.Context, sub *domain.TaxFilingSubmission) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sub.SubmissionID == "" {
		sub.SubmissionID = uuid.New().String()
	}
	sub.SubmittedAt = time.Now().UTC()

	m.submissions[sub.SubmissionID] = sub
	return nil
}

func (m *MemoryStore) GetSubmissionByID(ctx context.Context, id string) (*domain.TaxFilingSubmission, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sub, ok := m.submissions[id]
	if !ok {
		return nil, domain.ErrFilingNotFound
	}
	return sub, nil
}

func (m *MemoryStore) ListSubmissions(ctx context.Context, interfaceID string) ([]domain.TaxFilingSubmission, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	res := make([]domain.TaxFilingSubmission, 0)
	for _, sub := range m.submissions {
		if interfaceID == "" || sub.InterfaceID == interfaceID {
			res = append(res, *sub)
		}
	}
	return res, nil
}
