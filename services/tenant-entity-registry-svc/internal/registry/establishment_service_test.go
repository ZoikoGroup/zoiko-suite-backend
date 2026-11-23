package registry_test

// ORG-08 establishment service: validation precedes authorization precedes the store, each command is its own
// action, an establishment is authorized against its own legal entity, and a store without establishments answers
// "unavailable".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const estEntity = "0190a000-0000-7000-8000-00000000dddd"

type estFake struct {
	*memStore
	calls   int
	creates []registry.EstablishmentCreateCommand
	actor   string
	entity  string
}

func (e *estFake) CreateEstablishment(_ context.Context, cmd registry.EstablishmentCreateCommand) (*registry.EstablishmentOutcome, error) {
	e.calls++
	e.creates = append(e.creates, cmd)
	return &registry.EstablishmentOutcome{}, nil
}
func (e *estFake) ChangeEstablishmentStatus(_ context.Context, _ string, _ domain.ChangeEstablishmentStatusRequest, actor string, _ bool) (*registry.EstablishmentOutcome, error) {
	e.calls++
	e.actor = actor
	return &registry.EstablishmentOutcome{}, nil
}
func (e *estFake) GetEstablishment(_ context.Context, id string, _ time.Time, _ bool) (*domain.EstablishmentView, error) {
	e.calls++
	if id == "missing" {
		return nil, nil
	}
	return &domain.EstablishmentView{}, nil
}
func (e *estFake) EstablishmentEntity(_ context.Context, id string) (string, error) {
	e.calls++
	if id == "missing" {
		return "", registry.ErrNotFound
	}
	return e.entity, nil
}
func (e *estFake) ListEntityEstablishments(context.Context, string, domain.EstablishmentStatus) ([]domain.Establishment, error) {
	e.calls++
	return nil, nil
}
func (e *estFake) EstablishmentsAsOf(context.Context, string, time.Time) ([]domain.EstablishmentView, error) {
	e.calls++
	return nil, nil
}
func (e *estFake) ListEntityAddresses(context.Context, string, time.Time, bool) ([]domain.EntityAddress, error) {
	e.calls++
	return nil, nil
}

func estSvc(t *testing.T, az registry.AuthorizationClient) (*registry.Service, *estFake) {
	t.Helper()
	ef := &estFake{memStore: newMemStore(), entity: estEntity}
	ef.entities[estEntity] = &domain.LegalEntity{LegalEntityID: estEntity, TenantID: "tenant-001", EntityStatus: domain.EntityStatusActive}
	return newSvc(t, ef, az, acceptAllJurisd{}), ef
}

func validEst() domain.CreateEstablishmentRequest {
	return domain.CreateEstablishmentRequest{LegalEntityID: estEntity, EstablishmentType: domain.EstBranch, Name: "Leeds", AddressID: "a",
		SourceClass: domain.SourceAuthorityDocument, EvidenceRef: "cert"}
}

func TestEstablishment_CreateValidatesBeforeAuthorizingBeforeStoring(t *testing.T) {
	svc, ef := estSvc(t, permitAllAuthZ{})
	bad := validEst()
	bad.EvidenceRef = ""
	_, err := svc.CreateEstablishment(tenantCtx("tenant-001"), bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, ef.calls)

	denied, ef2 := estSvc(t, denyAllAuthZ{})
	_, err = denied.CreateEstablishment(tenantCtx("tenant-001"), validEst())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, ef2.calls)

	down, ef3 := estSvc(t, unavailableAuthZ{})
	_, err = down.CreateEstablishment(tenantCtx("tenant-001"), validEst())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	assert.Zero(t, ef3.calls)

	_, err = svc.CreateEstablishment(domain.WithTenant(context.Background(), "tenant-001"), validEst())
	require.Error(t, err, "no verified principal")
	assert.Zero(t, ef.calls)
}

func TestEstablishment_AnEntityMustBeOperationalAndTheTenantMustTransact(t *testing.T) {
	svc, ef := estSvc(t, permitAllAuthZ{})
	ef.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	_, err := svc.CreateEstablishment(tenantCtx("tenant-001"), validEst())
	require.Error(t, err)
	assert.Zero(t, ef.calls)
}

func TestEstablishment_CommandsAreAuthorizedAtTheirOwnEntityAsTheirOwnActions(t *testing.T) {
	for name, tc := range map[string]struct {
		to     domain.EstablishmentStatus
		action string
	}{
		"activate":   {domain.EstActive, "activate"},
		"deactivate": {domain.EstInactive, "deactivate"},
		"close":      {domain.EstClosed, "close"},
	} {
		t.Run(name, func(t *testing.T) {
			az := &recordingAuthZ{}
			svc, ef := estSvc(t, az)
			_, err := svc.ChangeEstablishmentStatus(tenantCtx("tenant-001"), "est-1",
				domain.ChangeEstablishmentStatusRequest{To: tc.to, ExpectedVersion: 1, EvidenceRef: "e", Reason: "r"})
			require.NoError(t, err)
			assert.Equal(t, "establishment", az.resource)
			assert.Equal(t, tc.action, az.action)
			assert.Equal(t, estEntity, az.scopeID, "authorized at the establishment's own legal entity, not the caller's envelope")
			assert.Equal(t, testPrincipal, ef.actor, "the actor is the verified principal")
		})
	}

	svc, ef := estSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized})
	_, err := svc.ChangeEstablishmentStatus(tenantCtx("tenant-001"), "est-1", domain.ChangeEstablishmentStatusRequest{To: domain.EstActive, ExpectedVersion: 1, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	calls := ef.calls
	_, err = svc.ChangeEstablishmentStatus(tenantCtx("tenant-001"), "missing", domain.ChangeEstablishmentStatusRequest{To: domain.EstActive, ExpectedVersion: 1, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrNotFound)
	assert.Equal(t, calls+1, ef.calls, "an unknown establishment stops at the lookup")

	_, err = svc.ChangeEstablishmentStatus(tenantCtx("tenant-001"), "est-1", domain.ChangeEstablishmentStatusRequest{To: domain.EstProposed, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestEstablishment_ReadsAndAStoreWithoutEstablishments(t *testing.T) {
	svc, _ := estSvc(t, permitAllAuthZ{})
	_, err := svc.GetEstablishment(tenantCtx("tenant-001"), "missing", time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = svc.ListEntityEstablishments(tenantCtx("tenant-001"), estEntity, "NOPE")
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.ListEntityEstablishments(tenantCtx("tenant-001"), estEntity, domain.EstActive)
	require.NoError(t, err)

	bare := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	_, err = bare.GetEstablishmentsAsOf(tenantCtx("tenant-001"), estEntity, time.Time{})
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.ListEntityAddresses(tenantCtx("tenant-001"), estEntity, time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
