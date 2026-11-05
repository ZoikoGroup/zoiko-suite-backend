package registry

// ORG-08 Establishment: CreateEstablishment, ChangeEstablishmentStatus and the reads GetEstablishment,
// ListEntityEstablishments and GetEstablishmentsAsOf (ZS-SVC-I-001 s4.8).
//
// A command on an establishment is authorized against the legal entity it belongs to. The service does not infer
// legal presence from an address: an establishment is recorded from evidence, and a material one is activated by a
// principal other than the one who proposed it (enforced by the store and by a table constraint).

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) establishmentStore() (EstablishmentStore, error) {
	es, ok := s.store.(EstablishmentStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return es, nil
}

// CreateEstablishment is CreateEstablishment.
func (s *Service) CreateEstablishment(ctx context.Context, req domain.CreateEstablishmentRequest) (*EstablishmentOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorizeEntity(ctx, req.LegalEntityID, "establishment", "create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if err := s.assertEntityOperational(ctx, req.LegalEntityID); err != nil {
		return nil, err
	}
	es, err := s.establishmentStore()
	if err != nil {
		return nil, err
	}
	out, err := es.CreateEstablishment(ctx, EstablishmentCreateCommand{Request: req, Actor: domain.PrincipalFromContext(ctx), Fingerprint: req.Fingerprint()})
	if err != nil {
		return nil, fmt.Errorf("store.CreateEstablishment: %w", err)
	}
	return out, nil
}

// ChangeEstablishmentStatus is ChangeEstablishmentStatus: to is ACTIVE, INACTIVE or CLOSED.
func (s *Service) ChangeEstablishmentStatus(ctx context.Context, establishmentID string, req domain.ChangeEstablishmentStatusRequest) (*EstablishmentOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	es, err := s.establishmentStore()
	if err != nil {
		return nil, err
	}
	entityID, err := es.EstablishmentEntity(ctx, establishmentID)
	if err != nil {
		return nil, fmt.Errorf("store.EstablishmentEntity: %w", err)
	}
	action := map[domain.EstablishmentStatus]string{domain.EstActive: "activate", domain.EstInactive: "deactivate", domain.EstClosed: "close"}[req.To]
	if err := s.authorizeEntity(ctx, entityID, "establishment", action); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	out, err := es.ChangeEstablishmentStatus(ctx, establishmentID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.ChangeEstablishmentStatus: %w", err)
	}
	return out, nil
}

// GetEstablishment is GetEstablishment: with the address version in force at asOf (now when zero).
func (s *Service) GetEstablishment(ctx context.Context, establishmentID string, asOf time.Time, history bool) (*domain.EstablishmentView, error) {
	es, err := s.establishmentStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	v, err := es.GetEstablishment(ctx, establishmentID, asOf, history)
	if err != nil {
		return nil, fmt.Errorf("store.GetEstablishment: %w", err)
	}
	if v == nil {
		return nil, ErrNotFound
	}
	return v, nil
}

// ListEntityEstablishments is ListEntityEstablishments, by current status.
func (s *Service) ListEntityEstablishments(ctx context.Context, legalEntityID string, status domain.EstablishmentStatus) ([]domain.Establishment, error) {
	if status != "" && !status.Valid() {
		return nil, fmt.Errorf("%w: status is not an establishment status", ErrInvalidInput)
	}
	es, err := s.establishmentStore()
	if err != nil {
		return nil, err
	}
	out, err := es.ListEntityEstablishments(ctx, legalEntityID, status)
	if err != nil {
		return nil, fmt.Errorf("store.ListEntityEstablishments: %w", err)
	}
	return out, nil
}

// GetEstablishmentsAsOf is GetEstablishmentsAsOf: the establishments active on the date.
func (s *Service) GetEstablishmentsAsOf(ctx context.Context, legalEntityID string, asOf time.Time) ([]domain.EstablishmentView, error) {
	es, err := s.establishmentStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := es.EstablishmentsAsOf(ctx, legalEntityID, asOf)
	if err != nil {
		return nil, fmt.Errorf("store.EstablishmentsAsOf: %w", err)
	}
	return out, nil
}

// ListEntityAddresses lists an entity's addresses: its registered office now and as it was.
func (s *Service) ListEntityAddresses(ctx context.Context, legalEntityID string, asOf time.Time, includeEnded bool) ([]domain.EntityAddress, error) {
	es, err := s.establishmentStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := es.ListEntityAddresses(ctx, legalEntityID, asOf, includeEnded)
	if err != nil {
		return nil, fmt.Errorf("store.ListEntityAddresses: %w", err)
	}
	return out, nil
}
