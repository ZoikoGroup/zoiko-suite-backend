package registry_test

// ORG-02 / ORG-03 service tests.
//
// The four negative paths §8 assigns to this service (NP3–NP6) each get a test
// named for it, plus the maker-checker, expected_version and named-command
// behaviour §4.2/§4.3 require.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const orgTenant = "tenant-001"

// seedEntityFor puts an ACTIVE entity with a registry identity into the store,
// along with its version-1 profile.
func seedEntityFor(ms *memStore, entityID, tenantID, regNumber, jurisdiction string) *domain.LegalEntity {
	e := &domain.LegalEntity{
		LegalEntityID:         entityID,
		TenantID:              tenantID,
		EntityCode:            "EC-" + entityID,
		LegalName:             "Original Name Ltd",
		EntityType:            domain.EntityTypeSubsidiary,
		EntityStatus:          domain.EntityStatusActive,
		DefaultCurrencyCode:   "USD",
		PrimaryJurisdictionID: jurisdiction,
		RecordVersion:         1,
		CreatedAt:             time.Now().UTC().Add(-720 * time.Hour),
	}
	if regNumber != "" {
		e.RegistrationNumber = &regNumber
	}
	ms.entities[entityID] = e

	ms.org().profileVersions[entityID] = []*domain.LegalEntityProfileVersion{{
		ProfileVersionID:     "pv-1-" + entityID,
		TenantID:             tenantID,
		LegalEntityID:        entityID,
		VersionNumber:        1,
		LegalName:            e.LegalName,
		RegistrationNumber:   e.RegistrationNumber,
		EffectiveFrom:        e.CreatedAt,
		ChangeReason:         domain.ProfileChangeInitial,
		CreatedByPrincipalID: testPrincipal,
	}}
	return e
}

// ---------------------------------------------------------------------------
// §8 NP3 — host resolves to tenant A, request claims tenant B
// ---------------------------------------------------------------------------

func TestNP3_HostBoundToAnotherTenantIsRefused(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.org().hostBindings["acme.example.com"] = &domain.TenantHostBinding{
		HostBindingID: "hb-1",
		Hostname:      "acme.example.com",
		TenantID:      "tenant-a",
		ActiveFlag:    true,
	}

	// The host belongs to tenant-a; the request claims tenant-b.
	err := svc.VerifyHostTenant(tenantCtx("tenant-b"), "acme.example.com", "tenant-b")
	require.ErrorIs(t, err, registry.ErrHostTenantMismatch)
}

func TestNP3_HostMatchingItsOwnTenantIsAllowed(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.org().hostBindings["acme.example.com"] = &domain.TenantHostBinding{
		Hostname: "acme.example.com", TenantID: "tenant-a", ActiveFlag: true,
	}
	require.NoError(t, svc.VerifyHostTenant(tenantCtx("tenant-a"), "acme.example.com", "tenant-a"))
}

func TestNP3_HostPortIsStrippedBeforeComparison(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.org().hostBindings["acme.example.com"] = &domain.TenantHostBinding{
		Hostname: "acme.example.com", TenantID: "tenant-a", ActiveFlag: true,
	}
	// A Host header routinely carries a port. If it were not stripped the
	// binding would never match and NP3 would silently never fire.
	require.ErrorIs(t,
		svc.VerifyHostTenant(tenantCtx("tenant-b"), "acme.example.com:8443", "tenant-b"),
		registry.ErrHostTenantMismatch)
}

func TestNP3_UnknownHostnameDoesNotFallBackToATenant(t *testing.T) {
	svc, _ := baseSvc(t)

	_, err := svc.ResolveTenantByHost(context.Background(), "never-bound.example.com")
	require.ErrorIs(t, err, registry.ErrNotFound,
		"an unknown hostname must not resolve to any tenant")
}

// ---------------------------------------------------------------------------
// §8 NP4 — suspended tenant denied protected writes
// ---------------------------------------------------------------------------

func TestNP4_SuspendedTenantCannotCreateEntity(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleSuspended

	_, err := svc.CreateEntity(tenantCtx(orgTenant), domain.CreateEntityRequest{
		TenantID:              orgTenant,
		EntityCode:            "E-NP4",
		LegalName:             "Should Not Exist",
		EntityType:            domain.EntityTypeSubsidiary,
		DefaultCurrencyCode:   "USD",
		FiscalCalendarID:      "fc-1",
		PrimaryJurisdictionID: "JUR-US",
		DataResidencyPolicyID: "drp-1",
	})
	require.ErrorIs(t, err, registry.ErrTenantNotTransactable)
	assert.Empty(t, ms.entities, "no entity may be written for a suspended tenant")
}

func TestNP4_TerminatedAndOffboardingTenantsAlsoRefused(t *testing.T) {
	for _, state := range []domain.TenantLifecycleState{
		domain.TenantLifecycleOffboarding,
		domain.TenantLifecycleTerminated,
	} {
		t.Run(string(state), func(t *testing.T) {
			svc, ms := baseSvc(t)
			ms.tenants[orgTenant].LifecycleState = state

			_, err := svc.CreateEntity(tenantCtx(orgTenant), domain.CreateEntityRequest{
				TenantID: orgTenant, EntityCode: "E", LegalName: "N",
				EntityType: domain.EntityTypeSubsidiary, DefaultCurrencyCode: "USD",
				FiscalCalendarID: "fc", PrimaryJurisdictionID: "JUR-US",
				DataResidencyPolicyID: "drp",
			})
			require.ErrorIs(t, err, registry.ErrTenantNotTransactable)
		})
	}
}

func TestNP4_SuspendedTenantCanStillBeRead(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleSuspended
	seedEntityFor(ms, "ent-read", orgTenant, "", "JUR-US")

	// NP4's required result is "Deny protected write; controlled read policy
	// only if allowed". Reads staying open is the controlled-read half: an
	// operator has to be able to see what they have to resolve the suspension.
	got, err := svc.GetEntity(tenantCtx(orgTenant), "ent-read")
	require.NoError(t, err)
	assert.Equal(t, "ent-read", got.LegalEntityID)
}

func TestNP4_OnboardingTenantMayStillTransact(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleOnboarding

	// A tenant mid-provisioning must be able to have entities created — that
	// is what ONBOARDING is for. Excluding it would make provisioning
	// impossible.
	_, err := svc.CreateEntity(tenantCtx(orgTenant), domain.CreateEntityRequest{
		TenantID: orgTenant, EntityCode: "E-ONB", LegalName: "Onboarding Co",
		EntityType: domain.EntityTypeSubsidiary, DefaultCurrencyCode: "USD",
		FiscalCalendarID: "fc", PrimaryJurisdictionID: "JUR-US",
		DataResidencyPolicyID: "drp",
	})
	require.NoError(t, err)
}

func TestNP4_ResumeTenantIsNotBlockedByTheGuardItLifts(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleSuspended

	// The whole point: if lifecycle commands were gated on the lifecycle state,
	// a suspended tenant could never be resumed — the guard would make itself
	// permanent.
	res, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandResume, domain.ExecuteTenantCommandRequest{
			Reason: "review cleared",
		})
	require.NoError(t, err)
	assert.Equal(t, domain.TenantLifecycleActive, res.ToState)
}

// ---------------------------------------------------------------------------
// §8 NP5 — duplicate registry number quarantined
// ---------------------------------------------------------------------------

func TestNP5_DuplicateRegistryNumberIsQuarantinedAndEntityNotCreated(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-incumbent", orgTenant, "RC-12345", "JUR-US")

	_, err := svc.CreateEntity(tenantCtx(orgTenant), domain.CreateEntityRequest{
		TenantID:              orgTenant,
		EntityCode:            "E-DUP",
		LegalName:             "Impostor Ltd",
		RegistrationNumber:    "RC-12345",
		EntityType:            domain.EntityTypeSubsidiary,
		DefaultCurrencyCode:   "USD",
		FiscalCalendarID:      "fc",
		PrimaryJurisdictionID: "JUR-US",
		DataResidencyPolicyID: "drp",
		CorrelationID:         "corr-np5",
	})
	require.ErrorIs(t, err, registry.ErrRegistryConflict)

	// "no silent merge": the incoming entity is not written.
	assert.Len(t, ms.entities, 1, "only the incumbent entity may exist")

	// And the attempt is quarantined, not merely refused.
	conflicts, err := svc.ListRegistryConflicts(tenantCtx(orgTenant), true)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "RC-12345", conflicts[0].RegistrationNumber)
	assert.Equal(t, "ent-incumbent", conflicts[0].ExistingLegalEntityID)
	assert.Equal(t, domain.RegistryConflictOpen, conflicts[0].Status)
	assert.Equal(t, "Impostor Ltd", conflicts[0].AttemptedPayload["legal_name"],
		"the rejected claim must be preserved: there is no row to point at")
}

func TestNP5_SameNumberInADifferentJurisdictionIsNotAConflict(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-us", orgTenant, "RC-12345", "JUR-US")

	// Registry numbers are only unique within a registry. Two jurisdictions
	// issuing the same string are unrelated facts.
	_, err := svc.CreateEntity(tenantCtx(orgTenant), domain.CreateEntityRequest{
		TenantID: orgTenant, EntityCode: "E-GB", LegalName: "Same Number GB Ltd",
		RegistrationNumber: "RC-12345", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "GBP", FiscalCalendarID: "fc",
		PrimaryJurisdictionID: "JUR-GB", DataResidencyPolicyID: "drp",
	})
	require.NoError(t, err)
}

func TestNP5_DissolvedIncumbentDoesNotBlockReRegistration(t *testing.T) {
	svc, ms := baseSvc(t)
	e := seedEntityFor(ms, "ent-dead", orgTenant, "RC-999", "JUR-US")
	e.EntityStatus = domain.EntityStatusDissolved

	// NP5's wording is "two ACTIVE entities". A dissolved entity's number is
	// commonly reissued or reused by a successor; quarantining against it would
	// block a lawful re-registration.
	_, err := svc.CreateEntity(tenantCtx(orgTenant), domain.CreateEntityRequest{
		TenantID: orgTenant, EntityCode: "E-NEW", LegalName: "Successor Ltd",
		RegistrationNumber: "RC-999", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "USD", FiscalCalendarID: "fc",
		PrimaryJurisdictionID: "JUR-US", DataResidencyPolicyID: "drp",
	})
	require.NoError(t, err)
}

func TestNP5_AmendmentOntoAnotherEntitysRegistryNumberIsAlsoQuarantined(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-incumbent", orgTenant, "RC-777", "JUR-US")
	seedEntityFor(ms, "ent-other", orgTenant, "RC-111", "JUR-US")

	// The more dangerous door: creation is guarded, so an attacker or a
	// mistaken import reaches the same invalid state by amending instead.
	num := "RC-777"
	_, err := svc.AmendLegalProfile(tenantCtx(orgTenant), "ent-other",
		domain.AmendLegalProfileRequest{
			RegistrationNumber: &num,
			ChangeReason:       domain.ProfileChangeAmendment,
		})
	// Refused at PROPOSAL, not only at execution: nobody is asked to approve
	// a change that could never apply.
	require.ErrorIs(t, err, registry.ErrRegistryConflict)

	conflicts, _ := svc.ListRegistryConflicts(tenantCtx(orgTenant), true)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "AmendLegalProfile", conflicts[0].AttemptedPayload["attempted_by"])
}

func TestResolveRegistryConflict_RequiresATerminalStatusAndANote(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.org().conflicts["c-1"] = &domain.EntityRegistryConflict{
		ConflictID: "c-1", TenantID: orgTenant, Status: domain.RegistryConflictOpen,
	}
	ctx := tenantCtx(orgTenant)

	// OPEN is not a resolution.
	require.ErrorIs(t, svc.ResolveRegistryConflict(ctx, "c-1",
		domain.ResolveRegistryConflictRequest{Status: domain.RegistryConflictOpen, ResolutionNote: "x"}),
		registry.ErrInvalidInput)

	// A resolution with no note is a resolution nobody can audit.
	require.ErrorIs(t, svc.ResolveRegistryConflict(ctx, "c-1",
		domain.ResolveRegistryConflictRequest{Status: domain.RegistryConflictDismissed}),
		registry.ErrInvalidInput)

	// Filed for approval, not applied; released by a second principal.
	approvePending(t, svc, orgTenant, svc.ResolveRegistryConflict(ctx, "c-1",
		domain.ResolveRegistryConflictRequest{
			Status: domain.RegistryConflictResolvedDistinct, ResolutionNote: "different registrars"}))

	// Resolving twice must not overwrite the first resolver's conclusion.
	require.ErrorIs(t, svc.ResolveRegistryConflict(ctx, "c-1",
		domain.ResolveRegistryConflictRequest{
			Status: domain.RegistryConflictDismissed, ResolutionNote: "second opinion"}),
		registry.ErrConflict)
}

// ---------------------------------------------------------------------------
// §8 NP6 — legal name changed after financial history exists
// ---------------------------------------------------------------------------

func TestNP6_LegalNameChangeCreatesAVersionAndHistoryResolvesTheOriginal(t *testing.T) {
	svc, ms := baseSvc(t)
	e := seedEntityFor(ms, "ent-np6", orgTenant, "RC-NP6", "JUR-US")
	incorporated := e.CreatedAt
	ctx := tenantCtx(orgTenant)

	renamedAt := time.Now().UTC().Add(-1 * time.Hour)
	_, err := svc.ChangeLegalName(ctx, "ent-np6", domain.ChangeLegalNameRequest{
		LegalName:         "Renamed Holdings Plc",
		EffectiveFrom:     renamedAt,
		SourceEvidenceRef: "companies-house/filing/NM01",
	})
	v2 := approvePending(t, svc, orgTenant, err).Result.(*domain.LegalEntityProfileVersion)
	assert.Equal(t, 2, v2.VersionNumber)
	assert.Equal(t, domain.ProfileChangeLegalNameChange, v2.ChangeReason)

	// The present resolves to the new name.
	now, err := svc.GetLegalEntityAsOf(ctx, "ent-np6", time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, now.Profile)
	assert.Equal(t, "Renamed Holdings Plc", now.Profile.LegalName)

	// The instant the financial history was booked under resolves to the
	// ORIGINAL name. This is the assertion NP6 exists for: an in-place update
	// would have destroyed it.
	then, err := svc.GetLegalEntityAsOf(ctx, "ent-np6", incorporated.Add(24*time.Hour))
	require.NoError(t, err)
	require.NotNil(t, then.Profile)
	assert.Equal(t, "Original Name Ltd", then.Profile.LegalName)
	assert.Equal(t, 1, then.Profile.VersionNumber)

	// Both versions remain listed — nothing was rewritten.
	versions, err := svc.ListEntityVersions(ctx, "ent-np6")
	require.NoError(t, err)
	require.Len(t, versions, 2)
}

func TestNP6_AsOfBoundaryBelongsToTheLaterVersion(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-b", orgTenant, "", "JUR-US")
	ctx := tenantCtx(orgTenant)

	boundary := time.Now().UTC().Add(-2 * time.Hour)
	_, err := svc.ChangeLegalName(ctx, "ent-b", domain.ChangeLegalNameRequest{
		LegalName:     "Second Name Ltd",
		EffectiveFrom: boundary,
	})
	approvePending(t, svc, orgTenant, err)

	// The interval is half-open [from, to): the exact boundary instant belongs
	// to the version starting there, not the one ending there. Without this the
	// two versions would both match and the answer would depend on ordering.
	at, err := svc.GetLegalEntityAsOf(ctx, "ent-b", boundary)
	require.NoError(t, err)
	assert.Equal(t, "Second Name Ltd", at.Profile.LegalName)

	justBefore, err := svc.GetLegalEntityAsOf(ctx, "ent-b", boundary.Add(-time.Nanosecond))
	require.NoError(t, err)
	assert.Equal(t, "Original Name Ltd", justBefore.Profile.LegalName)
}

func TestAmendLegalProfile_CarriesForwardUnmentionedFields(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-cf", orgTenant, "RC-CF", "JUR-US")
	ctx := tenantCtx(orgTenant)

	office := `{"line1":"1 New Street"}`
	v2, err := svc.ChangeRegisteredOffice(ctx, "ent-cf", domain.ChangeRegisteredOfficeRequest{
		RegisteredOffice: office,
	})
	require.NoError(t, err)

	// Only the office was named; the registry number must survive. An
	// amendment that blanked every unmentioned field would quietly erase the
	// entity's registry identity.
	require.NotNil(t, v2.RegistrationNumber)
	assert.Equal(t, "RC-CF", *v2.RegistrationNumber)
	assert.Equal(t, "Original Name Ltd", v2.LegalName)
}

func TestAmendLegalProfile_ServiceAssignedChangeReasonsAreRefused(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-cr", orgTenant, "", "JUR-US")

	for _, reason := range []domain.ProfileChangeReason{
		domain.ProfileChangeInitial,
		domain.ProfileChangeInitialBackfill,
	} {
		name := "New"
		_, err := svc.AmendLegalProfile(tenantCtx(orgTenant), "ent-cr",
			domain.AmendLegalProfileRequest{
				LegalName:    &name,
				ChangeReason: reason,
			})
		require.ErrorIs(t, err, registry.ErrInvalidInput,
			"%s describes how a version was born and must not be caller-chosen", reason)
	}
}

// ---------------------------------------------------------------------------
// §4.3 — maker-checker on legal identity changes
// ---------------------------------------------------------------------------

func TestAmendLegalProfile_LegalIdentityChangeRequiresAnIndependentApprover(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-sod", orgTenant, "", "JUR-US")
	ctx := tenantCtx(orgTenant)
	name := "Renamed Ltd"

	// The old self-asserted approver field is refused, whoever it names.
	for _, named := range []string{testPrincipal, "someone-else"} {
		_, err := svc.AmendLegalProfile(ctx, "ent-sod", domain.AmendLegalProfileRequest{
			LegalName: &name, ChangeReason: domain.ProfileChangeLegalNameChange,
			ApprovedByPrincipalID: named,
		})
		require.ErrorIs(t, err, registry.ErrApprovalRequired, "body approver %q", named)
	}
	assert.Equal(t, "Original Name Ltd", ms.entities["ent-sod"].LegalName)

	// Without it, the change is filed, not applied.
	_, err := svc.AmendLegalProfile(ctx, "ent-sod", domain.AmendLegalProfileRequest{
		LegalName: &name, ChangeReason: domain.ProfileChangeLegalNameChange,
	})
	a := pendingOf(t, err)
	assert.Equal(t, "ChangeLegalName", a.CommandName)
	assert.Equal(t, "Original Name Ltd", ms.entities["ent-sod"].LegalName)

	// "Maker cannot approve legal-name/registry/jurisdiction change" (§4.3).
	_, err = svc.ApproveRequest(ctx, a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	assert.Equal(t, "Original Name Ltd", ms.entities["ent-sod"].LegalName)

	// A second verified principal releases it, and becomes approver of record.
	v := approveByID(t, svc, orgTenant, a.ApprovalRequestID).Result.(*domain.LegalEntityProfileVersion)
	assert.Equal(t, "Renamed Ltd", ms.entities["ent-sod"].LegalName)
	assert.Equal(t, testPrincipal, v.CreatedByPrincipalID)
	require.NotNil(t, v.ApprovedByPrincipalID)
	assert.Equal(t, approverPrincipal, *v.ApprovedByPrincipalID)
	require.NotNil(t, v.ApprovalRequestID)
	assert.Equal(t, a.ApprovalRequestID, *v.ApprovalRequestID)
}

func TestAmendLegalProfile_NonIdentityFieldsNeedNoApprover(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-tn", orgTenant, "", "JUR-US")

	// Trading name is not one of the three fields §4.3 puts under SoD.
	// Requiring an approver for every field would make the control routine and
	// therefore ignored.
	trading := "ACME"
	_, err := svc.AmendLegalProfile(tenantCtx(orgTenant), "ent-tn",
		domain.AmendLegalProfileRequest{TradingName: &trading})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// §4.2 — named commands
// ---------------------------------------------------------------------------

func TestTenantCommands_EachNamedCommandMovesAndRecordsItself(t *testing.T) {
	cases := []struct {
		command   domain.TenantCommand
		from, to  domain.TenantLifecycleState
		wantEvent string
	}{
		{domain.TenantCommandActivate, domain.TenantLifecycleOnboarding, domain.TenantLifecycleActive, events.EventTenantActivated},
		{domain.TenantCommandSuspend, domain.TenantLifecycleActive, domain.TenantLifecycleSuspended, events.EventTenantSuspended},
		{domain.TenantCommandResume, domain.TenantLifecycleSuspended, domain.TenantLifecycleActive, events.EventTenantResumed},
		{domain.TenantCommandInitiateTermination, domain.TenantLifecycleActive, domain.TenantLifecycleOffboarding, events.EventTenantTerminationInitiated},
		{domain.TenantCommandCompleteTermination, domain.TenantLifecycleOffboarding, domain.TenantLifecycleTerminated, events.EventTenantTerminated},
	}

	for _, tc := range cases {
		t.Run(string(tc.command), func(t *testing.T) {
			svc, ms := baseSvc(t)
			ms.tenants[orgTenant].LifecycleState = tc.from
			if tc.from == domain.TenantLifecycleOnboarding {
				seedApprovedCreation(ms, orgTenant)
			}

			req := domain.ExecuteTenantCommandRequest{Reason: "test"}
			res, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant, tc.command, req)
			if tc.command.RequiresMakerChecker() {
				res = approvePending(t, svc, orgTenant, err).Result.(*registry.TenantCommandResult)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.from, res.FromState)
			assert.Equal(t, tc.to, res.ToState)
			assert.Equal(t, int64(2), res.NewVersion, "every command bumps record_version")

			// §4.2's durable lifecycle evidence, naming the COMMAND rather
			// than just the destination state.
			history, err := svc.ListTenantLifecycleHistory(tenantCtx(orgTenant), orgTenant)
			require.NoError(t, err)
			require.Len(t, history, 1)
			assert.Equal(t, tc.command, history[0].CommandName)
			assert.Equal(t, "test", history[0].Reason)
			assert.Equal(t, testPrincipal, history[0].ActorPrincipalID)

			assert.Contains(t, ms.publishedEvents(), tc.wantEvent)
		})
	}
}

func TestTenantCommands_UnknownCommandIsRefused(t *testing.T) {
	svc, _ := baseSvc(t)
	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommand("DeleteTenant"), domain.ExecuteTenantCommandRequest{Reason: "r"})
	require.ErrorIs(t, err, registry.ErrUnknownCommand)
}

func TestTenantCommands_ReasonIsMandatory(t *testing.T) {
	svc, _ := baseSvc(t)
	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandSuspend, domain.ExecuteTenantCommandRequest{})
	require.ErrorIs(t, err, registry.ErrInvalidInput,
		"a lifecycle history whose reason is empty answers 'why is this suspended?' with silence")
}

func TestTenantCommands_CommandFromAnIllegalStateIsRefused(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleOnboarding

	// SuspendTenant is reachable only from ACTIVE.
	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandSuspend, domain.ExecuteTenantCommandRequest{Reason: "r"})
	require.ErrorIs(t, err, registry.ErrInvalidTransition)
}

func TestTenantCommands_TerminationRequiresAnIndependentApprover(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	ctx := tenantCtx(orgTenant)

	// The audit's scenario: one person typing a second name. Refused.
	_, err := svc.ExecuteTenantCommand(ctx, orgTenant, domain.TenantCommandInitiateTermination,
		domain.ExecuteTenantCommandRequest{Reason: "contract ended", ApprovedByPrincipalID: "a-colleague"})
	require.ErrorIs(t, err, registry.ErrApprovalRequired)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)

	// Filed, not executed.
	_, err = svc.ExecuteTenantCommand(ctx, orgTenant, domain.TenantCommandInitiateTermination,
		domain.ExecuteTenantCommandRequest{Reason: "contract ended"})
	a := pendingOf(t, err)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)
	history, _ := svc.ListTenantLifecycleHistory(ctx, orgTenant)
	assert.Empty(t, history, "nothing ran, so nothing is recorded as having run")

	// No self-approval.
	_, err = svc.ApproveRequest(ctx, a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)
	still, _ := svc.GetApprovalRequest(ctx, a.ApprovalRequestID)
	assert.Equal(t, domain.ApprovalPending, still.Status)

	// A second verified principal releases it.
	approveByID(t, svc, orgTenant, a.ApprovalRequestID)
	assert.Equal(t, domain.TenantLifecycleOffboarding, ms.tenants[orgTenant].LifecycleState)
	history, _ = svc.ListTenantLifecycleHistory(ctx, orgTenant)
	require.Len(t, history, 1)
	assert.Equal(t, testPrincipal, history[0].ActorPrincipalID)
	require.NotNil(t, history[0].ApprovedByPrincipalID)
	assert.Equal(t, approverPrincipal, *history[0].ApprovedByPrincipalID)
}

func TestTenantCommands_SuspensionDeliberatelyNeedsNoApprover(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive

	// Suspension is a containment action taken during an incident. Requiring a
	// second approver would leave a compromised tenant live until one is found.
	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandSuspend, domain.ExecuteTenantCommandRequest{Reason: "incident 4412"})
	require.NoError(t, err)
}

func TestTenantCommands_StatusMovesInStepWithLifecycle(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive

	res, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandSuspend, domain.ExecuteTenantCommandRequest{Reason: "r"})
	require.NoError(t, err)

	// A SUSPENDED tenant whose status still reads ACTIVE is the inconsistency
	// NP4 turns on — a caller checking the wrong one would keep transacting.
	assert.Equal(t, domain.TenantStatusSuspended, res.Status)
	assert.Equal(t, domain.TenantStatusSuspended, ms.tenants[orgTenant].Status)
}

// ---------------------------------------------------------------------------
// §4.2 / §4.3 — expected_version
// ---------------------------------------------------------------------------

func TestExpectedVersion_StaleValueIsRefused(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	ms.tenants[orgTenant].RecordVersion = 7

	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandSuspend, domain.ExecuteTenantCommandRequest{
			Reason: "r", ExpectedVersion: 3,
		})
	require.ErrorIs(t, err, registry.ErrConflict)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState,
		"a refused command must not have applied")
}

func TestExpectedVersion_OmittedIsTreatedAsCompareAndSwap(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	ms.tenants[orgTenant].RecordVersion = 7

	// Omitting expected_version must not mean "skip the guard": the version
	// just read is substituted, so a concurrent writer is still detected.
	res, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandSuspend, domain.ExecuteTenantCommandRequest{Reason: "r"})
	require.NoError(t, err)
	assert.Equal(t, int64(8), res.NewVersion)
}

func TestExpectedVersion_MatchingValueIsAccepted(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	ms.tenants[orgTenant].RecordVersion = 4

	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandSuspend, domain.ExecuteTenantCommandRequest{
			Reason: "r", ExpectedVersion: 4,
		})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// §4.2 — remaining read surfaces
// ---------------------------------------------------------------------------

func TestChangeDefaultLocale_UpdatesAndRecordsTheCommand(t *testing.T) {
	svc, ms := baseSvc(t)

	updated, err := svc.ChangeDefaultLocale(tenantCtx(orgTenant), orgTenant,
		domain.ChangeDefaultLocaleRequest{
			PrimaryLocale: "fr-FR", Reason: "customer relocated",
		})
	require.NoError(t, err)
	assert.Equal(t, "fr-FR", updated.PrimaryLocale)

	// Not a lifecycle transition, but still recorded: an unrecorded
	// configuration change is one nobody can attribute later.
	history, _ := svc.ListTenantLifecycleHistory(tenantCtx(orgTenant), orgTenant)
	require.Len(t, history, 1)
	assert.Equal(t, domain.TenantCommandChangeDefaultLocale, history[0].CommandName)
	assert.Equal(t, domain.TenantLifecycleActive, history[0].ToState,
		"lifecycle state must be untouched by a defaults change")
	assert.Contains(t, ms.publishedEvents(), events.EventTenantDefaultsChanged)
}

func TestGetTenantDefaults_ReturnsBaselineConfiguration(t *testing.T) {
	svc, _ := baseSvc(t)
	d, err := svc.GetTenantDefaults(tenantCtx(orgTenant), orgTenant)
	require.NoError(t, err)
	assert.Equal(t, "USD", d.DefaultCurrencyCode)
	assert.Equal(t, "en-US", d.PrimaryLocale)
	assert.Equal(t, int64(1), d.RecordVersion)
}

func TestORGReads_RefuseACrossTenantRequest(t *testing.T) {
	svc, _ := baseSvc(t)

	// tenant-b's session asking about tenant-a. Every tenant-scoped ORG read
	// must go through assertTenantScope, not just the pre-existing ones.
	for name, call := range map[string]func() error{
		"lifecycle history": func() error {
			_, err := svc.ListTenantLifecycleHistory(tenantCtx("tenant-b"), "tenant-a")
			return err
		},
		"tenant defaults": func() error {
			_, err := svc.GetTenantDefaults(tenantCtx("tenant-b"), "tenant-a")
			return err
		},
		"host bindings": func() error {
			_, err := svc.ListTenantHostBindings(tenantCtx("tenant-b"), "tenant-a")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, call(), registry.ErrNotFound)
		})
	}
}

func TestFindByRegistryNumber_ReturnsAllCandidatesNotOne(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-1", orgTenant, "RC-SAME", "JUR-US")
	e2 := seedEntityFor(ms, "ent-2", orgTenant, "RC-SAME", "JUR-US")
	e2.EntityStatus = domain.EntityStatusDissolved

	// §4.3: "registry+jurisdiction is dedup signal not universal identifier".
	// Returning one entity would present a hint as an identity lookup — the
	// assumption that produces silent merges.
	found, err := svc.FindByRegistryNumber(tenantCtx(orgTenant), "RC-SAME", "JUR-US")
	require.NoError(t, err)
	assert.Len(t, found, 2)
}
