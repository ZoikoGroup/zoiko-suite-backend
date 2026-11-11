package registry_test

// ORG-08 address service: validation precedes authorization precedes the store, each command is its own action,
// the verified principal is the actor, a blocked external verifier never falls back to the local rules, and a store
// without addresses answers "unavailable".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type addressFake struct {
	*memStore
	calls    int
	creates  []registry.AddressCreateCommand
	actor    string
	allow    bool
	verified domain.AddressObservation
}

func (a *addressFake) CreateAddress(_ context.Context, cmd registry.AddressCreateCommand) (*registry.AddressOutcome, error) {
	a.calls++
	a.creates = append(a.creates, cmd)
	return &registry.AddressOutcome{}, nil
}
func (a *addressFake) AmendAddress(_ context.Context, _ string, _ domain.AmendAddressRequest, actor string, allow bool) (*registry.AddressOutcome, error) {
	a.calls++
	a.actor, a.allow = actor, allow
	return &registry.AddressOutcome{}, nil
}
func (a *addressFake) VerifyAddress(_ context.Context, _ string, _ domain.VerifyAddressRequest, actor string, _ bool, check registry.AddressCheck) (*registry.VerifyOutcome, error) {
	a.calls++
	a.actor = actor
	a.verified = check(domain.AddressVersion{VersionNo: 1, Country: "GB", PostalCode: "SW1A 1AA", Normalized: "n"})
	return &registry.VerifyOutcome{Observation: a.verified}, nil
}
func (a *addressFake) GetAddress(_ context.Context, id string, _ time.Time, _ bool) (*domain.AddressView, error) {
	a.calls++
	if id == "missing" {
		return nil, nil
	}
	return &domain.AddressView{}, nil
}
func (a *addressFake) SearchAddresses(context.Context, domain.AddressSearchQuery) ([]domain.AddressSummary, string, error) {
	a.calls++
	return nil, "", nil
}
func (a *addressFake) LinkAddressToParty(context.Context, string, domain.LinkAddressRequest, string, bool) (*registry.AddressLinkOutcome, error) {
	a.calls++
	return &registry.AddressLinkOutcome{}, nil
}
func (a *addressFake) ListPartyAddresses(context.Context, string, time.Time, bool) ([]domain.PartyAddress, error) {
	a.calls++
	return nil, nil
}

func addrSvc(t *testing.T, az registry.AuthorizationClient) (*registry.Service, *addressFake) {
	t.Helper()
	af := &addressFake{memStore: newMemStore()}
	return newSvc(t, af, az, acceptAllJurisd{}), af
}

func validAddress() domain.CreateAddressRequest {
	return domain.CreateAddressRequest{AddressType: domain.AddressBilling, AddressFields: domain.AddressFields{
		Lines: []string{"1 Main St"}, Locality: "Leeds", Country: "GB", SourceClass: domain.SourceTenantAsserted, EvidenceRef: "e"}}
}

func TestAddress_CreateValidatesBeforeAuthorizingBeforeStoring(t *testing.T) {
	svc, af := addrSvc(t, permitAllAuthZ{})
	bad := validAddress()
	bad.Locality = ""
	_, err := svc.CreateAddress(tenantCtx("tenant-001"), bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, af.calls)

	reg := validAddress()
	reg.AddressType = domain.AddressRegistered
	_, err = svc.CreateAddress(tenantCtx("tenant-001"), reg)
	require.ErrorIs(t, err, registry.ErrSourceUnverified, "a registered office needs authority evidence")
	assert.Zero(t, af.calls)

	denied, af2 := addrSvc(t, denyAllAuthZ{})
	_, err = denied.CreateAddress(tenantCtx("tenant-001"), validAddress())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, af2.calls)

	down, af3 := addrSvc(t, unavailableAuthZ{})
	_, err = down.CreateAddress(tenantCtx("tenant-001"), validAddress())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	assert.Zero(t, af3.calls)

	req := validAddress()
	_, err = svc.CreateAddress(tenantCtx("tenant-001"), req)
	require.NoError(t, err)
	require.Len(t, af.creates, 1)
	assert.Equal(t, testPrincipal, af.creates[0].Actor)
	assert.Equal(t, req.Fingerprint(), af.creates[0].Fingerprint)
}

func TestAddress_RefusedWhileTheTenantMayNotTransact(t *testing.T) {
	svc, af := addrSvc(t, permitAllAuthZ{})
	af.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	_, err := svc.CreateAddress(tenantCtx("tenant-001"), validAddress())
	require.Error(t, err)
	assert.Zero(t, af.calls)
}

func TestAddress_CommandsAreAuthorizedAsTheirOwnActions(t *testing.T) {
	amend := domain.AmendAddressRequest{Reason: "r", ExpectedVersion: 1, AddressFields: validAddress().AddressFields}
	for name, tc := range map[string]struct {
		action string
		run    func(*registry.Service) error
	}{
		"create": {"create", func(s *registry.Service) error {
			_, err := s.CreateAddress(tenantCtx("tenant-001"), validAddress())
			return err
		}},
		"amend": {"amend", func(s *registry.Service) error {
			_, err := s.AmendAddress(tenantCtx("tenant-001"), "a", amend)
			return err
		}},
		"verify": {"verify", func(s *registry.Service) error {
			_, err := s.VerifyAddress(tenantCtx("tenant-001"), "a", domain.VerifyAddressRequest{ExpectedVersion: 1})
			return err
		}},
		"link": {"link", func(s *registry.Service) error {
			_, err := s.LinkAddressToParty(tenantCtx("tenant-001"), "p", domain.LinkAddressRequest{AddressID: "a", LinkType: domain.AddressBilling, EvidenceRef: "e"})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			az := &recordingAuthZ{}
			svc, _ := addrSvc(t, az)
			require.NoError(t, tc.run(svc))
			assert.Equal(t, "address", az.resource)
			assert.Equal(t, tc.action, az.action)
		})
	}
}

func TestAddress_VerifyUsesTheLocalRulesAndTheExternalProviderIsBlocked(t *testing.T) {
	svc, af := addrSvc(t, permitAllAuthZ{})
	out, err := svc.VerifyAddress(tenantCtx("tenant-001"), "a", domain.VerifyAddressRequest{ExpectedVersion: 1})
	require.NoError(t, err)
	assert.Equal(t, domain.VerifierLocalRules, out.Observation.Verifier)
	assert.Equal(t, domain.VerifyPassed, af.verified.Result)
	assert.Equal(t, testPrincipal, af.actor)

	calls := af.calls
	_, err = svc.VerifyAddress(tenantCtx("tenant-001"), "a", domain.VerifyAddressRequest{ExpectedVersion: 1, Verifier: domain.VerifierExternal})
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	assert.Equal(t, calls, af.calls, "a blocked provider reaches no store and does not fall back to local rules")
}

func TestAddress_ReadsAndSearchValidation(t *testing.T) {
	svc, af := addrSvc(t, permitAllAuthZ{})
	_, err := svc.GetAddress(tenantCtx("tenant-001"), "missing", time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, _, err = svc.SearchAddresses(tenantCtx("tenant-001"), domain.AddressSearchQuery{Type: "nope"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, _, err = svc.SearchAddresses(tenantCtx("tenant-001"), domain.AddressSearchQuery{Country: "GBR"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	calls := af.calls
	_, _, err = svc.SearchAddresses(tenantCtx("tenant-001"), domain.AddressSearchQuery{Country: "GB"})
	require.NoError(t, err)
	assert.Equal(t, calls+1, af.calls)
}

func TestAddress_AStoreWithoutAddressesIsUnavailable(t *testing.T) {
	svc := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	_, err := svc.CreateAddress(tenantCtx("tenant-001"), validAddress())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.GetAddress(tenantCtx("tenant-001"), "a", time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
