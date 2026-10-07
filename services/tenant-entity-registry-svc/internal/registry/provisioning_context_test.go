package registry_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/entitlement"
	"zoiko.io/tenant-entity-registry-svc/internal/jurisdiction"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ORG-02 §4.2 CreateTenant: required source inputs and server-resolved
// context (migration 000013), with the dev-compatibility mode OFF.

const (
	gbJurisdiction = "0b000000-0000-4000-8000-000000000001"
	kpJurisdiction = "0b000000-0000-4000-8000-000000000002"
	euRegion       = "0c000000-0000-4000-8000-000000000001"
	offRegion      = "0c000000-0000-4000-8000-000000000002"
)

// codedJurisd knows jurisdiction codes, as the HTTP validator does.
type codedJurisd map[string]string

func (j codedJurisd) ValidateExists(_ context.Context, id string) error {
	if _, ok := j[id]; !ok {
		return fmt.Errorf("%w: %s", jurisdiction.ErrJurisdictionNotFound, id)
	}
	return nil
}
func (j codedJurisd) Lookup(_ context.Context, id string) (*jurisdiction.Ref, error) {
	return &jurisdiction.Ref{JurisdictionID: id, JurisdictionCode: j[id], ActiveFlag: true}, nil
}

// fakeEntitlement answers per subscription.
type fakeEntitlement map[string]error

func (f fakeEntitlement) CheckProvisioning(_ context.Context, id, _ string) error { return f[id] }

func strictSvc(t *testing.T) (*registry.Service, *memStore) {
	t.Helper()
	ms := newMemStore()
	ms.regions = map[string]*domain.ResidencyRegion{
		euRegion:  {ResidencyRegionID: euRegion, ActiveFlag: true},
		offRegion: {ResidencyRegionID: offRegion, ActiveFlag: false},
	}
	svc := registry.NewService(ms, permitAllAuthZ{},
		codedJurisd{gbJurisdiction: "GB", kpJurisdiction: "KP"}, testPlatformScope, zap.NewNop())
	svc.ConfigureProvisioning(fakeEntitlement{
		"sub-active":  nil,
		"sub-pastdue": fmt.Errorf("%w: PAST_DUE", entitlement.ErrNotEntitled),
		"sub-missing": entitlement.ErrSubscriptionNotFound,
		"sub-outage":  entitlement.ErrUnavailable,
	}, []string{"KP"}, false)
	return svc, ms
}

func fullRequest() domain.ProvisionTenantRequest {
	return domain.ProvisionTenantRequest{
		TenantCode: "ACME-GB", LegalName: "Acme GB Ltd", DefaultCurrencyCode: "GBP",
		PrimaryTimezone: "Europe/London", PrimaryLocale: "en-GB",
		ExternalCustomerKey: "ck-acme-gb", OnboardingRequestRef: "ONB-1",
		PrimaryJurisdictionID: gbJurisdiction, ResidencyRegionID: euRegion, SubscriptionID: "sub-active",
	}
}

func TestProvisioning_ResidencyPreferenceBecomesTheHomeRegion(t *testing.T) {
	svc, ms := strictSvc(t)
	created, err := svc.ProvisionTenant(authCtx(), fullRequest(), "corr")
	require.NoError(t, err)
	require.NotNil(t, ms.lastPolicy.ResidencyRegionID, "the home region is assigned at birth, not left unset")
	assert.Equal(t, euRegion, *ms.lastPolicy.ResidencyRegionID)
	require.NotNil(t, created.PrimaryJurisdictionID)
	assert.Equal(t, gbJurisdiction, *created.PrimaryJurisdictionID)
	require.NotNil(t, created.SubscriptionID)
}

func TestProvisioning_RequiredInputsAreRequired(t *testing.T) {
	for name, mutate := range map[string]func(*domain.ProvisionTenantRequest){
		"primary jurisdiction": func(r *domain.ProvisionTenantRequest) { r.PrimaryJurisdictionID = "" },
		"residency preference": func(r *domain.ProvisionTenantRequest) { r.ResidencyRegionID = "" },
		"subscription":         func(r *domain.ProvisionTenantRequest) { r.SubscriptionID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := strictSvc(t)
			req := fullRequest()
			mutate(&req)
			_, err := svc.ProvisionTenant(authCtx(), req, "corr")
			require.ErrorIs(t, err, registry.ErrInvalidInput)
		})
	}
	svc, _ := strictSvc(t)
	req := fullRequest()
	req.OnboardingRequestRef = ""
	_, err := svc.ProvisionTenant(authCtx(), req, "corr")
	require.ErrorIs(t, err, registry.ErrSourceUnverified, "onboarding evidence is a source-verification failure")
}

func TestProvisioning_ServerResolvedContextRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*domain.ProvisionTenantRequest)
		want   error
	}{
		"unknown jurisdiction": {func(r *domain.ProvisionTenantRequest) {
			r.PrimaryJurisdictionID = "0b000000-0000-4000-8000-0000000000ff"
		}, registry.ErrReferenceInvalid},
		"restricted jurisdiction": {func(r *domain.ProvisionTenantRequest) { r.PrimaryJurisdictionID = kpJurisdiction }, registry.ErrJurisdictionRestricted},
		"unavailable region":      {func(r *domain.ProvisionTenantRequest) { r.ResidencyRegionID = offRegion }, registry.ErrReferenceInvalid},
		"unknown region":          {func(r *domain.ProvisionTenantRequest) { r.ResidencyRegionID = "0c000000-0000-4000-8000-0000000000ff" }, registry.ErrReferenceInvalid},
		"not entitled":            {func(r *domain.ProvisionTenantRequest) { r.SubscriptionID = "sub-pastdue" }, registry.ErrNotEntitled},
		"unknown subscription":    {func(r *domain.ProvisionTenantRequest) { r.SubscriptionID = "sub-missing" }, registry.ErrReferenceInvalid},
		"entitlement outage":      {func(r *domain.ProvisionTenantRequest) { r.SubscriptionID = "sub-outage" }, registry.ErrServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			svc, ms := strictSvc(t)
			req := fullRequest()
			tc.mutate(&req)
			_, err := svc.ProvisionTenant(authCtx(), req, "corr")
			require.True(t, errors.Is(err, tc.want), "got %v, want %v", err, tc.want)
			assert.Nil(t, ms.lastPolicy, "nothing is written when the context does not resolve")
		})
	}
}

// A restricted list with a validator that cannot name jurisdictions must
// refuse, not wave the tenant through.
func TestProvisioning_RestrictedListWithoutCodesFailsClosed(t *testing.T) {
	ms := newMemStore()
	ms.regions = map[string]*domain.ResidencyRegion{euRegion: {ResidencyRegionID: euRegion, ActiveFlag: true}}
	svc := registry.NewService(ms, permitAllAuthZ{}, acceptAllJurisd{}, testPlatformScope, zap.NewNop())
	svc.ConfigureProvisioning(fakeEntitlement{}, []string{"KP"}, false)
	_, err := svc.ProvisionTenant(authCtx(), fullRequest(), "corr")
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
