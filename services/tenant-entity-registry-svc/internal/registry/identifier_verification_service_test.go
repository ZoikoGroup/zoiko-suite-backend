package registry_test

// Identifier verification service: validation precedes authorization precedes the store, the verified principal is the
// actor, a sensitive identifier is masked in the answer, and a store without verification answers "unavailable".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type ivFake struct {
	*memStore
	calls  int
	actor  string
	scheme domain.IdentifierScheme
	got    domain.VerifyIdentifierRequest
}

func (f *ivFake) VerifyPartyIdentifier(_ context.Context, id string, req domain.VerifyIdentifierRequest, actor string, _ bool) (*registry.IdentifierVerifyOutcome, error) {
	f.calls++
	f.actor, f.got = actor, req
	return &registry.IdentifierVerifyOutcome{Identifier: domain.PartyIdentifier{IdentifierID: id, Scheme: f.scheme, Value: "AB1234567890"}}, nil
}
func (f *ivFake) ListIdentifierVerifications(context.Context, string) ([]domain.IdentifierObservation, error) {
	f.calls++
	return nil, nil
}

func ivSvc(t *testing.T, az registry.AuthorizationClient, scheme domain.IdentifierScheme) (*registry.Service, *ivFake) {
	t.Helper()
	f := &ivFake{memStore: newMemStore(), scheme: scheme}
	return newSvc(t, f, az, acceptAllJurisd{}), f
}

func validVerify() domain.VerifyIdentifierRequest {
	return domain.VerifyIdentifierRequest{ExpectedMasterVersion: 1, Result: domain.ObsMatch, SourceClass: domain.SourceAuthorityRegistry,
		SourceRef: "Registry", ObservedAt: time.Now().UTC().Add(-time.Hour), EvidenceRef: "resp"}
}

func TestIdentifierVerification_ValidatesBeforeAuthorizingBeforeStoring(t *testing.T) {
	svc, f := ivSvc(t, permitAllAuthZ{}, domain.SchemeVAT)
	bad := validVerify()
	bad.ObservedAt = time.Now().UTC().Add(time.Hour)
	_, err := svc.VerifyPartyIdentifier(tenantCtx("tenant-001"), "i1", bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "an observation from the future is refused")
	assert.Zero(t, f.calls)

	denied, f2 := ivSvc(t, denyAllAuthZ{}, domain.SchemeVAT)
	_, err = denied.VerifyPartyIdentifier(tenantCtx("tenant-001"), "i1", validVerify())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, f2.calls)

	down, f3 := ivSvc(t, unavailableAuthZ{}, domain.SchemeVAT)
	_, err = down.VerifyPartyIdentifier(tenantCtx("tenant-001"), "i1", validVerify())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	assert.Zero(t, f3.calls)

	_, err = svc.VerifyPartyIdentifier(domain.WithTenant(context.Background(), "tenant-001"), "i1", validVerify())
	require.Error(t, err, "no verified principal")
	assert.Zero(t, f.calls)

	f.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	_, err = svc.VerifyPartyIdentifier(tenantCtx("tenant-001"), "i1", validVerify())
	require.Error(t, err)
	assert.Zero(t, f.calls, "a tenant that may not transact reaches no data")
}

func TestIdentifierVerification_IsItsOwnActionAndTheActorIsTheVerifiedPrincipal(t *testing.T) {
	az := &recordingAuthZ{}
	svc, f := ivSvc(t, az, domain.SchemeVAT)
	out, err := svc.VerifyPartyIdentifier(tenantCtx("tenant-001"), "i1", validVerify())
	require.NoError(t, err)
	assert.Equal(t, "party.identifier", az.resource)
	assert.Equal(t, "verify", az.action)
	assert.Equal(t, testPrincipal, f.actor, "never a body field")
	assert.Equal(t, "i1", out.Identifier.IdentifierID)
	assert.Equal(t, "AB1234567890", out.Identifier.Value, "a non-sensitive identifier is shown as recorded")
	assert.False(t, out.Identifier.Masked)
}

func TestIdentifierVerification_ASensitiveIdentifierIsMaskedInTheAnswer(t *testing.T) {
	svc, _ := ivSvc(t, permitAllAuthZ{}, domain.SchemeTaxID)
	out, err := svc.VerifyPartyIdentifier(tenantCtx("tenant-001"), "i1", validVerify())
	require.NoError(t, err)
	assert.Equal(t, "********7890", out.Identifier.Value)
	assert.True(t, out.Identifier.Masked)
}

func TestIdentifierVerification_AStoreWithoutItIsUnavailable(t *testing.T) {
	bare := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	_, err := bare.VerifyPartyIdentifier(tenantCtx("tenant-001"), "i1", validVerify())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.ListIdentifierVerifications(tenantCtx("tenant-001"), "i1")
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)

	svc, f := ivSvc(t, permitAllAuthZ{}, domain.SchemeVAT)
	_, err = svc.ListIdentifierVerifications(tenantCtx("tenant-001"), "i1")
	require.NoError(t, err)
	assert.Equal(t, 1, f.calls)
}
