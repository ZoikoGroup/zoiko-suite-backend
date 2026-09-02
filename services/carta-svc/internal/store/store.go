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
	// legalEntityID is MANDATORY: it is the authorization scope for a
	// listing. subjectID stays optional and narrows WITHIN that scope.
	ListAssessments(ctx context.Context, tenantID, legalEntityID, subjectID string) ([]domain.CartaAssessment, error)
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

func (m *MemoryStore) ListAssessments(ctx context.Context, tenantID, legalEntityID, subjectID string) ([]domain.CartaAssessment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.CartaAssessment
	for _, a := range m.assessments {
		if a.TenantID != tenantID {
			continue
		}
		// Tenant AND legal entity are both mandatory; subject narrows within
		// them. Written as a plain equality rather than an
		// "empty means match everything" branch on purpose — that shape is
		// the self-disabling filter found across the six connector services,
		// and here legalEntityID is also the authorization scope, so a
		// self-disabling version would disable the scope itself.
		if a.LegalEntityID != legalEntityID {
			continue
		}
		if subjectID != "" && a.SubjectID != subjectID {
			continue
		}
		result = append(result, *a)
	}
	return result, nil
}
