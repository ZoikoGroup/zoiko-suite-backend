package registry_test

// ORG-09 registration service: validation precedes authorization precedes the store, each command is authorized as
// its own action at its holder, sensitive numbers are masked unless revealed with their own permission, and expiry is
// surfaced from the dates on every read.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const regEntity = "0190a000-0000-7000-8000-00000000dddd"

type regFake struct {
	*memStore
	calls    int
	holderLE string
	holderP  string
	reg      *domain.Registration
	sweeps   int
	actor    string
	creates  []registry.RegistrationCreateCommand
}

func (r *regFake) outcome() *registry.RegistrationOutcome {
	cp := *r.reg
	return &registry.RegistrationOutcome{Registration: cp}
}

func (r *regFake) CreateRegistration(_ context.Context, cmd registry.RegistrationCreateCommand) (*registry.RegistrationOutcome, error) {
	r.calls++
	r.creates = append(r.creates, cmd)
	return r.outcome(), nil
}
func (r *regFake) VerifyRegistration(_ context.Context, _ string, _ domain.VerifyRegistrationRequest, actor string, _ bool) (*registry.RegistrationOutcome, error) {
	r.calls++
	r.actor = actor
	return r.outcome(), nil
}
func (r *regFake) RenewRegistration(_ context.Context, _ string, _ domain.RenewRegistrationRequest, actor string, _ bool) (*registry.RegistrationOutcome, error) {
	r.calls++
	r.actor = actor
	return r.outcome(), nil
}
func (r *regFake) ChangeRegistrationStatus(_ context.Context, _, _ string, _ domain.RegistrationStatus, _ domain.RegistrationEndRequest, actor string, _ bool) (*registry.RegistrationOutcome, error) {
	r.calls++
	r.actor = actor
	return r.outcome(), nil
}
func (r *regFake) GetRegistration(_ context.Context, id string) (*domain.Registration, error) {
	r.calls++
	if id == "missing" {
		return nil, nil
	}
	cp := *r.reg
	return &cp, nil
}
func (r *regFake) RegistrationHolder(_ context.Context, id string) (string, string, error) {
	r.calls++
	if id == "missing" {
		return "", "", registry.ErrNotFound
	}
	return r.holderLE, r.holderP, nil
}
func (r *regFake) RegistrationHistory(context.Context, string) ([]domain.RegistrationTransition, error) {
	r.calls++
	return nil, nil
}
func (r *regFake) FindRegistrationsByNumber(context.Context, string, domain.RegistrationType, string, string) ([]domain.Registration, error) {
	r.calls++
	cp := *r.reg
	return []domain.Registration{cp}, nil
}
func (r *regFake) ListRegistrations(context.Context, domain.RegistrationSearchQuery) ([]domain.Registration, string, error) {
	r.calls++
	cp := *r.reg
	return []domain.Registration{cp}, "", nil
}
func (r *regFake) ListExpiringRegistrations(context.Context, int, time.Time) ([]domain.Registration, error) {
	r.calls++
	cp := *r.reg
	return []domain.Registration{cp}, nil
}
func (r *regFake) SweepRegistrationExpiries(context.Context, time.Time, int) (registry.SweepResult, error) {
	r.sweeps++
	return registry.SweepResult{Expired: 1}, nil
}

func regSvc(t *testing.T, az registry.AuthorizationClient, typ domain.RegistrationType, status domain.RegistrationStatus, expiry string) (*registry.Service, *regFake) {
	t.Helper()
	rf := &regFake{memStore: newMemStore(), holderLE: regEntity,
		reg: &domain.Registration{RegistrationID: "r1", RegistrationType: typ, Number: "GB1234567890", Status: status, EffectiveFrom: "2020-01-01", ExpiryDate: expiry}}
	rf.entities[regEntity] = &domain.LegalEntity{LegalEntityID: regEntity, TenantID: "tenant-001", EntityStatus: domain.EntityStatusActive}
	return newSvc(t, rf, az, acceptAllJurisd{}), rf
}

func validReg() domain.CreateRegistrationRequest {
	return domain.CreateRegistrationRequest{RegistrationType: domain.RegLicense, IssuingAuthority: "FCA", Jurisdiction: "GB", Number: "123",
		HolderLegalEntityID: regEntity, EffectiveFrom: "2025-01-01", SourceClass: domain.SourceAuthorityDocument, EvidenceRef: "e"}
}

func TestRegistration_CreateValidatesBeforeAuthorizingBeforeStoring(t *testing.T) {
	svc, rf := regSvc(t, permitAllAuthZ{}, domain.RegLicense, domain.RegDraft, "")
	bad := validReg()
	bad.EvidenceRef = ""
	_, err := svc.CreateRegistration(tenantCtx("tenant-001"), bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, rf.calls)

	denied, rf2 := regSvc(t, denyAllAuthZ{}, domain.RegLicense, domain.RegDraft, "")
	_, err = denied.CreateRegistration(tenantCtx("tenant-001"), validReg())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, rf2.calls)

	down, rf3 := regSvc(t, unavailableAuthZ{}, domain.RegLicense, domain.RegDraft, "")
	_, err = down.CreateRegistration(tenantCtx("tenant-001"), validReg())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	assert.Zero(t, rf3.calls)

	req := validReg()
	_, err = svc.CreateRegistration(tenantCtx("tenant-001"), req)
	require.NoError(t, err)
	require.Len(t, rf.creates, 1)
	assert.Equal(t, testPrincipal, rf.creates[0].Actor, "the actor is the verified principal")
	assert.Equal(t, req.Fingerprint(), rf.creates[0].Fingerprint)

	rf.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	calls := rf.calls
	_, err = svc.CreateRegistration(tenantCtx("tenant-001"), validReg())
	require.Error(t, err)
	assert.Equal(t, calls, rf.calls, "a tenant that may not transact reaches no data")
}

func TestRegistration_CommandsAreAuthorizedAsTheirOwnActionsAtTheirHolder(t *testing.T) {
	end := domain.RegistrationEndRequest{ExpectedVersion: 1, Reason: "r", EvidenceRef: "e"}
	for name, tc := range map[string]struct {
		action string
		run    func(*registry.Service) error
	}{
		"verify": {"verify", func(s *registry.Service) error {
			_, err := s.VerifyRegistration(tenantCtx("tenant-001"), "r1", domain.VerifyRegistrationRequest{ExpectedVersion: 1, EvidenceRef: "e"})
			return err
		}},
		"renew": {"renew", func(s *registry.Service) error {
			_, err := s.RenewRegistration(tenantCtx("tenant-001"), "r1", domain.RenewRegistrationRequest{ExpectedVersion: 1, EvidenceRef: "e", NewExpiryDate: "2030-01-01"})
			return err
		}},
		"suspend": {"suspend", func(s *registry.Service) error {
			_, err := s.ChangeRegistrationStatus(tenantCtx("tenant-001"), "r1", domain.RegSuspended, end)
			return err
		}},
		"expire": {"expire", func(s *registry.Service) error {
			_, err := s.ChangeRegistrationStatus(tenantCtx("tenant-001"), "r1", domain.RegExpired, end)
			return err
		}},
		"revoke": {"revoke", func(s *registry.Service) error {
			_, err := s.ChangeRegistrationStatus(tenantCtx("tenant-001"), "r1", domain.RegRevoked, end)
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			az := &recordingAuthZ{}
			svc, rf := regSvc(t, az, domain.RegCompany, domain.RegActive, "")
			require.NoError(t, tc.run(svc))
			assert.Equal(t, "registration", az.resource)
			assert.Equal(t, tc.action, az.action)
			assert.Equal(t, regEntity, az.scopeID, "authorized at the registration's own holder, not the caller's envelope")
			assert.Equal(t, testPrincipal, rf.actor)
		})
	}

	// A party holder is authorized at the tenant, not at an entity.
	az := &recordingAuthZ{}
	svc, rf := regSvc(t, az, domain.RegCompany, domain.RegActive, "")
	rf.holderLE, rf.holderP = "", "party-1"
	_, err := svc.VerifyRegistration(tenantCtx("tenant-001"), "r1", domain.VerifyRegistrationRequest{ExpectedVersion: 1, EvidenceRef: "e"})
	require.NoError(t, err)
	assert.NotEqual(t, regEntity, az.scopeID)

	svc, rf = regSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized}, domain.RegCompany, domain.RegActive, "")
	calls := rf.calls
	_, err = svc.VerifyRegistration(tenantCtx("tenant-001"), "r1", domain.VerifyRegistrationRequest{ExpectedVersion: 1, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, calls+1, rf.calls, "a denied caller stops after the holder lookup")
	_, err = svc.VerifyRegistration(tenantCtx("tenant-001"), "missing", domain.VerifyRegistrationRequest{ExpectedVersion: 1, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = svc.ChangeRegistrationStatus(tenantCtx("tenant-001"), "r1", domain.RegActive, end)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "nothing is set ACTIVE by an ending command")
}

func TestRegistration_SensitiveNumbersAreMaskedUnlessRevealedWithPermission(t *testing.T) {
	az := &recordingAuthZ{}
	svc, _ := regSvc(t, az, domain.RegTaxReference, domain.RegActive, "")
	r, err := svc.GetRegistration(tenantCtx("tenant-001"), "r1", false)
	require.NoError(t, err)
	assert.Equal(t, "********7890", r.Number)
	assert.True(t, r.Masked)
	assert.Zero(t, az.calls, "a masked read needs no extra permission")

	r, err = svc.GetRegistration(tenantCtx("tenant-001"), "r1", true)
	require.NoError(t, err)
	assert.Equal(t, "GB1234567890", r.Number)
	assert.False(t, r.Masked)
	assert.Equal(t, "registration.number", az.resource)
	assert.Equal(t, "reveal", az.action)

	denied, _ := regSvc(t, denyAllAuthZ{}, domain.RegTaxReference, domain.RegActive, "")
	_, err = denied.GetRegistration(tenantCtx("tenant-001"), "r1", true)
	require.ErrorIs(t, err, registry.ErrUnauthorized, "asking to reveal without the permission is refused, not silently masked")

	// Search and the expiring list mask too; a non-sensitive type is shown as recorded.
	found, err := svc.FindRegistrationsByNumber(tenantCtx("tenant-001"), "GB1234567890", "", "", "", false)
	require.NoError(t, err)
	assert.Equal(t, "********7890", found[0].Number)
	list, _, err := svc.ListRegistrations(tenantCtx("tenant-001"), domain.RegistrationSearchQuery{}, false)
	require.NoError(t, err)
	assert.True(t, list[0].Masked)
	exp, err := svc.ListExpiringRegistrations(tenantCtx("tenant-001"), 30, time.Time{}, false)
	require.NoError(t, err)
	assert.True(t, exp[0].Masked)

	plain, _ := regSvc(t, permitAllAuthZ{}, domain.RegCompany, domain.RegActive, "")
	r, err = plain.GetRegistration(tenantCtx("tenant-001"), "r1", false)
	require.NoError(t, err)
	assert.Equal(t, "GB1234567890", r.Number)
	assert.False(t, r.Masked)

	_, err = svc.GetRegistration(tenantCtx("tenant-001"), "missing", false)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestRegistration_ExpiryIsSurfacedFromTheDatesOnEveryRead(t *testing.T) {
	svc, _ := regSvc(t, permitAllAuthZ{}, domain.RegCompany, domain.RegActive, "2021-01-01")
	r, err := svc.GetRegistration(tenantCtx("tenant-001"), "r1", false)
	require.NoError(t, err)
	assert.Equal(t, domain.RegActive, r.Status, "the sweep has not recorded it")
	assert.False(t, r.InForce, "but it does not read as in force")
	assert.True(t, r.Lapsed)

	fine, _ := regSvc(t, permitAllAuthZ{}, domain.RegCompany, domain.RegActive, "")
	r, err = fine.GetRegistration(tenantCtx("tenant-001"), "r1", false)
	require.NoError(t, err)
	assert.True(t, r.InForce)

	draft, _ := regSvc(t, permitAllAuthZ{}, domain.RegCompany, domain.RegDraft, "")
	r, err = draft.GetRegistration(tenantCtx("tenant-001"), "r1", false)
	require.NoError(t, err)
	assert.False(t, r.InForce, "an unverified registration never reads as active")
}

func TestRegistration_QueryValidationAndASweepNeedsNoCaller(t *testing.T) {
	svc, rf := regSvc(t, permitAllAuthZ{}, domain.RegCompany, domain.RegActive, "")
	_, err := svc.FindRegistrationsByNumber(tenantCtx("tenant-001"), " - ", "", "", "", false)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.FindRegistrationsByNumber(tenantCtx("tenant-001"), "1", "NOPE", "", "", false)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, _, err = svc.ListRegistrations(tenantCtx("tenant-001"), domain.RegistrationSearchQuery{Status: "NOPE"}, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.ListExpiringRegistrations(tenantCtx("tenant-001"), -1, time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	res, err := svc.SweepRegistrationExpiries(context.Background(), time.Now(), 30)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Expired)
	assert.Equal(t, 1, rf.sweeps)

	bare := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	_, err = bare.GetRegistration(tenantCtx("tenant-001"), "r1", false)
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.SweepRegistrationExpiries(context.Background(), time.Now(), 30)
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
