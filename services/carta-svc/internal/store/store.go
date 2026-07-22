package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"zoiko.io/carta-svc/internal/domain"
)

type Store interface {
	SaveAssessment(ctx context.Context, tenantID string, asm *domain.CartaAssessment) error
	GetAssessmentByID(ctx context.Context, tenantID, id string) (*domain.CartaAssessment, error)
	ListAssessments(ctx context.Context, tenantID, subjectID string) ([]domain.CartaAssessment, error)
}

type MemoryStore struct {
	mu          sync.RWMutex
	assessments map[string]*domain.CartaAssessment
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{assessments: make(map[string]*domain.CartaAssessment)}
}

func (m *MemoryStore) SaveAssessment(ctx context.Context, tenantID string, asm *domain.CartaAssessment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	asm.ID = uuid.New().String()
	asm.TenantID = tenantID
	m.assessments[asm.ID] = asm
	return nil
}

func (m *MemoryStore) GetAssessmentByID(ctx context.Context, tenantID, id string) (*domain.CartaAssessment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	asm, ok := m.assessments[id]
	if !ok || asm.TenantID != tenantID {
		return nil, fmt.Errorf("assessment not found")
	}
	return asm, nil
}

func (m *MemoryStore) ListAssessments(ctx context.Context, tenantID, subjectID string) ([]domain.CartaAssessment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.CartaAssessment
	for _, a := range m.assessments {
		if a.TenantID != tenantID {
			continue
		}
		if subjectID != "" && a.SubjectID != subjectID {
			continue
		}
		result = append(result, *a)
	}
	return result, nil
}
