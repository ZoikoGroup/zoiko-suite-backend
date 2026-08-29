package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-transfer-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual fail-closed/expiry semantics, not canned
// responses.
type stubStore struct {
	relationships map[string]*domain.ProcessorRelationship
	subprocessors map[string][]domain.Subprocessor
	mechanisms    map[string]*domain.TransferMechanism
	assessments   map[string][]domain.TransferAssessment // by relationship_id, append order
	decisions     map[string]*domain.TransferDecision
}

func newStubStore() *stubStore {
	return &stubStore{
		relationships: map[string]*domain.ProcessorRelationship{},
		subprocessors: map[string][]domain.Subprocessor{},
		mechanisms:    map[string]*domain.TransferMechanism{},
		assessments:   map[string][]domain.TransferAssessment{},
		decisions:     map[string]*domain.TransferDecision{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) CreateRelationship(_ context.Context, tenantID string, req domain.CreateProcessorRelationshipRequest, principalID string) (*domain.ProcessorRelationship, error) {
	r := &domain.ProcessorRelationship{
		RelationshipID: uuid.New().String(), TenantID: strp(tenantID), ControllerRef: req.ControllerRef,
		ProcessorRef: req.ProcessorRef, Service: req.Service, ProcessingInstructions: req.ProcessingInstructions,
		PurposeActivityRefs: req.PurposeActivityRefs, DataCategories: req.DataCategories, SubjectClasses: req.SubjectClasses,
		ContractEvidenceRef: req.ContractEvidenceRef, Jurisdictions: req.Jurisdictions,
		Status: domain.RelationshipActive, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.relationships[r.RelationshipID] = r
	return r, nil
}

func (s *stubStore) FindRelationship(_ context.Context, relationshipID string) (*domain.ProcessorRelationship, error) {
	r, ok := s.relationships[relationshipID]
	if !ok {
		return nil, domain.ErrRelationshipNotFound
	}
	return r, nil
}

func (s *stubStore) ListRelationships(_ context.Context) ([]domain.ProcessorRelationship, error) {
	var out []domain.ProcessorRelationship
	for _, r := range s.relationships {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) UpdateRelationshipStatus(_ context.Context, relationshipID string, status domain.RelationshipStatus) (*domain.ProcessorRelationship, error) {
	r, ok := s.relationships[relationshipID]
	if !ok {
		return nil, domain.ErrRelationshipNotFound
	}
	r.Status = status
	return r, nil
}

func (s *stubStore) AttachSubprocessor(_ context.Context, relationshipID string, req domain.AttachSubprocessorRequest, principalID string) (*domain.Subprocessor, error) {
	r, ok := s.relationships[relationshipID]
	if !ok {
		return nil, domain.ErrRelationshipNotFound
	}
	sp := domain.Subprocessor{
		SubprocessorID: uuid.New().String(), TenantID: r.TenantID, RelationshipID: relationshipID,
		ProviderIdentity: req.ProviderIdentity, Service: req.Service, Purpose: req.Purpose, DataScope: req.DataScope,
		ProcessingLocations: req.ProcessingLocations, OnwardSubprocessors: req.OnwardSubprocessors,
		NotificationApprovalModel: req.NotificationApprovalModel, ContractEvidenceRef: req.ContractEvidenceRef,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.subprocessors[relationshipID] = append(s.subprocessors[relationshipID], sp)
	return &sp, nil
}

func (s *stubStore) ListSubprocessors(_ context.Context, relationshipID string) ([]domain.Subprocessor, error) {
	return s.subprocessors[relationshipID], nil
}

func (s *stubStore) CreateMechanism(_ context.Context, tenantID string, req domain.CreateTransferMechanismRequest, principalID string) (*domain.TransferMechanism, error) {
	validFrom := time.Now().UTC()
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	m := &domain.TransferMechanism{
		MechanismID: uuid.New().String(), TenantID: strp(tenantID), MechanismType: req.MechanismType,
		EvidenceRef: req.EvidenceRef, Conditions: req.Conditions, ValidFrom: validFrom, ValidUntil: req.ValidUntil,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.mechanisms[m.MechanismID] = m
	return m, nil
}

func (s *stubStore) FindMechanism(_ context.Context, mechanismID string) (*domain.TransferMechanism, error) {
	m, ok := s.mechanisms[mechanismID]
	if !ok {
		return nil, domain.ErrMechanismNotFound
	}
	return m, nil
}

func (s *stubStore) RecordAssessment(_ context.Context, tenantID string, req domain.RecordTransferAssessmentRequest, principalID string) (*domain.TransferAssessment, error) {
	a := domain.TransferAssessment{
		AssessmentID: uuid.New().String(), TenantID: strp(tenantID), RelationshipID: req.RelationshipID,
		Outcome: req.Outcome, ReviewerPrincipalID: principalID, ResidualRisk: req.ResidualRisk,
		EvidenceRef: req.EvidenceRef, ReviewTriggerAt: req.ReviewTriggerAt, CreatedAt: time.Now().UTC(),
	}
	s.assessments[req.RelationshipID] = append(s.assessments[req.RelationshipID], a)
	return &a, nil
}

func (s *stubStore) FindLatestAssessment(_ context.Context, relationshipID string) (*domain.TransferAssessment, error) {
	list := s.assessments[relationshipID]
	if len(list) == 0 {
		return nil, nil
	}
	latest := list[0]
	for _, a := range list[1:] {
		if a.CreatedAt.After(latest.CreatedAt) {
			latest = a
		}
	}
	return &latest, nil
}

func (s *stubStore) RecordDecision(_ context.Context, tenantID string, d *domain.TransferDecision) error {
	if d.DecisionID == "" {
		d.DecisionID = uuid.New().String()
	}
	d.DecidedAt = time.Now().UTC()
	if tenantID != "" {
		d.TenantID = strp(tenantID)
	}
	cp := *d
	s.decisions[d.DecisionID] = &cp
	return nil
}

func (s *stubStore) FindDecision(_ context.Context, decisionID string) (*domain.TransferDecision, error) {
	d, ok := s.decisions[decisionID]
	if !ok {
		return nil, domain.ErrDecisionNotFound
	}
	return d, nil
}
