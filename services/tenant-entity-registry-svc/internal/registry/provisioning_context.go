package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/entitlement"
	"zoiko.io/tenant-entity-registry-svc/internal/jurisdiction"
)

// ORG-02 §4.2 — CreateTenant's required source inputs and server-resolved
// context (migration 000013):
//
//	Required source inputs: … primary jurisdiction; default locale/timezone;
//	functional defaults; residency preference; onboarding evidence
//	Server-resolved context: available regions; plan entitlement; uniqueness;
//	onboarding policy; restricted-jurisdiction checks
//
// Before 28 Sep 2026 a tenant could be created with none of these: no
// jurisdiction, no residency preference (the home region was unassigned
// until an operator set it), no onboarding evidence and no commercial check.

var (
	// ErrJurisdictionRestricted — the primary jurisdiction is on the
	// configured restricted list. 422, JURISDICTION_RESTRICTED.
	ErrJurisdictionRestricted error = &kindError{"jurisdiction restricted", ErrInvalidTransition}
	// ErrNotEntitled — the subscription does not permit provisioning.
	// 422, NOT_ENTITLED.
	ErrNotEntitled error = &kindError{"not entitled", ErrInvalidTransition}
)

// jurisdictionLookup is implemented by the HTTP validator; the local stub
// does not, and then no code is known to check against the restricted list.
type jurisdictionLookup interface {
	Lookup(ctx context.Context, jurisdictionID string) (*jurisdiction.Ref, error)
}

// ConfigureProvisioning wires the §4.2 server-resolved checks. A nil checker
// fails provisioning closed; legacyInputs (dev only, refused at boot in
// staging and production) lets callers omit the 000013 inputs.
func (s *Service) ConfigureProvisioning(checker entitlement.Checker, restrictedCodes []string, legacyInputs bool) {
	s.entitlement = checker
	s.restrictedJurisdictions = map[string]bool{}
	for _, c := range restrictedCodes {
		s.restrictedJurisdictions[strings.ToUpper(strings.TrimSpace(c))] = true
	}
	s.legacyProvisioningInputs = legacyInputs
}

// resolveProvisioningContext validates the inputs and resolves the context.
// It returns the region to assign to the default residency policy.
func (s *Service) resolveProvisioningContext(ctx context.Context, req *domain.ProvisionTenantRequest) (*string, error) {
	req.PrimaryJurisdictionID = strings.TrimSpace(req.PrimaryJurisdictionID)
	req.ResidencyRegionID = strings.TrimSpace(req.ResidencyRegionID)
	req.SubscriptionID = strings.TrimSpace(req.SubscriptionID)
	req.OnboardingRequestRef = strings.TrimSpace(req.OnboardingRequestRef)

	if !s.legacyProvisioningInputs {
		switch {
		case req.PrimaryJurisdictionID == "":
			return nil, fmt.Errorf("%w: primary_jurisdiction_id is required (ORG-02 §4.2 source input)", ErrInvalidInput)
		case req.ResidencyRegionID == "":
			return nil, fmt.Errorf("%w: residency_region_id (the residency preference) is required (ORG-02 §4.2 source input)", ErrInvalidInput)
		case req.OnboardingRequestRef == "":
			return nil, fmt.Errorf("%w: onboarding_request_ref is required — tenant creation is evidenced by its onboarding request", ErrSourceUnverified)
		case req.SubscriptionID == "":
			return nil, fmt.Errorf("%w: subscription_id is required — plan entitlement is a §4.2 server-resolved check", ErrInvalidInput)
		}
	}

	if req.PrimaryJurisdictionID != "" {
		if _, err := uuid.Parse(req.PrimaryJurisdictionID); err != nil {
			return nil, fmt.Errorf("%w: primary_jurisdiction_id must be a UUID", ErrInvalidInput)
		}
		if err := s.jurisd.ValidateExists(ctx, req.PrimaryJurisdictionID); err != nil {
			return nil, s.mapJurisdictionErr(err, req.PrimaryJurisdictionID)
		}
		if len(s.restrictedJurisdictions) > 0 {
			l, ok := s.jurisd.(jurisdictionLookup)
			if !ok {
				// A restricted list is configured but nothing can say which
				// jurisdiction this is: refuse rather than wave it through.
				return nil, fmt.Errorf("%w: restricted-jurisdiction check cannot resolve jurisdiction codes", ErrServiceUnavailable)
			}
			ref, err := l.Lookup(ctx, req.PrimaryJurisdictionID)
			if err != nil {
				return nil, s.mapJurisdictionErr(err, req.PrimaryJurisdictionID)
			}
			if s.restrictedJurisdictions[strings.ToUpper(ref.JurisdictionCode)] {
				return nil, fmt.Errorf("%w: tenants may not be provisioned in jurisdiction %s", ErrJurisdictionRestricted, ref.JurisdictionCode)
			}
		}
	}

	var region *string
	if req.ResidencyRegionID != "" {
		if _, err := uuid.Parse(req.ResidencyRegionID); err != nil {
			return nil, fmt.Errorf("%w: residency_region_id must be a UUID", ErrInvalidInput)
		}
		r, err := s.store.GetResidencyRegionByID(ctx, req.ResidencyRegionID)
		if err != nil {
			return nil, fmt.Errorf("store.GetResidencyRegionByID: %w", err)
		}
		if r == nil || !r.ActiveFlag {
			return nil, fmt.Errorf("%w: residency_region_id %s is not an available region", ErrReferenceInvalid, req.ResidencyRegionID)
		}
		region = &req.ResidencyRegionID
	}

	if req.SubscriptionID != "" {
		if s.entitlement == nil {
			return nil, fmt.Errorf("%w: no entitlement checker configured", ErrServiceUnavailable)
		}
		err := s.entitlement.CheckProvisioning(ctx, req.SubscriptionID, domain.PrincipalFromContext(ctx))
		switch {
		case err == nil:
		case errors.Is(err, entitlement.ErrNotEntitled):
			return nil, fmt.Errorf("%w: %v", ErrNotEntitled, err)
		case errors.Is(err, entitlement.ErrSubscriptionNotFound):
			return nil, fmt.Errorf("%w: %v", ErrReferenceInvalid, err)
		default:
			return nil, ErrServiceUnavailable
		}
	}
	return region, nil
}
