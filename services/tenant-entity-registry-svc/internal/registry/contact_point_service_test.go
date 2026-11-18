package registry_test

// MDM-01 contact point service: validation precedes authorization precedes the store, each command is its own
// action, the actor is the verified principal, store refusals surface typed, the read applies as_of, and a store
// without contact points answers "unavailable".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type cpFake struct {
	*memStore
	calls   int
	creates []registry.ContactPointCreateCommand
	actor   string
	err     error
	rows    []domain.ContactPoint
	gotQ    domain.ContactPointQuery
}

func (c *cpFake) CreateContactPoint(_ context.Context, cmd registry.ContactPointCreateCommand) (*registry.ContactPointOutcome, error) {
	c.calls++
	c.creates = append(c.creates, cmd)
	return &registry.ContactPointOutcome{}, c.err
}
func (c *cpFake) VerifyContactPoint(_ context.Context, _ string, _ domain.VerifyContactPointRequest, actor string, _ bool) (*registry.ContactPointOutcome, error) {
	c.calls++
	c.actor = actor
	return &registry.ContactPointOutcome{}, c.err
}
func (c *cpFake) RetireContactPoint(_ context.Context, _ string, _ domain.RetireContactPointRequest, actor string, _ bool) (*registry.ContactPointOutcome, error) {
	c.calls++
	c.actor = actor
	return &registry.ContactPointOutcome{}, c.err
}
func (c *cpFake) ListPartyContactPoints(_ context.Context, _ string, q domain.ContactPointQuery) ([]domain.ContactPoint, error) {
	c.calls++
	c.gotQ = q
	return c.rows, c.err
}

func cpSvc(t *testing.T, az registry.AuthorizationClient) (*registry.Service, *cpFake) {
	t.Helper()
	cf := &cpFake{memStore: newMemStore()}
	return newSvc(t, cf, az, acceptAllJurisd{}), cf
}

func validContact() domain.CreateContactPointRequest {
	return domain.CreateContactPointRequest{Channel: domain.ContactEmail, Value: " Ops@Example.COM ", SourceClass: domain.SourceTenantAsserted}
}

func TestContactPoint_CreateValidatesBeforeAuthorizingBeforeStoring(t *testing.T) {
	svc, cf := cpSvc(t, permitAllAuthZ{})
	bad := validContact()
	bad.Value = "Ann <ann@example.com>"
	_, err := svc.CreateContactPoint(tenantCtx("tenant-001"), "p", bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, cf.calls)

	denied, cf2 := cpSvc(t, denyAllAuthZ{})
	_, err = denied.CreateContactPoint(tenantCtx("tenant-001"), "p", validContact())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, cf2.calls)

	down, cf3 := cpSvc(t, unavailableAuthZ{})
	_, err = down.CreateContactPoint(tenantCtx("tenant-001"), "p", validContact())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	assert.Zero(t, cf3.calls)

	_, err = svc.CreateContactPoint(domain.WithTenant(context.Background(), "tenant-001"), "p", validContact())
	require.Error(t, err, "no verified principal")
	assert.Zero(t, cf.calls)

	susp, cf4 := cpSvc(t, permitAllAuthZ{})
	cf4.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	_, err = susp.CreateContactPoint(tenantCtx("tenant-001"), "p", validContact())
	require.Error(t, err)
	assert.Zero(t, cf4.calls)
}

func TestContactPoint_CreatePassesTheNormalisedValueAndTheVerifiedPrincipal(t *testing.T) {
	az := &recordingAuthZ{}
	svc, cf := cpSvc(t, az)
	_, err := svc.CreateContactPoint(tenantCtx("tenant-001"), "party-1", validContact())
	require.NoError(t, err)
	require.Len(t, cf.creates, 1)
	assert.Equal(t, "Ops@example.com", cf.creates[0].Request.Value)
	assert.Equal(t, domain.ContactGeneral, cf.creates[0].Request.Purpose)
	assert.Equal(t, "party-1", cf.creates[0].PartyID)
	assert.Equal(t, testPrincipal, cf.creates[0].Actor)
	assert.Equal(t, "party.contact", az.resource)
	assert.Equal(t, "create", az.action)
}

func TestContactPoint_EachCommandIsItsOwnAction(t *testing.T) {
	for action, call := range map[string]func(*registry.Service) error{
		"verify": func(s *registry.Service) error {
			_, err := s.VerifyContactPoint(tenantCtx("tenant-001"), "c", domain.VerifyContactPointRequest{ExpectedVersion: 1, EvidenceRef: "e"})
			return err
		},
		"retire": func(s *registry.Service) error {
			_, err := s.RetireContactPoint(tenantCtx("tenant-001"), "c", domain.RetireContactPointRequest{ExpectedVersion: 1, Reason: "r"})
			return err
		},
		"read": func(s *registry.Service) error {
			_, err := s.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{})
			return err
		},
	} {
		az := &recordingAuthZ{}
		svc, cf := cpSvc(t, az)
		require.NoError(t, call(svc), action)
		assert.Equal(t, "party.contact", az.resource)
		assert.Equal(t, action, az.action)
		if action != "read" {
			assert.Equal(t, testPrincipal, cf.actor, "the actor is the verified principal, never a body field")
		}

		denied, cf2 := cpSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized})
		require.ErrorIs(t, call(denied), registry.ErrUnauthorized, action)
		assert.Zero(t, cf2.calls, "a denied %s reaches no store", action)
	}
}

func TestContactPoint_VerifyAndRetireNeedTheirJustification(t *testing.T) {
	svc, cf := cpSvc(t, permitAllAuthZ{})
	_, err := svc.VerifyContactPoint(tenantCtx("tenant-001"), "c", domain.VerifyContactPointRequest{ExpectedVersion: 1})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.RetireContactPoint(tenantCtx("tenant-001"), "c", domain.RetireContactPointRequest{ExpectedVersion: 1})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, cf.calls)
}

func TestContactPoint_StoreRefusalsSurfaceTyped(t *testing.T) {
	for _, e := range []error{registry.ErrSelfApproval, registry.ErrNotFound, registry.ErrConflict, registry.ErrStateConflict, registry.ErrVersionConflict} {
		svc, cf := cpSvc(t, permitAllAuthZ{})
		cf.err = e
		_, err := svc.VerifyContactPoint(tenantCtx("tenant-001"), "c", domain.VerifyContactPointRequest{ExpectedVersion: 1, EvidenceRef: "e"})
		require.ErrorIs(t, err, e)
		_, err = svc.CreateContactPoint(tenantCtx("tenant-001"), "p", validContact())
		require.ErrorIs(t, err, e)
		_, err = svc.RetireContactPoint(tenantCtx("tenant-001"), "c", domain.RetireContactPointRequest{ExpectedVersion: 1, Reason: "r"})
		require.ErrorIs(t, err, e)
		_, err = svc.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{})
		require.ErrorIs(t, err, e)
	}
}

func TestContactPoint_ReadAppliesAsOfAndDefaultsToNow(t *testing.T) {
	now := time.Now().UTC()
	end := now.Add(-time.Hour)
	svc, cf := cpSvc(t, permitAllAuthZ{})
	cf.rows = []domain.ContactPoint{
		{ContactPointID: "live", Value: "a@example.com", ValidFrom: now.Add(-48 * time.Hour)},
		{ContactPointID: "ended", Value: "b@example.com", ValidFrom: now.Add(-48 * time.Hour), ValidTo: &end},
		{ContactPointID: "future", Value: "c@example.com", ValidFrom: now.Add(48 * time.Hour)},
	}
	ids := func(items []domain.ContactPointItem) (out []string) {
		for _, i := range items {
			out = append(out, i.ContactPointID)
		}
		return
	}

	got, err := svc.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{})
	require.NoError(t, err)
	assert.Equal(t, []string{"live"}, ids(got), "default as_of is now")
	assert.False(t, cf.gotQ.AsOf.IsZero(), "the store is asked for a concrete instant")

	got, err = svc.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{AsOf: now.Add(-24 * time.Hour)})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"live", "ended"}, ids(got), "an earlier instant includes what has since ended")

	got, err = svc.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{AsOf: end})
	require.NoError(t, err)
	assert.Equal(t, []string{"live"}, ids(got), "valid_to is exclusive")

	got, err = svc.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{AsOf: now.Add(72 * time.Hour)})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"live", "future"}, ids(got))

	_, err = svc.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{Status: "GONE"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	empty, err := svc.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{AsOf: now.Add(-1000 * time.Hour)})
	require.NoError(t, err)
	assert.NotNil(t, empty, "an empty result is a list, not null")
}

func TestContactPoint_AStoreWithoutContactPointsIsUnavailable(t *testing.T) {
	bare := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	_, err := bare.CreateContactPoint(tenantCtx("tenant-001"), "p", validContact())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.ListPartyContactPoints(tenantCtx("tenant-001"), "p", domain.ContactPointQuery{})
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
