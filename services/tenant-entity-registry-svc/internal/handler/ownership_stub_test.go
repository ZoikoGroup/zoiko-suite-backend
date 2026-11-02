package handler_test

// The ORG-04 half of stubSvc: every method returns s.err and records the
// identifier the handler took from the URL, as the rest of the stub does.

import (
	"context"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
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
