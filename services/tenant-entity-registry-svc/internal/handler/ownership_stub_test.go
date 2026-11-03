package handler_test

// The ORG-04 half of stubSvc: every method returns s.err and records the
// identifier the handler took from the URL, as the rest of the stub does.

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

func (s *stubSvc) ProposeOwnershipRelationship(_ context.Context, req domain.ProposeOwnershipRequest) (*domain.OwnershipRelationship, error) {
	s.gotCorrID = req.CorrelationID
	return &domain.OwnershipRelationship{RelationshipID: relID, Status: domain.OwnershipProposed}, s.err
}

func (s *stubSvc) ApproveOwnershipRelationship(_ context.Context, id string, req domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error) {
	s.gotID, s.gotCorrID = id, req.CorrelationID
	return &domain.OwnershipRelationship{RelationshipID: id, Status: domain.OwnershipActive}, s.err
}

func (s *stubSvc) RejectOwnershipRelationship(_ context.Context, id string, req domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error) {
	s.gotID, s.gotCorrID = id, req.CorrelationID
	return &domain.OwnershipRelationship{RelationshipID: id, Status: domain.OwnershipRejected}, s.err
}

func (s *stubSvc) GetOwnershipRelationship(_ context.Context, id string) (*domain.OwnershipRelationship, error) {
	s.gotID = id
	return &domain.OwnershipRelationship{RelationshipID: id}, s.err
}

func (s *stubSvc) SupersedeOwnershipRelationship(_ context.Context, id string, req domain.SupersedeOwnershipRequest) (*domain.OwnershipRelationship, error) {
	s.gotID, s.gotCorrID = id, req.CorrelationID
	return &domain.OwnershipRelationship{RelationshipID: relID, Status: domain.OwnershipProposed}, s.err
}

func (s *stubSvc) EndOwnershipRelationship(_ context.Context, id string, req domain.EndOwnershipRequest) (*domain.OwnershipRelationship, error) {
	s.gotID, s.gotCorrID = id, req.CorrelationID
	return &domain.OwnershipRelationship{RelationshipID: id, Status: domain.OwnershipEnded}, s.err
}

func (s *stubSvc) GetOwnershipGraph(_ context.Context, asOf time.Time, root string) (*domain.OwnershipGraphView, error) {
	s.gotAsOf, s.gotID = asOf, root
	return &domain.OwnershipGraphView{AsOf: asOf, Entities: []string{}, Relationships: []domain.OwnershipRelationship{}}, s.err
}

func (s *stubSvc) GetGroupAsOf(_ context.Context, id string, asOf time.Time) (*domain.GroupView, error) {
	s.gotID, s.gotAsOf = id, asOf
	return &domain.GroupView{LegalEntityID: id, AsOf: asOf}, s.err
}

func (s *stubSvc) ListEntityParents(_ context.Context, id string, asOf time.Time) (*registry.EntityParents, error) {
	s.gotID, s.gotAsOf = id, asOf
	return &registry.EntityParents{LegalEntityID: id, AsOf: asOf, Parents: []domain.OwnershipRelationship{}}, s.err
}

func (s *stubSvc) ExplainControlChain(_ context.Context, id string, asOf time.Time) (*domain.ControlChainView, error) {
	s.gotID, s.gotAsOf = id, asOf
	return &domain.ControlChainView{LegalEntityID: id, AsOf: asOf, Chains: []domain.UltimateParentChain{}}, s.err
}
