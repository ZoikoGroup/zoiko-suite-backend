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
	"zoiko.io/tenant-entity-registry-svc/internal/events"
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

	r := s.newOwnershipRow(ctx, req)
	ctx = WithEvent(ctx, events.OwnershipRecord)
	if err := s.store.CreateOwnershipRelationship(ctx, r); err != nil {
		return nil, fmt.Errorf("store.CreateOwnershipRelationship: %w", err)
	}
	return r, nil
}

// newOwnershipRow builds the PROPOSED row for a validated request, stamped with
// the verified tenant and caller.
func (s *Service) newOwnershipRow(ctx context.Context, req domain.ProposeOwnershipRequest) *domain.OwnershipRelationship {
	now := time.Now().UTC()
	return &domain.OwnershipRelationship{
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

	ctx = WithEvent(ctx, events.OwnershipRecord)
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
		// Evaluate at the caller's own scope so an unauthorized caller learns
		// nothing about which ids exist.
		if err := s.authorize(ctx, "entity.ownership", "view"); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	if err := s.authorizeEntities(ctx, "entity.ownership", "view", rel.ChildLegalEntityID, rel.ParentLegalEntityID); err != nil {
		return nil, err
	}
	sensitive, err := s.canViewSensitive(ctx, rel.ChildLegalEntityID)
	if err != nil {
		return nil, err
	}
	if !sensitive {
		rel.BeneficialNotes = nil
	}
	return rel, nil
}

// ErrOwnershipConflict — the relationship contradicts the active group
// structure: it would close a cycle, overlap an identical relationship, or take
// an entity's owners above 100%. Wraps ErrConflict so it is a 409; the wrapped
// domain error names which rule.
var ErrOwnershipConflict error = &kindError{"ownership conflict", ErrConflict}

// SupersedeOwnershipRelationship proposes new terms for an ACTIVE relationship.
//
// It files a PROPOSED replacement that names the relationship it replaces. Until
// a different principal approves it nothing changes; on approval the replacement
// becomes ACTIVE and the original SUPERSEDED, with its valid_to cut to the
// replacement's valid_from. The original stays retrievable.
func (s *Service) SupersedeOwnershipRelationship(ctx context.Context, relationshipID string, req domain.SupersedeOwnershipRequest) (*domain.OwnershipRelationship, error) {
	old, err := s.store.GetOwnershipRelationship(ctx, relationshipID)
	if err != nil {
		return nil, fmt.Errorf("store.GetOwnershipRelationship: %w", err)
	}
	if old == nil {
		if err := s.authorize(ctx, "entity.ownership", "supersede"); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	if err := s.authorizeEntities(ctx, "entity.ownership", "supersede", old.ChildLegalEntityID, old.ParentLegalEntityID); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if old.Status != domain.OwnershipActive {
		return nil, fmt.Errorf("%w: only an ACTIVE relationship can be superseded; this one is %s", ErrInvalidTransition, old.Status)
	}
	if _, err := s.checkExpected(req.ExpectedVersion, old.RecordVersion, "ownership relationship"); err != nil {
		return nil, err
	}
	proposal := req.AsProposal(*old)
	if err := proposal.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if !req.ValidFrom.After(old.ValidFrom) {
		return nil, fmt.Errorf("%w: a supersession must take effect after the relationship it replaces starts (%s)",
			ErrInvalidInput, old.ValidFrom.UTC().Format("2006-01-02"))
	}
	for _, id := range []string{old.ParentLegalEntityID, old.ChildLegalEntityID} {
		if err := s.assertEntityOperational(ctx, id); err != nil {
			return nil, err
		}
	}

	r := s.newOwnershipRow(ctx, proposal)
	r.SupersedesRelationshipID = &old.RelationshipID
	ctx = WithEvent(ctx, events.OwnershipRecord)
	if err := s.store.CreateOwnershipRelationship(ctx, r); err != nil {
		return nil, fmt.Errorf("store.CreateOwnershipRelationship: %w", err)
	}
	return r, nil
}

// EndOwnershipRelationship records that an ACTIVE relationship stopped.
//
// It takes effect at once, so it is a HIGH-tier action of its own
// (ENTITY_OWNERSHIP_END) and requires the evidence and reason that a start does.
func (s *Service) EndOwnershipRelationship(ctx context.Context, relationshipID string, req domain.EndOwnershipRequest) (*domain.OwnershipRelationship, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	rel, err := s.store.GetOwnershipRelationship(ctx, relationshipID)
	if err != nil {
		return nil, fmt.Errorf("store.GetOwnershipRelationship: %w", err)
	}
	if rel == nil {
		if err := s.authorize(ctx, "entity.ownership", "end"); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	if err := s.authorizeEntities(ctx, "entity.ownership", "end", rel.ChildLegalEntityID, rel.ParentLegalEntityID); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	version, err := s.checkExpected(req.ExpectedVersion, rel.RecordVersion, "ownership relationship")
	if err != nil {
		return nil, err
	}
	ctx = WithEvent(ctx, events.OwnershipRecord)
	out, err := s.store.EndOwnershipRelationship(ctx, relationshipID, version, req.EndDate.UTC(),
		domain.PrincipalFromContext(ctx), req.Reason, req.EvidenceRef, req.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("store.EndOwnershipRelationship: %w", err)
	}
	return out, nil
}

// ── ORG-04 read surfaces ────────────────────────────────────────────────────
//
// Reads need ENTITY_OWNERSHIP_VIEW. Beneficial and control notes need the
// separate ENTITY_OWNERSHIP_VIEW_SENSITIVE (s4.4 "sensitive beneficial/control
// information scoped"): a caller without it still gets the structure, with the
// notes removed, rather than an error, so the graph stays usable for the many
// callers who need the shape but not the people behind it.

// canViewSensitive decides whether notes may be returned. entityID "" asks at
// the tenant scope. A denial is not an error: it means "mask". An outage is an
// error, because masking is a security decision and must fail closed.
func (s *Service) canViewSensitive(ctx context.Context, entityID string) (bool, error) {
	var err error
	if entityID == "" {
		err = s.authorize(ctx, "entity.ownership", "view_sensitive")
	} else {
		err = s.authorizeEntity(ctx, entityID, "entity.ownership", "view_sensitive")
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrUnauthorized):
		return false, nil
	}
	return false, err
}

// maskNotes removes beneficial notes from every relationship, in place.
func maskNotes(rels []domain.OwnershipRelationship) {
	for i := range rels {
		rels[i].BeneficialNotes = nil
	}
}

// asOfOrNow treats the zero time as "now".
func asOfOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

// readGraph loads the relationships in force at asOf, authorized at one entity
// (or at the tenant when entityID is "").
func (s *Service) readGraph(ctx context.Context, entityID string, asOf time.Time) ([]domain.OwnershipRelationship, bool, error) {
	var err error
	if entityID == "" {
		err = s.authorize(ctx, "entity.ownership", "view")
	} else {
		err = s.authorizeEntity(ctx, entityID, "entity.ownership", "view")
	}
	if err != nil {
		return nil, false, err
	}
	if entityID != "" {
		if _, err := s.GetEntity(ctx, entityID); err != nil {
			return nil, false, err
		}
	}
	sensitive, err := s.canViewSensitive(ctx, entityID)
	if err != nil {
		return nil, false, err
	}
	rels, err := s.store.ListOwnershipInForceAt(ctx, asOf)
	if err != nil {
		return nil, false, fmt.Errorf("store.ListOwnershipInForceAt: %w", err)
	}
	return rels, sensitive, nil
}

// GetOwnershipGraph returns the group structure in force at asOf (now when
// zero), optionally limited to what root owns or controls.
func (s *Service) GetOwnershipGraph(ctx context.Context, asOf time.Time, root string) (*domain.OwnershipGraphView, error) {
	asOf = asOfOrNow(asOf)
	rels, sensitive, err := s.readGraph(ctx, root, asOf)
	if err != nil {
		return nil, err
	}
	view := domain.GraphAsOf(rels, asOf, root)
	if !sensitive {
		maskNotes(view.Relationships)
	}
	return &view, nil
}

// GetGroupAsOf returns the corporate group the entity belonged to at asOf.
func (s *Service) GetGroupAsOf(ctx context.Context, legalEntityID string, asOf time.Time) (*domain.GroupView, error) {
	asOf = asOfOrNow(asOf)
	rels, sensitive, err := s.readGraph(ctx, legalEntityID, asOf)
	if err != nil {
		return nil, err
	}
	view := domain.GroupAsOf(rels, asOf, legalEntityID)
	if !sensitive {
		maskNotes(view.Relationships)
	}
	return &view, nil
}

// EntityParents is the answer to ListEntityParents.
type EntityParents struct {
	AsOf          time.Time                      `json:"as_of"`
	LegalEntityID string                         `json:"legal_entity_id"`
	Parents       []domain.OwnershipRelationship `json:"parents"`
}

// ListEntityParents returns the entity's direct owners and controllers at asOf.
func (s *Service) ListEntityParents(ctx context.Context, legalEntityID string, asOf time.Time) (*EntityParents, error) {
	asOf = asOfOrNow(asOf)
	rels, sensitive, err := s.readGraph(ctx, legalEntityID, asOf)
	if err != nil {
		return nil, err
	}
	out := &EntityParents{AsOf: asOf, LegalEntityID: legalEntityID, Parents: domain.ParentsAsOf(rels, asOf, legalEntityID)}
	if !sensitive {
		maskNotes(out.Parents)
	}
	return out, nil
}

// ExplainControlChain explains who owns and controls the entity at asOf, with
// every path, its effective ownership and whether it amounts to control.
func (s *Service) ExplainControlChain(ctx context.Context, legalEntityID string, asOf time.Time) (*domain.ControlChainView, error) {
	asOf = asOfOrNow(asOf)
	rels, _, err := s.readGraph(ctx, legalEntityID, asOf)
	if err != nil {
		return nil, err
	}
	view := domain.ExplainControlChain(rels, asOf, legalEntityID)
	return &view, nil
}
