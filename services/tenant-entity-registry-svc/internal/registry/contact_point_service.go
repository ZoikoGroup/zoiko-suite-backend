package registry

// MDM-01 party contact points: CreateContactPoint, VerifyContactPoint, RetireContactPoint and ListPartyContactPoints.
//
// Each command is its own action (PARTY_CONTACT_CREATE, _VERIFY, _RETIRE, _READ); the read is separate because it
// returns the address itself. A contact point is proposed by one principal and verified by another: the store refuses
// the same principal and so does a table constraint. The address value is never logged.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) contactPointStore() (ContactPointStore, error) {
	cs, ok := s.store.(ContactPointStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return cs, nil
}

// CreateContactPoint proposes a contact point for a party of the caller's tenant.
func (s *Service) CreateContactPoint(ctx context.Context, partyID string, req domain.CreateContactPointRequest) (*ContactPointOutcome, error) {
	req, err := req.Normalized()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.contact", "create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	cs, err := s.contactPointStore()
	if err != nil {
		return nil, err
	}
	out, err := cs.CreateContactPoint(ctx, ContactPointCreateCommand{PartyID: partyID, Request: req, Actor: domain.PrincipalFromContext(ctx)})
	if err != nil {
		return nil, fmt.Errorf("store.CreateContactPoint: %w", err)
	}
	return out, nil
}

// VerifyContactPoint moves a PROPOSED contact point to VERIFIED, by a principal other than the one who proposed it.
func (s *Service) VerifyContactPoint(ctx context.Context, id string, req domain.VerifyContactPointRequest) (*ContactPointOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.contact", "verify"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	cs, err := s.contactPointStore()
	if err != nil {
		return nil, err
	}
	out, err := cs.VerifyContactPoint(ctx, id, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.VerifyContactPoint: %w", err)
	}
	return out, nil
}

// RetireContactPoint ends a contact point. Retiring is final.
func (s *Service) RetireContactPoint(ctx context.Context, id string, req domain.RetireContactPointRequest) (*ContactPointOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.contact", "retire"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	cs, err := s.contactPointStore()
	if err != nil {
		return nil, err
	}
	out, err := cs.RetireContactPoint(ctx, id, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.RetireContactPoint: %w", err)
	}
	return out, nil
}

// ListPartyContactPoints returns the party's contact points in force at q.AsOf (now when zero), filtered by channel and
// status. The caller needs PARTY_CONTACT_READ, because the rows carry the address.
func (s *Service) ListPartyContactPoints(ctx context.Context, partyID string, q domain.ContactPointQuery) ([]domain.ContactPointItem, error) {
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.contact", "read"); err != nil {
		return nil, err
	}
	cs, err := s.contactPointStore()
	if err != nil {
		return nil, err
	}
	if q.AsOf.IsZero() {
		q.AsOf = time.Now().UTC()
	}
	rows, err := cs.ListPartyContactPoints(ctx, partyID, q)
	if err != nil {
		return nil, fmt.Errorf("store.ListPartyContactPoints: %w", err)
	}
	items := make([]domain.ContactPointItem, 0, len(rows))
	for _, r := range rows {
		if r.InForce(q.AsOf) {
			items = append(items, r.Item())
		}
	}
	return items, nil
}
