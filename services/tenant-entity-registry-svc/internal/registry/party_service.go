package registry

// ORG-07 Party / Counterparty and MDM-01 identity creation (ZS-SVC-I-001 s4.7, ZS-SVC-U-001 s10.1):
// CreateParty, AmendPartyProfile (facts:propose), AddPartyRole, RemovePartyRole, LinkExternalIdentifier and the
// reads GetPartyAsOf, SearchParties and FindByExternalIdentifier.
//
// The create flow is s10.1 in order: authorize against the verified tenant (1), normalise without touching the
// source (2), exact-identifier and candidate search (3), return the existing party on a safe unique match (4),
// open a case and answer REVIEW_REQUIRED on ambiguity (5), reserve the id and persist sourced facts (6), and emit
// party.created through the outbox (7). Steps 2 to 7 run in the store, in one transaction under a per-tenant lock,
// because "never creates a duplicate" is a property of the whole sequence, not of any step.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) partyStore() (PartyStore, error) {
	ps, ok := s.store.(PartyStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return ps, nil
}

// CreateParty runs the create flow.
func (s *Service) CreateParty(ctx context.Context, req domain.CreatePartyRequest) (*domain.CreatePartyResult, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party", "create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if req.LegalEntityID != "" {
		if err := s.assertEntityOperational(ctx, req.LegalEntityID); err != nil {
			return nil, err
		}
	}
	ps, err := s.partyStore()
	if err != nil {
		return nil, err
	}
	res, err := ps.CreateParty(ctx, PartyCreateCommand{
		Request: req, Actor: domain.PrincipalFromContext(ctx), Fingerprint: req.Fingerprint(),
	})
	if err != nil {
		return nil, fmt.Errorf("store.CreateParty: %w", err)
	}
	return res, nil
}

// GetParty is GetParty and GetPartyAsOf: the master view in force at asOf as known at knownAt. A zero knownAt
// is now. Revealing sensitive identifiers is its own permission and is refused, not silently masked, when the
// caller asked for it without holding it.
func (s *Service) GetParty(ctx context.Context, partyID string, asOf, knownAt time.Time, reveal bool) (*domain.PartyView, error) {
	if reveal {
		if err := s.authorize(ctx, "party.identifier", "reveal"); err != nil {
			return nil, err
		}
	}
	ps, err := s.partyStore()
	if err != nil {
		return nil, err
	}
	if knownAt.IsZero() {
		knownAt = time.Now().UTC()
	}
	v, err := ps.GetPartyView(ctx, partyID, asOf, knownAt, reveal)
	if err != nil {
		return nil, fmt.Errorf("store.GetPartyView: %w", err)
	}
	if v == nil {
		return nil, ErrNotFound
	}
	return v, nil
}

// ProposePartyFact is AmendPartyProfile: it records a sourced, effective-dated fact. It never overwrites a verified
// fact; one that contradicts a verified fact is held beside it and opens a stewardship case.
func (s *Service) ProposePartyFact(ctx context.Context, partyID string, req domain.ProposeFactRequest) (*PartyFactOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party", "amend"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	ps, err := s.partyStore()
	if err != nil {
		return nil, err
	}
	out, err := ps.ProposePartyFact(ctx, partyID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.ProposePartyFact: %w", err)
	}
	return out, nil
}

// AddPartyRole gives a party an explicit, evidenced, time-aware role. A role never redefines identity.
func (s *Service) AddPartyRole(ctx context.Context, partyID string, req domain.AddPartyRoleRequest) (*PartyRoleOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.role", "manage"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	ps, err := s.partyStore()
	if err != nil {
		return nil, err
	}
	out, err := ps.AddPartyRole(ctx, partyID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.AddPartyRole: %w", err)
	}
	return out, nil
}

// RemovePartyRole ends a role. The row stays as history.
func (s *Service) RemovePartyRole(ctx context.Context, partyID, roleID string, req domain.RemovePartyRoleRequest) (*PartyRoleOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.role", "manage"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	ps, err := s.partyStore()
	if err != nil {
		return nil, err
	}
	out, err := ps.RemovePartyRole(ctx, partyID, roleID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.RemovePartyRole: %w", err)
	}
	return out, nil
}

// LinkPartyIdentifier is LinkExternalIdentifier. An identifier that already names another party is not linked: it
// opens a stewardship case instead.
func (s *Service) LinkPartyIdentifier(ctx context.Context, partyID string, req domain.LinkIdentifierRequest) (*PartyIdentifierOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.identifier", "link"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	ps, err := s.partyStore()
	if err != nil {
		return nil, err
	}
	out, err := ps.LinkPartyIdentifier(ctx, partyID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.LinkPartyIdentifier: %w", err)
	}
	if out.Identifier != nil && domain.IdentifierScheme(out.Identifier.Scheme).Sensitive() {
		out.Identifier.Value = domain.MaskIdentifier(out.Identifier.Scheme, out.Identifier.Value)
		out.Identifier.Masked = true
	}
	return out, nil
}

// FindPartiesByIdentifier is FindByExternalIdentifier.
func (s *Service) FindPartiesByIdentifier(ctx context.Context, scheme domain.IdentifierScheme, scope, value string) ([]domain.PartySummary, error) {
	in := domain.PartyIdentifierInput{Scheme: scheme, Scope: scope, Value: value}
	if err := in.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	ps, err := s.partyStore()
	if err != nil {
		return nil, err
	}
	out, err := ps.FindPartiesByIdentifier(ctx, scheme, scope, value)
	if err != nil {
		return nil, fmt.Errorf("store.FindPartiesByIdentifier: %w", err)
	}
	return out, nil
}

// SearchParties is SearchParties. The search reads the authoritative tables; a search index is a projection and
// can lag, so it is not what answers a question about who a party is.
func (s *Service) SearchParties(ctx context.Context, q domain.PartySearchQuery) ([]domain.PartySummary, string, error) {
	if q.Kind != "" && !q.Kind.Valid() {
		return nil, "", fmt.Errorf("%w: kind is not a party kind", ErrInvalidInput)
	}
	if q.Role != "" && !q.Role.Valid() {
		return nil, "", fmt.Errorf("%w: role is not a party role", ErrInvalidInput)
	}
	switch q.Status {
	case "", domain.PartyCandidate, domain.PartyActive, domain.PartyInactive:
	default:
		return nil, "", fmt.Errorf("%w: status is not a party status", ErrInvalidInput)
	}
	ps, err := s.partyStore()
	if err != nil {
		return nil, "", err
	}
	out, next, err := ps.SearchParties(ctx, q)
	if err != nil {
		return nil, "", fmt.Errorf("store.SearchParties: %w", err)
	}
	return out, next, nil
}

// GetResolutionCase reads one stewardship case of the caller's tenant.
func (s *Service) GetResolutionCase(ctx context.Context, caseID string) (*domain.ResolutionCase, error) {
	ps, err := s.partyStore()
	if err != nil {
		return nil, err
	}
	c, err := ps.GetResolutionCase(ctx, caseID)
	if err != nil {
		return nil, fmt.Errorf("store.GetResolutionCase: %w", err)
	}
	if c == nil {
		return nil, ErrNotFound
	}
	return c, nil
}
