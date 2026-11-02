package registry

// ORG-04 Corporate Group & Ownership (ZS-SVC-I-001 s4.4): the named commands
// ProposeOwnershipRelationship and ApproveOwnershipRelationship (plus its
// refusal, RejectOwnershipRelationship).
//
// A relationship takes effect only when a principal other than its proposer
// approves it. The approver is the gateway-verified caller, never a body field,
// and the database enforces the same rule (or_no_self_approval).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// ProposeOwnershipRelationship files a PROPOSED ownership or control
// relationship between two operational legal entities of the caller's tenant.
func (s *Service) ProposeOwnershipRelationship(ctx context.Context, req domain.ProposeOwnershipRequest) (*domain.OwnershipRelationship, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	// Both sides change: the parent gains a subsidiary, the child an owner.
	if err := s.authorizeEntities(ctx, "entity.ownership", "propose", req.ChildLegalEntityID, req.ParentLegalEntityID); err != nil {
		return nil, err
	}
	// ORG s8 NP4: protected writes are refused while the tenant may not transact.
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	for _, id := range []string{req.ParentLegalEntityID, req.ChildLegalEntityID} {
		if err := s.assertEntityOperational(ctx, id); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	r := &domain.OwnershipRelationship{
		RelationshipID:        newID(),
		TenantID:              domain.TenantFromContext(ctx),
		ParentLegalEntityID:   req.ParentLegalEntityID,
		ChildLegalEntityID:    req.ChildLegalEntityID,
		RelationshipKind:      req.RelationshipKind,
		OwnershipPct:          req.OwnershipPct,
		VotingPct:             req.VotingPct,
		ControlType:           req.ControlType,
		BeneficialNotes:       nullableString(req.BeneficialNotes),
		ValidFrom:             req.ValidFrom.UTC(),
		ValidTo:               req.ValidTo,
		RecordedAt:            now,
		Status:                domain.OwnershipProposed,
		EvidenceRef:           req.EvidenceRef,
		Reason:                req.Reason,
		ProposedByPrincipalID: domain.PrincipalFromContext(ctx),
		ProposedAt:            now,
		CorrelationID:         req.CorrelationID,
		RecordVersion:         1,
	}
	if err := s.store.CreateOwnershipRelationship(ctx, r); err != nil {
		return nil, fmt.Errorf("store.CreateOwnershipRelationship: %w", err)
	}
	return r, nil
}

// ApproveOwnershipRelationship activates a PROPOSED relationship.
func (s *Service) ApproveOwnershipRelationship(ctx context.Context, relationshipID string, req domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error) {
	return s.decideOwnership(ctx, relationshipID, req, domain.OwnershipActive)
}

// RejectOwnershipRelationship refuses a PROPOSED relationship. It is a decision
// like approval and carries the same segregation of duties.
func (s *Service) RejectOwnershipRelationship(ctx context.Context, relationshipID string, req domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error) {
	return s.decideOwnership(ctx, relationshipID, req, domain.OwnershipRejected)
}

func (s *Service) decideOwnership(ctx context.Context, relationshipID string, req domain.OwnershipDecisionRequest, to domain.OwnershipStatus) (*domain.OwnershipRelationship, error) {
	rel, err := s.store.GetOwnershipRelationship(ctx, relationshipID)
	if err != nil {
		return nil, fmt.Errorf("store.GetOwnershipRelationship: %w", err)
	}
	if rel == nil {
		// Evaluate at the caller's own scope so an unauthorized caller learns
		// nothing about which ids exist, then answer not-found.
		if err := s.authorize(ctx, "entity.ownership", "approve"); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	if err := s.authorizeEntities(ctx, "entity.ownership", "approve", rel.ChildLegalEntityID, rel.ParentLegalEntityID); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}

	decider := domain.PrincipalFromContext(ctx)
	if decider == rel.ProposedByPrincipalID {
		return nil, fmt.Errorf("%w: the proposer of a relationship cannot decide it", ErrSelfApproval)
	}
	if rel.Status != domain.OwnershipProposed {
		return nil, fmt.Errorf("%w: the relationship is %s, only a PROPOSED relationship can be decided", ErrInvalidTransition, rel.Status)
	}
	version, err := s.checkExpected(req.ExpectedVersion, rel.RecordVersion, "ownership relationship")
	if err != nil {
		return nil, err
	}
	if to == domain.OwnershipActive {
		// An entity may have been merged or suspended since the proposal.
		for _, id := range []string{rel.ParentLegalEntityID, rel.ChildLegalEntityID} {
			if err := s.assertEntityOperational(ctx, id); err != nil {
				return nil, err
			}
		}
	}

	out, err := s.store.DecideOwnershipRelationship(ctx, relationshipID, version, to, decider, req.DecisionNote, req.CorrelationID)
	if err != nil {
		if errors.Is(err, ErrSelfApproval) {
			return nil, err
		}
		return nil, fmt.Errorf("store.DecideOwnershipRelationship: %w", err)
	}
	return out, nil
}

// GetOwnershipRelationship returns one relationship of the caller's tenant.
func (s *Service) GetOwnershipRelationship(ctx context.Context, relationshipID string) (*domain.OwnershipRelationship, error) {
	rel, err := s.store.GetOwnershipRelationship(ctx, relationshipID)
	if err != nil {
		return nil, fmt.Errorf("store.GetOwnershipRelationship: %w", err)
	}
	if rel == nil {
		return nil, ErrNotFound
	}
	return rel, nil
}
