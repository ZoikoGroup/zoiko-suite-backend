package registry

// ORG-08 Address: CreateAddress, VerifyAddress, AmendAddress, LinkAddressToParty and the reads GetAddress,
// SearchAddresses and ListPartyAddresses (ZS-SVC-I-001 s4.8, ZS-SVC-U-001 s14.2).
//
// Three rules shape it. The structured source is kept and the normalized form only aids matching. A verification
// is an observation about one version and never changes the address. And a REGISTERED address is a legal
// assertion: it is recorded only from authority evidence, never upgraded from a postal check.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) addressStore() (AddressStore, error) {
	as, ok := s.store.(AddressStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return as, nil
}

// addressVerifier resolves the verifier a caller named. EXTERNAL_PROVIDER is a known verifier that is BLOCKED: it
// answers ErrServiceUnavailable instead of falling back to the local rules and implying a provider checked it.
func (s *Service) addressVerifier(name string) (AddressVerifier, error) {
	switch name {
	case "", domain.VerifierLocalRules:
		return LocalAddressVerifier{}, nil
	case domain.VerifierExternal:
		return nil, fmt.Errorf("%w: external address verification is blocked until the external data feed service is available", ErrServiceUnavailable)
	}
	return nil, fmt.Errorf("%w: verifier must be %s", ErrInvalidInput, domain.VerifierLocalRules)
}

func addressInvalid(err error) error {
	if errors.Is(err, domain.ErrRegisteredNeedsAuthority) {
		return fmt.Errorf("%w: %v", ErrSourceUnverified, err)
	}
	return fmt.Errorf("%w: %v", ErrInvalidInput, err)
}

// CreateAddress is CreateAddress.
func (s *Service) CreateAddress(ctx context.Context, req domain.CreateAddressRequest) (*AddressOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, addressInvalid(err)
	}
	if err := s.authorize(ctx, "address", "create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	as, err := s.addressStore()
	if err != nil {
		return nil, err
	}
	out, err := as.CreateAddress(ctx, AddressCreateCommand{Request: req, Actor: domain.PrincipalFromContext(ctx), Fingerprint: req.Fingerprint()})
	if err != nil {
		return nil, fmt.Errorf("store.CreateAddress: %w", err)
	}
	return out, nil
}

// AmendAddress is AmendAddress.
func (s *Service) AmendAddress(ctx context.Context, addressID string, req domain.AmendAddressRequest) (*AddressOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, addressInvalid(err)
	}
	if err := s.authorize(ctx, "address", "amend"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	as, err := s.addressStore()
	if err != nil {
		return nil, err
	}
	out, err := as.AmendAddress(ctx, addressID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.AmendAddress: %w", err)
	}
	return out, nil
}

// VerifyAddress is VerifyAddress.
func (s *Service) VerifyAddress(ctx context.Context, addressID string, req domain.VerifyAddressRequest) (*VerifyOutcome, error) {
	v, err := s.addressVerifier(req.Verifier)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, "address", "verify"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	as, err := s.addressStore()
	if err != nil {
		return nil, err
	}
	out, err := as.VerifyAddress(ctx, addressID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional, v.Verify)
	if err != nil {
		return nil, fmt.Errorf("store.VerifyAddress: %w", err)
	}
	return out, nil
}

// LinkAddressToParty is LinkAddressToParty.
func (s *Service) LinkAddressToParty(ctx context.Context, partyID string, req domain.LinkAddressRequest) (*AddressLinkOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, addressInvalid(err)
	}
	if err := s.authorize(ctx, "address", "link"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	as, err := s.addressStore()
	if err != nil {
		return nil, err
	}
	out, err := as.LinkAddressToParty(ctx, partyID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.LinkAddressToParty: %w", err)
	}
	return out, nil
}

// GetAddress is GetAddress: the version in force at asOf (now when zero).
func (s *Service) GetAddress(ctx context.Context, addressID string, asOf time.Time, history bool) (*domain.AddressView, error) {
	as, err := s.addressStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	v, err := as.GetAddress(ctx, addressID, asOf, history)
	if err != nil {
		return nil, fmt.Errorf("store.GetAddress: %w", err)
	}
	if v == nil {
		return nil, ErrNotFound
	}
	return v, nil
}

// SearchAddresses is SearchAddresses, over the normalized comparison form.
func (s *Service) SearchAddresses(ctx context.Context, q domain.AddressSearchQuery) ([]domain.AddressSummary, string, error) {
	if q.Type != "" && !q.Type.Valid() {
		return nil, "", fmt.Errorf("%w: type is not an address type", ErrInvalidInput)
	}
	if q.Country != "" && len(q.Country) != 2 {
		return nil, "", fmt.Errorf("%w: country must be a two-letter code", ErrInvalidInput)
	}
	as, err := s.addressStore()
	if err != nil {
		return nil, "", err
	}
	out, next, err := as.SearchAddresses(ctx, q)
	if err != nil {
		return nil, "", fmt.Errorf("store.SearchAddresses: %w", err)
	}
	return out, next, nil
}

// ListPartyAddresses lists the addresses a party has, or had: historical addresses stay queryable.
func (s *Service) ListPartyAddresses(ctx context.Context, partyID string, asOf time.Time, includeEnded bool) ([]domain.PartyAddress, error) {
	as, err := s.addressStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := as.ListPartyAddresses(ctx, partyID, asOf, includeEnded)
	if err != nil {
		return nil, fmt.Errorf("store.ListPartyAddresses: %w", err)
	}
	return out, nil
}
