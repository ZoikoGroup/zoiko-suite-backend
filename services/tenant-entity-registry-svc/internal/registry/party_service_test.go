package registry_test

// ORG-07 party service: validation precedes authorization precedes the store; the verified principal, never a
// body field, is the actor; revealing identifiers is its own permission; and a store without parties answers
// "unavailable" rather than pretending a party was recorded.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type partyFake struct {
	*memStore
	creates     []registry.PartyCreateCommand
	viewReveal  bool
	factActor   string
	factVersion int64
	allowUnver  bool
	identOut    *registry.PartyIdentifierOutcome
	storeCalls  int
}

func (p *partyFake) CreateParty(_ context.Context, cmd registry.PartyCreateCommand) (*domain.CreatePartyResult, error) {
	p.storeCalls++
	p.creates = append(p.creates, cmd)
	return &domain.CreatePartyResult{Outcome: domain.OutcomeCreated}, nil
}

func (p *partyFake) GetPartyView(_ context.Context, id string, _, _ time.Time, reveal bool) (*domain.PartyView, error) {
	p.storeCalls++
	p.viewReveal = reveal
	if id == "missing" {
		return nil, nil
	}
	return &domain.PartyView{}, nil
}

func (p *partyFake) ProposePartyFact(_ context.Context, _ string, req domain.ProposeFactRequest, actor string, allow bool) (*registry.PartyFactOutcome, error) {
	p.storeCalls++
	p.factActor, p.factVersion, p.allowUnver = actor, req.ExpectedMasterVersion, allow
	return &registry.PartyFactOutcome{}, nil
}

func (p *partyFake) AddPartyRole(context.Context, string, domain.AddPartyRoleRequest, string, bool) (*registry.PartyRoleOutcome, error) {
	p.storeCalls++
	return &registry.PartyRoleOutcome{}, nil
}

func (p *partyFake) RemovePartyRole(context.Context, string, string, domain.RemovePartyRoleRequest, string, bool) (*registry.PartyRoleOutcome, error) {
	p.storeCalls++
	return &registry.PartyRoleOutcome{}, nil
}

func (p *partyFake) LinkPartyIdentifier(context.Context, string, domain.LinkIdentifierRequest, string, bool) (*registry.PartyIdentifierOutcome, error) {
	p.storeCalls++
	return p.identOut, nil
}

func (p *partyFake) FindPartiesByIdentifier(context.Context, domain.IdentifierScheme, string, string) ([]domain.PartySummary, error) {
	p.storeCalls++
	return nil, nil
}

func (p *partyFake) SearchParties(context.Context, domain.PartySearchQuery) ([]domain.PartySummary, string, error) {
	p.storeCalls++
	return nil, "", nil
}

func (p *partyFake) GetResolutionCase(_ context.Context, id string) (*domain.ResolutionCase, error) {
	p.storeCalls++
	if id == "missing" {
		return nil, nil
	}
	return &domain.ResolutionCase{}, nil
}

func partySvc(t *testing.T, az registry.AuthorizationClient) (*registry.Service, *partyFake) {
	t.Helper()
	pf := &partyFake{memStore: newMemStore()}
	return newSvc(t, pf, az, acceptAllJurisd{}), pf
}

func validCreate() domain.CreatePartyRequest {
	return domain.CreatePartyRequest{
		CreateRequestID: "r1", PartyKind: domain.PartyLegalEntity,
		Names:       []domain.PartyNameInput{{Type: domain.FactLegalName, Value: "Nova Holdings Limited"}},
		SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "e", ValidFrom: "2025-01-01",
	}
}

func TestParty_CreateValidatesBeforeAuthorizingBeforeStoring(t *testing.T) {
	svc, pf := partySvc(t, permitAllAuthZ{})
	bad := validCreate()
	bad.EvidenceRef = ""
	_, err := svc.CreateParty(tenantCtx("tenant-001"), bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, pf.storeCalls)

	denied, pf2 := partySvc(t, denyAllAuthZ{})
	_, err = denied.CreateParty(tenantCtx("tenant-001"), validCreate())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, pf2.storeCalls, "a denied caller reaches no data")

	down, pf3 := partySvc(t, unavailableAuthZ{})
	_, err = down.CreateParty(tenantCtx("tenant-001"), validCreate())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable, "fail closed")
	assert.Zero(t, pf3.storeCalls)

	_, err = svc.CreateParty(domain.WithTenant(context.Background(), "tenant-001"), validCreate())
	require.Error(t, err, "no verified principal")
	assert.Zero(t, pf.storeCalls)
}

func TestParty_CreatePassesTheFingerprintAndTheVerifiedActor(t *testing.T) {
	svc, pf := partySvc(t, permitAllAuthZ{})
	req := validCreate()
	res, err := svc.CreateParty(tenantCtx("tenant-001"), req)
	require.NoError(t, err)
	assert.Equal(t, domain.OutcomeCreated, res.Outcome)
	require.Len(t, pf.creates, 1)
	assert.Equal(t, testPrincipal, pf.creates[0].Actor, "the actor is the verified principal")
	assert.Equal(t, req.Fingerprint(), pf.creates[0].Fingerprint)
}

func TestParty_CreateIsRefusedWhileTheTenantMayNotTransact(t *testing.T) {
	svc, pf := partySvc(t, permitAllAuthZ{})
	pf.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	_, err := svc.CreateParty(tenantCtx("tenant-001"), validCreate())
	require.Error(t, err)
	assert.Zero(t, pf.storeCalls, "ORG s8 NP4: protected writes are refused for a tenant that may not transact")
}

func TestParty_RevealingIdentifiersIsItsOwnPermission(t *testing.T) {
	az := &recordingAuthZ{}
	svc, pf := partySvc(t, az)

	_, err := svc.GetParty(tenantCtx("tenant-001"), "p1", time.Now(), time.Time{}, false)
	require.NoError(t, err)
	assert.Zero(t, az.calls, "a masked read needs no extra permission")
	assert.False(t, pf.viewReveal)

	_, err = svc.GetParty(tenantCtx("tenant-001"), "p1", time.Now(), time.Time{}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, az.calls)
	assert.Equal(t, "party.identifier", az.resource)
	assert.Equal(t, "reveal", az.action)
	assert.True(t, pf.viewReveal)

	denied, pf2 := partySvc(t, denyAllAuthZ{})
	_, err = denied.GetParty(tenantCtx("tenant-001"), "p1", time.Now(), time.Time{}, true)
	require.ErrorIs(t, err, registry.ErrUnauthorized, "asking to reveal without the permission is refused, not silently masked")
	assert.Zero(t, pf2.storeCalls)

	_, err = svc.GetParty(tenantCtx("tenant-001"), "missing", time.Now(), time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestParty_CommandsAreAuthorizedAsTheirOwnActions(t *testing.T) {
	for name, tc := range map[string]struct {
		resource, action string
		run              func(*registry.Service) error
	}{
		"create": {"party", "create", func(s *registry.Service) error {
			_, err := s.CreateParty(tenantCtx("tenant-001"), validCreate())
			return err
		}},
		"amend": {"party", "amend", func(s *registry.Service) error {
			_, err := s.ProposePartyFact(tenantCtx("tenant-001"), "p1", domain.ProposeFactRequest{FactType: domain.FactTradingName, Value: "N",
				ValidFrom: "2025-01-01", SourceClass: domain.SourceTenantAsserted, EvidenceRef: "e", ExpectedMasterVersion: 5})
			return err
		}},
		"add role": {"party.role", "manage", func(s *registry.Service) error {
			_, err := s.AddPartyRole(tenantCtx("tenant-001"), "p1", domain.AddPartyRoleRequest{RoleType: domain.RoleCustomer, ValidFrom: "2025-01-01", EvidenceRef: "e"})
			return err
		}},
		"remove role": {"party.role", "manage", func(s *registry.Service) error {
			_, err := s.RemovePartyRole(tenantCtx("tenant-001"), "p1", "r1", domain.RemovePartyRoleRequest{EndDate: "2025-02-01", Reason: "x"})
			return err
		}},
		"link identifier": {"party.identifier", "link", func(s *registry.Service) error {
			_, err := s.LinkPartyIdentifier(tenantCtx("tenant-001"), "p1", domain.LinkIdentifierRequest{PartyIdentifierInput: domain.PartyIdentifierInput{
				Scheme: domain.SchemeVAT, Value: "GB1", SourceClass: domain.SourceTenantAsserted, EvidenceRef: "e"}})
			return err
		}},
	} {
		az := &recordingAuthZ{}
		svc, pf := partySvc(t, az)
		pf.identOut = &registry.PartyIdentifierOutcome{}
		require.NoError(t, tc.run(svc), name)
		assert.Equal(t, tc.resource, az.resource, name)
		assert.Equal(t, tc.action, az.action, name)
		assert.Equal(t, testPrincipal, az.principalID, name)
	}
}

func TestParty_AmendPassesTheExpectedVersionAndTheVerifiedActor(t *testing.T) {
	svc, pf := partySvc(t, permitAllAuthZ{})
	_, err := svc.ProposePartyFact(tenantCtx("tenant-001"), "p1", domain.ProposeFactRequest{FactType: domain.FactTradingName, Value: "N",
		ValidFrom: "2025-01-01", SourceClass: domain.SourceTenantAsserted, EvidenceRef: "e", ExpectedMasterVersion: 5})
	require.NoError(t, err)
	assert.Equal(t, int64(5), pf.factVersion)
	assert.Equal(t, testPrincipal, pf.factActor)
	assert.True(t, pf.allowUnver, "the dev-only switch is carried to the store, which is where it is checked")
}

func TestParty_SensitiveIdentifierIsMaskedOnTheLinkAnswerToo(t *testing.T) {
	svc, pf := partySvc(t, permitAllAuthZ{})
	pf.identOut = &registry.PartyIdentifierOutcome{Identifier: &domain.PartyIdentifier{Scheme: domain.SchemeTaxID, Value: "1234567890"}}
	out, err := svc.LinkPartyIdentifier(tenantCtx("tenant-001"), "p1", domain.LinkIdentifierRequest{PartyIdentifierInput: domain.PartyIdentifierInput{
		Scheme: domain.SchemeTaxID, Value: "1234567890", SourceClass: domain.SourceTenantAsserted, EvidenceRef: "e"}})
	require.NoError(t, err)
	assert.Equal(t, "******7890", out.Identifier.Value)
	assert.True(t, out.Identifier.Masked)
}

func TestParty_ReadValidation(t *testing.T) {
	svc, pf := partySvc(t, permitAllAuthZ{})
	_, err := svc.FindPartiesByIdentifier(tenantCtx("tenant-001"), "PASSPORT", "", "1")
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, _, err = svc.SearchParties(tenantCtx("tenant-001"), domain.PartySearchQuery{Kind: "COMPANY"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, _, err = svc.SearchParties(tenantCtx("tenant-001"), domain.PartySearchQuery{Role: "BOSS"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, _, err = svc.SearchParties(tenantCtx("tenant-001"), domain.PartySearchQuery{Status: "DELETED"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, pf.storeCalls)

	_, err = svc.GetResolutionCase(tenantCtx("tenant-001"), "missing")
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestParty_AStoreWithoutPartiesIsUnavailableNotPretending(t *testing.T) {
	svc, _ := baseSvc(t) // memStore does not implement PartyStore
	_, err := svc.CreateParty(tenantCtx("tenant-001"), validCreate())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.GetParty(tenantCtx("tenant-001"), "p1", time.Now(), time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}

// ── MDM-05 ───────────────────────────────────────────────────────────────────

type resolutionFake struct {
	*partyFake
	calls   int
	decided domain.CaseDecisionRequest
	actor   string
}

func (r *resolutionFake) MatchParties(context.Context, domain.MatchPartiesRequest) (*domain.MatchPartiesResult, error) {
	r.calls++
	return &domain.MatchPartiesResult{}, nil
}

func (r *resolutionFake) ListResolutionCases(context.Context, string, string, int, string) ([]domain.ResolutionCase, string, error) {
	r.calls++
	return nil, "", nil
}

func (r *resolutionFake) DecideResolutionCase(_ context.Context, _ string, req domain.CaseDecisionRequest, actor string, _ bool) (*registry.CaseDecisionOutcome, error) {
	r.calls++
	r.decided, r.actor = req, actor
	return &registry.CaseDecisionOutcome{}, nil
}

func (r *resolutionFake) MergeParties(_ context.Context, _ string, _ domain.MergePartiesRequest, actor string) (*registry.MergeOutcome, error) {
	r.calls++
	r.actor = actor
	return &registry.MergeOutcome{}, nil
}

func (r *resolutionFake) SplitParties(_ context.Context, _ string, _ domain.SplitPartiesRequest, actor string) (*registry.SplitOutcome, error) {
	r.calls++
	r.actor = actor
	return &registry.SplitOutcome{}, nil
}

func (r *resolutionFake) ExplainMergeHistory(_ context.Context, id string) (*domain.MergeHistory, error) {
	r.calls++
	if id == "missing" {
		return nil, nil
	}
	return &domain.MergeHistory{}, nil
}

func resolutionSvc(t *testing.T, az registry.AuthorizationClient) (*registry.Service, *resolutionFake) {
	t.Helper()
	rf := &resolutionFake{partyFake: &partyFake{memStore: newMemStore()}}
	return newSvc(t, rf, az, acceptAllJurisd{}), rf
}

func validMerge() domain.MergePartiesRequest {
	s, m := "0190a000-0000-7000-8000-00000000aaa1", "0190a000-0000-7000-8000-00000000aaa2"
	return domain.MergePartiesRequest{SurvivorPartyID: s, MergedPartyIDs: []string{m}, ExpectedMasterVersions: map[string]int64{s: 2, m: 3},
		Reason: "same company", EvidenceRef: "extract"}
}

func TestResolution_MergeAndSplitAreValidatedThenAuthorizedThenStored(t *testing.T) {
	svc, rf := resolutionSvc(t, permitAllAuthZ{})
	bad := validMerge()
	bad.MergedPartyIDs = []string{bad.SurvivorPartyID}
	_, err := svc.MergeParties(tenantCtx("tenant-001"), "c1", bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a party is never merged into itself")
	noVersion := validMerge()
	noVersion.ExpectedMasterVersions = map[string]int64{noVersion.SurvivorPartyID: 2}
	_, err = svc.MergeParties(tenantCtx("tenant-001"), "c1", noVersion)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "the version of every party involved is required")
	_, err = svc.SplitParties(tenantCtx("tenant-001"), "c1", domain.SplitPartiesRequest{MergedPartyID: "x"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, rf.calls)

	denied, rf2 := resolutionSvc(t, denyAllAuthZ{})
	_, err = denied.MergeParties(tenantCtx("tenant-001"), "c1", validMerge())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, rf2.calls, "a denied caller reaches no data")

	_, err = svc.MergeParties(tenantCtx("tenant-001"), "c1", validMerge())
	require.NoError(t, err)
	assert.Equal(t, testPrincipal, rf.actor, "the merger is the verified principal, never a body field")
}

func TestResolution_ActionsAreTheirOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		resource, action string
		run              func(*registry.Service) error
	}{
		"merge": {"party", "merge", func(s *registry.Service) error {
			_, err := s.MergeParties(tenantCtx("tenant-001"), "c", validMerge())
			return err
		}},
		"split": {"party", "split", func(s *registry.Service) error {
			_, err := s.SplitParties(tenantCtx("tenant-001"), "c", domain.SplitPartiesRequest{MergedPartyID: "0190a000-0000-7000-8000-00000000aaa2",
				ExpectedMasterVersions: map[string]int64{"0190a000-0000-7000-8000-00000000aaa2": 2}, Reason: "r", EvidenceRef: "e"})
			return err
		}},
		"decide": {"party.resolution", "decide", func(s *registry.Service) error {
			_, err := s.DecideResolutionCase(tenantCtx("tenant-001"), "c", domain.CaseDecisionRequest{Decision: domain.DecisionDismiss, Note: "n", ExpectedCaseVersion: 1})
			return err
		}},
	} {
		az := &recordingAuthZ{}
		svc, _ := resolutionSvc(t, az)
		require.NoError(t, tc.run(svc), name)
		assert.Equal(t, tc.resource, az.resource, name)
		assert.Equal(t, tc.action, az.action, name)
	}
}

func TestResolution_DecisionNeedsANoteAndLinkNeedsAParty(t *testing.T) {
	svc, rf := resolutionSvc(t, permitAllAuthZ{})
	_, err := svc.DecideResolutionCase(tenantCtx("tenant-001"), "c", domain.CaseDecisionRequest{Decision: domain.DecisionDismiss})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a decision states why")
	_, err = svc.DecideResolutionCase(tenantCtx("tenant-001"), "c", domain.CaseDecisionRequest{Decision: domain.DecisionLinkExisting, Note: "n"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, rf.calls)
}

func TestResolution_ReadsValidateAndNotFound(t *testing.T) {
	svc, rf := resolutionSvc(t, permitAllAuthZ{})
	_, _, err := svc.ListResolutionCases(tenantCtx("tenant-001"), "WAITING", "", 0, "")
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, _, err = svc.ListResolutionCases(tenantCtx("tenant-001"), "", "DISPUTE", 0, "")
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.MatchParties(tenantCtx("tenant-001"), domain.MatchPartiesRequest{PartyKind: domain.PartyLegalEntity})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a match needs a name or an identifier")
	assert.Zero(t, rf.calls)
	_, err = svc.ExplainMergeHistory(tenantCtx("tenant-001"), "missing")
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestResolution_AStoreWithoutResolutionIsUnavailable(t *testing.T) {
	svc, _ := partySvc(t, permitAllAuthZ{}) // partyFake has parties but not resolution
	_, err := svc.MergeParties(tenantCtx("tenant-001"), "c", validMerge())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
