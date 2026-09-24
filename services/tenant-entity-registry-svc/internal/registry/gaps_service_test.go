package registry_test

// Negative paths for the remaining ORG-02 / ORG-03 audit gaps (migration
// 000008). Each test is the scenario the audit's "Notes" column described, or
// the shortest way around the new control.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/envelope"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	validLEI   = "5493001KJTIIGC8Y1R12"
	badLEI     = "5493001KJTIIGC8Y1R13" // one check digit off
	clerk      = "clerk-principal"
	gapsTenant = "tenant-001"
)

func as(principal, tenant string) context.Context {
	return domain.WithTenant(domain.WithPrincipal(context.Background(), principal), tenant)
}

func provisionReq(code string) domain.ProvisionTenantRequest {
	return domain.ProvisionTenantRequest{
		TenantCode: code, LegalName: "Tenant " + code, DefaultCurrencyCode: "GBP",
		PrimaryTimezone: "Europe/London", PrimaryLocale: "en-GB",
		ExternalCustomerKey: "ck-" + code, OnboardingRequestRef: "onb-" + code,
	}
}

// ---------------------------------------------------------------------------
// Gap 5 — "retried provisioning duplicates tenants"
// ---------------------------------------------------------------------------

func TestOnboarding_RetriedProvisioningReturnsTheSameTenant(t *testing.T) {
	svc, ms := baseSvc(t)
	before := len(ms.tenants)

	first, err := svc.ProvisionTenant(authCtx(), provisionReq("ONB1"), "c1")
	require.NoError(t, err)
	assert.False(t, first.IdempotentReplay)

	again, err := svc.ProvisionTenant(authCtx(), provisionReq("ONB1"), "c2")
	require.NoError(t, err)
	assert.True(t, again.IdempotentReplay)
	assert.Equal(t, first.TenantID, again.TenantID)
	assert.Len(t, ms.tenants, before+1, "a retried onboarding must not create a second tenant")
	require.NotNil(t, again.CreationApprovalRequestID)
	assert.Equal(t, *first.CreationApprovalRequestID, *again.CreationApprovalRequestID)
}

func TestOnboarding_KeyReusedForADifferentRequestIsRefused(t *testing.T) {
	svc, ms := baseSvc(t)
	_, err := svc.ProvisionTenant(authCtx(), provisionReq("ONB2"), "c1")
	require.NoError(t, err)
	before := len(ms.tenants)

	other := provisionReq("ONB3")
	other.ExternalCustomerKey = "ck-ONB2"
	_, err = svc.ProvisionTenant(authCtx(), other, "c2")
	require.ErrorIs(t, err, registry.ErrConflict)
	assert.Len(t, ms.tenants, before)
}

func TestOnboarding_KeyIsRequiredUnlessTheDevFlagIsOn(t *testing.T) {
	svc, _ := baseSvc(t)
	req := provisionReq("ONB4")
	req.ExternalCustomerKey = "  "
	_, err := svc.ProvisionTenant(authCtx(), req, "c")
	require.ErrorIs(t, err, registry.ErrOnboardingKeyRequired)

	svc.ConfigureCompatibility(false, true)
	_, err = svc.ProvisionTenant(authCtx(), req, "c")
	require.NoError(t, err)
}

func TestOnboarding_EvidenceIsRecordedAndBoundByTheCreationApproval(t *testing.T) {
	svc, ms := baseSvc(t)
	tn, err := svc.ProvisionTenant(authCtx(), provisionReq("ONB5"), "c")
	require.NoError(t, err)
	require.NotNil(t, tn.OnboardingRequestRef)
	assert.Equal(t, "onb-ONB5", *tn.OnboardingRequestRef)
	a := ms.approvals()[*tn.CreationApprovalRequestID]
	assert.Contains(t, string(a.Payload), `"onboarding_request_ref":"onb-ONB5"`)
	assert.Contains(t, ms.publishedEvents(), "tenant.created", "tenant.created is now enqueued with the approval")
}

// ---------------------------------------------------------------------------
// Gap 6b — FailedProvisioning with compensating cleanup
// ---------------------------------------------------------------------------

func failedTenant(t *testing.T, svc *registry.Service, ms *memStore, code string) *domain.Tenant {
	t.Helper()
	ms.gaps().failCompletion = errors.New("approval store unavailable")
	tn, err := svc.ProvisionTenant(authCtx(), provisionReq(code), "c")
	ms.gaps().failCompletion = nil
	require.NoError(t, err)
	require.Equal(t, domain.TenantLifecycleFailedProvisioning, tn.LifecycleState)
	return tn
}

func TestFailedProvisioning_PartialFailureIsNeverOnboardingOrActive(t *testing.T) {
	svc, ms := baseSvc(t)
	tn := failedTenant(t, svc, ms, "FP1")
	stored := ms.tenants[tn.TenantID]
	assert.Equal(t, domain.TenantLifecycleFailedProvisioning, stored.LifecycleState)
	require.NotNil(t, stored.ProvisioningFailureReason)
	assert.Contains(t, *stored.ProvisioningFailureReason, "approval store unavailable")

	// Not activatable, by the named command or the generic route.
	_, err := svc.ExecuteTenantCommand(tenantCtx(tn.TenantID), tn.TenantID, domain.TenantCommandActivate,
		domain.ExecuteTenantCommandRequest{Reason: "go live"})
	require.ErrorIs(t, err, registry.ErrInvalidTransition)
	err = svc.TransitionTenantLifecycle(tenantCtx(tn.TenantID), tn.TenantID,
		domain.TransitionTenantLifecycleRequest{TargetState: domain.TenantLifecycleActive})
	require.ErrorIs(t, err, registry.ErrInvalidTransition)

	// Not transactable.
	_, err = svc.CreateEntity(tenantCtx(tn.TenantID), domain.CreateEntityRequest{
		TenantID: tn.TenantID, EntityCode: "E", LegalName: "X", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "GBP", FiscalCalendarID: "fc", PrimaryJurisdictionID: "JUR-UK", DataResidencyPolicyID: "p",
	})
	require.ErrorIs(t, err, registry.ErrTenantNotTransactable)
}

func TestFailedProvisioning_RetryCompletesTheMissingStep(t *testing.T) {
	svc, ms := baseSvc(t)
	tn := failedTenant(t, svc, ms, "FP2")
	res, err := svc.ExecuteTenantCommand(tenantCtx(tn.TenantID), tn.TenantID, domain.TenantCommandRetryProvisioning,
		domain.ExecuteTenantCommandRequest{Reason: "store back"})
	require.NoError(t, err)
	assert.Equal(t, domain.TenantLifecycleOnboarding, res.ToState)
	a, _ := ms.LatestApprovalForSubject(context.Background(), domain.ApprovalSubjectTenantCreation, tn.TenantID)
	require.NotNil(t, a, "the step that failed — filing the creation approval — has now run")
	assert.Equal(t, domain.ApprovalPending, a.Status)
}

func TestFailedProvisioning_AbandonIsMakerCheckerAndCleansUp(t *testing.T) {
	svc, ms := baseSvc(t)
	tn := failedTenant(t, svc, ms, "FP3")
	ms.org().hostBindings["fp3.example"] = &domain.TenantHostBinding{Hostname: "fp3.example", TenantID: tn.TenantID, ActiveFlag: true}

	_, err := svc.ExecuteTenantCommand(tenantCtx(tn.TenantID), tn.TenantID, domain.TenantCommandAbandonProvisioning,
		domain.ExecuteTenantCommandRequest{Reason: "customer withdrew"})
	a := pendingOf(t, err)
	assert.Equal(t, domain.TenantLifecycleFailedProvisioning, ms.tenants[tn.TenantID].LifecycleState)
	assert.True(t, ms.org().hostBindings["fp3.example"].ActiveFlag)

	_, err = svc.ApproveRequest(tenantCtx(tn.TenantID), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)

	approveByID(t, svc, tn.TenantID, a.ApprovalRequestID)
	assert.Equal(t, domain.TenantLifecycleTerminated, ms.tenants[tn.TenantID].LifecycleState)
	assert.False(t, ms.org().hostBindings["fp3.example"].ActiveFlag, "compensating cleanup deactivates host bindings")
	for _, p := range ms.residencyPolicies {
		if p.TenantID == tn.TenantID {
			assert.False(t, p.ActiveFlag, "and residency policies")
		}
	}
}

// ---------------------------------------------------------------------------
// Gap 6a — LEI
// ---------------------------------------------------------------------------

func TestLEI_ValidationAndStorage(t *testing.T) {
	assert.True(t, domain.ValidLEI(validLEI))
	assert.False(t, domain.ValidLEI(badLEI))
	assert.False(t, domain.ValidLEI("5493001KJTIIGC8Y1R1"))

	svc, ms := baseSvc(t)
	base := domain.CreateEntityRequest{
		TenantID: gapsTenant, EntityCode: "L1", LegalName: "LEI Co", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "USD", FiscalCalendarID: "fc", PrimaryJurisdictionID: "JUR-US", DataResidencyPolicyID: "p",
	}
	for name, mut := range map[string]func(r *domain.CreateEntityRequest){
		"bad checksum":   func(r *domain.CreateEntityRequest) { r.LEI, r.LEISource, r.LEIStatus = badLEI, "GLEIF", "ISSUED" },
		"no source":      func(r *domain.CreateEntityRequest) { r.LEI, r.LEIStatus = validLEI, "ISSUED" },
		"unknown status": func(r *domain.CreateEntityRequest) { r.LEI, r.LEISource, r.LEIStatus = validLEI, "GLEIF", "ACTIVE" },
		"status, no LEI": func(r *domain.CreateEntityRequest) { r.LEIStatus = "ISSUED" },
	} {
		req := base
		mut(&req)
		_, err := svc.CreateEntity(tenantCtx(gapsTenant), req)
		require.ErrorIs(t, err, registry.ErrInvalidInput, name)
	}

	req := base
	req.LEI, req.LEISource, req.LEIStatus = validLEI, "GLEIF", "ISSUED"
	e, err := svc.CreateEntity(tenantCtx(gapsTenant), req)
	require.NoError(t, err)
	v := ms.org().profileVersions[e.LegalEntityID][0]
	require.NotNil(t, v.LEI)
	assert.Equal(t, validLEI, *v.LEI)
	assert.Equal(t, "GLEIF", *v.LEISource)
	assert.Equal(t, "ISSUED", *v.LEIStatus)
}

func TestLEI_ChangingItIsAnIndependentlyApprovedIdentityChange(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-lei", gapsTenant, "", "JUR-US")
	lei, src, st := validLEI, "GLEIF", "ISSUED"
	_, err := svc.AmendLegalProfile(tenantCtx(gapsTenant), "ent-lei", domain.AmendLegalProfileRequest{
		LEI: &lei, LEISource: &src, LEIStatus: &st,
	})
	a := pendingOf(t, err)
	v := approveByID(t, svc, gapsTenant, a.ApprovalRequestID).Result.(*domain.LegalEntityProfileVersion)
	require.NotNil(t, v.LEI)
	assert.Equal(t, validLEI, *v.LEI)
	assert.Equal(t, approverPrincipal, *v.ApprovedByPrincipalID)

	// A later unrelated amendment carries the LEI forward.
	trading := "Trading"
	v3, err := svc.AmendLegalProfile(tenantCtx(gapsTenant), "ent-lei", domain.AmendLegalProfileRequest{TradingName: &trading})
	require.NoError(t, err)
	require.NotNil(t, v3.LEI)
	assert.Equal(t, validLEI, *v3.LEI)
}

// ---------------------------------------------------------------------------
// Gap 3 — Draft → Verified → Active
// ---------------------------------------------------------------------------

func draftEntity(t *testing.T, svc *registry.Service, code string) *domain.LegalEntity {
	t.Helper()
	e, err := svc.CreateEntity(tenantCtx(gapsTenant), domain.CreateEntityRequest{
		TenantID: gapsTenant, EntityCode: code, LegalName: code + " Ltd", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "USD", FiscalCalendarID: "fc", PrimaryJurisdictionID: "JUR-US", DataResidencyPolicyID: "p",
		RegistrationNumber: "RC-" + code,
	})
	require.NoError(t, err)
	require.Equal(t, domain.EntityStatusDraft, e.EntityStatus)
	return e
}

func TestDraftEntity_CannotBeTransactedAgainstOrActivatedUnverified(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-parent", gapsTenant, "", "JUR-US")
	e := draftEntity(t, svc, "D1")
	ctx := tenantCtx(gapsTenant)

	_, err := svc.CreateHierarchy(ctx, domain.CreateHierarchyRequest{
		TenantID: gapsTenant, ParentLegalEntityID: "ent-parent", ChildLegalEntityID: e.LegalEntityID,
		RelationshipType: "SUBSIDIARY_OF", EffectiveFrom: time.Now().UTC(),
	})
	require.ErrorIs(t, err, registry.ErrEntityNotOperational)
	_, err = svc.AssignJurisdiction(ctx, e.LegalEntityID, domain.AssignJurisdictionRequest{JurisdictionID: "JUR-US"})
	require.ErrorIs(t, err, registry.ErrEntityNotOperational)
	_, err = svc.CreateTaxIdentityBundle(ctx, e.LegalEntityID, domain.CreateTaxIdentityBundleRequest{JurisdictionID: "JUR-US"})
	require.ErrorIs(t, err, registry.ErrEntityNotOperational)
	_, err = svc.CreateWorkspace(ctx, domain.CreateWorkspaceRequest{
		TenantID: gapsTenant, LegalEntityID: e.LegalEntityID, Name: "w", BillingClassification: "INTERNAL",
	})
	require.ErrorIs(t, err, registry.ErrEntityNotOperational)

	// No shortcut to ACTIVE: not by activation, not by the generic route.
	_, err = svc.ActivateLegalEntity(ctx, e.LegalEntityID, domain.ActivateLegalEntityRequest{})
	require.ErrorIs(t, err, registry.ErrInvalidTransition)
	err = svc.TransitionEntityStatus(ctx, e.LegalEntityID, domain.TransitionEntityStatusRequest{NewStatus: domain.EntityStatusActive})
	require.ErrorIs(t, err, registry.ErrInvalidTransition)
	assert.Equal(t, domain.EntityStatusDraft, ms.entities[e.LegalEntityID].EntityStatus)

	// A draft is a registry claim: a second entity for the same number is quarantined.
	_, err = svc.CreateEntity(ctx, domain.CreateEntityRequest{
		TenantID: gapsTenant, EntityCode: "D1b", LegalName: "Dup", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "USD", FiscalCalendarID: "fc", PrimaryJurisdictionID: "JUR-US", DataResidencyPolicyID: "p",
		RegistrationNumber: "RC-D1",
	})
	require.ErrorIs(t, err, registry.ErrRegistryConflict)
}

func TestVerification_IsIndependentOfRequesterAndCreator(t *testing.T) {
	svc, ms := baseSvc(t)
	e := draftEntity(t, svc, "V1") // created by testPrincipal

	// A clerk requests verification.
	err := svc.RequestEntityVerification(as(clerk, gapsTenant), e.LegalEntityID,
		domain.RequestEntityVerificationRequest{VerificationEvidenceRef: "companies-house/extract/1"})
	a := pendingOf(t, err)

	// The requester cannot approve…
	_, err = svc.ApproveRequest(as(clerk, gapsTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	// …nor can the entity's creator.
	_, err = svc.ApproveRequest(as(testPrincipal, gapsTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	assert.Equal(t, domain.EntityStatusDraft, ms.entities[e.LegalEntityID].EntityStatus)

	approveByID(t, svc, gapsTenant, a.ApprovalRequestID)
	got := ms.entities[e.LegalEntityID]
	assert.Equal(t, domain.EntityStatusVerified, got.EntityStatus)
	assert.Equal(t, approverPrincipal, *got.VerifiedByPrincipalID)
	assert.Equal(t, "companies-house/extract/1", *got.VerificationEvidenceRef)

	// Still not transactable until activated.
	_, err = svc.AssignJurisdiction(tenantCtx(gapsTenant), e.LegalEntityID, domain.AssignJurisdictionRequest{JurisdictionID: "JUR-US"})
	require.ErrorIs(t, err, registry.ErrEntityNotOperational)

	act, err := svc.ActivateLegalEntity(tenantCtx(gapsTenant), e.LegalEntityID, domain.ActivateLegalEntityRequest{Reason: "verified"})
	require.NoError(t, err)
	assert.Equal(t, domain.EntityStatusActive, act.EntityStatus)
	_, err = svc.AssignJurisdiction(tenantCtx(gapsTenant), e.LegalEntityID, domain.AssignJurisdictionRequest{JurisdictionID: "JUR-US"})
	require.NoError(t, err)
}

func TestVerification_RequiresEvidenceAndADraft(t *testing.T) {
	svc, ms := baseSvc(t)
	e := draftEntity(t, svc, "V2")
	err := svc.RequestEntityVerification(tenantCtx(gapsTenant), e.LegalEntityID, domain.RequestEntityVerificationRequest{})
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	seedEntityFor(ms, "ent-active", gapsTenant, "", "JUR-US")
	err = svc.RequestEntityVerification(tenantCtx(gapsTenant), "ent-active",
		domain.RequestEntityVerificationRequest{VerificationEvidenceRef: "x"})
	require.ErrorIs(t, err, registry.ErrInvalidTransition)
}

func TestLegacyEntityCreateActive_OnlyWhenConfigured(t *testing.T) {
	svc, _ := baseSvc(t)
	svc.ConfigureCompatibility(true, false)
	e, err := svc.CreateEntity(tenantCtx(gapsTenant), domain.CreateEntityRequest{
		TenantID: gapsTenant, EntityCode: "LG", LegalName: "Legacy", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "USD", FiscalCalendarID: "fc", PrimaryJurisdictionID: "JUR-US", DataResidencyPolicyID: "p",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.EntityStatusActive, e.EntityStatus)
}

// ---------------------------------------------------------------------------
// ❓→ decided — MergeDuplicateCandidate, non-destructive
// ---------------------------------------------------------------------------

func TestMerge_NonDestructiveAndIndependentlyApproved(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-dup", gapsTenant, "RC-X1", "JUR-US")
	seedEntityFor(ms, "ent-surv", gapsTenant, "RC-X2", "JUR-US")
	ctx := tenantCtx(gapsTenant)

	err := svc.MergeDuplicateCandidate(ctx, "ent-dup", domain.MergeDuplicateCandidateRequest{SurvivorLegalEntityID: "ent-dup", Reason: "r"})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "no merging into itself")

	err = svc.MergeDuplicateCandidate(ctx, "ent-dup", domain.MergeDuplicateCandidateRequest{
		SurvivorLegalEntityID: "ent-surv", Reason: "same company, two registrations", EvidenceRef: "review-7",
	})
	a := pendingOf(t, err)
	assert.Equal(t, domain.EntityStatusActive, ms.entities["ent-dup"].EntityStatus, "filed, not merged")

	// "No self-approval of merge".
	_, err = svc.ApproveRequest(ctx, a.ApprovalRequestID, domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)

	approveByID(t, svc, gapsTenant, a.ApprovalRequestID)
	dup := ms.entities["ent-dup"]
	assert.Equal(t, domain.EntityStatusDormant, dup.EntityStatus)
	require.NotNil(t, dup.MergedIntoLegalEntityID)
	assert.Equal(t, "ent-surv", *dup.MergedIntoLegalEntityID)
	_, stillThere := ms.entities["ent-dup"]
	assert.True(t, stillThere, "nothing is deleted")
	assert.Len(t, ms.org().profileVersions["ent-dup"], 1, "history untouched")

	// A merged duplicate cannot be revived by the generic route, nor transacted against.
	err = svc.TransitionEntityStatus(ctx, "ent-dup", domain.TransitionEntityStatusRequest{NewStatus: domain.EntityStatusActive})
	require.ErrorIs(t, err, registry.ErrInvalidTransition)
	_, err = svc.AssignJurisdiction(ctx, "ent-dup", domain.AssignJurisdictionRequest{JurisdictionID: "JUR-US"})
	require.ErrorIs(t, err, registry.ErrEntityNotOperational)

	// Lineage is readable from both sides.
	recs, err := svc.ListEntityMergeRecords(ctx, "ent-surv")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, testPrincipal, recs[0].MergedByPrincipalID)
	assert.Equal(t, approverPrincipal, recs[0].MergeApprovedByPrincipalID)

	// Unmerge: same governed path.
	err = svc.UnmergeEntity(ctx, "ent-dup", domain.UnmergeEntityRequest{Reason: "not the same after all"})
	u := pendingOf(t, err)
	_, err = svc.ApproveRequest(ctx, u.ApprovalRequestID, domain.ApproveRequestBody{PayloadFingerprint: u.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	approveByID(t, svc, gapsTenant, u.ApprovalRequestID)
	assert.Equal(t, domain.EntityStatusActive, ms.entities["ent-dup"].EntityStatus)
	assert.Nil(t, ms.entities["ent-dup"].MergedIntoLegalEntityID)
	recs, _ = svc.ListEntityMergeRecords(ctx, "ent-dup")
	require.Len(t, recs, 1, "the record is completed, not removed")
	require.NotNil(t, recs[0].UnmergedAt)
}

func TestMerge_SurvivorMustBeActiveAndUnmerged(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-a", gapsTenant, "", "JUR-US")
	s := seedEntityFor(ms, "ent-b", gapsTenant, "", "JUR-US")
	s.EntityStatus = domain.EntityStatusDissolved
	err := svc.MergeDuplicateCandidate(tenantCtx(gapsTenant), "ent-a",
		domain.MergeDuplicateCandidateRequest{SurvivorLegalEntityID: "ent-b", Reason: "r"})
	require.ErrorIs(t, err, registry.ErrConflict)
}

// ---------------------------------------------------------------------------
// ❓→ decided — hard isolation identifiers
// ---------------------------------------------------------------------------

func TestIsolation_HostBindingIsAuthorizedInThePlatformScope(t *testing.T) {
	ms := newMemStore()
	rec := &recordingAuthZ{}
	svc := newSvc(t, ms, rec, acceptAllJurisd{})
	_, err := svc.BindTenantHost(tenantCtx(gapsTenant), gapsTenant, domain.BindTenantHostRequest{Hostname: "acme.example"})
	require.NoError(t, err)
	assert.Equal(t, testPlatformScope, rec.scopeID,
		"a tenant-scope grant — a tenant admin's — must not be able to bind a hostname")
	assert.NotEqual(t, gapsTenant, rec.scopeID)
}

// ---------------------------------------------------------------------------
// ❓→ decided — sensitive identifier access scoped
// ---------------------------------------------------------------------------

func TestSensitiveTaxBundle_NeedsPermissionAndPurpose(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.bundles["b-r"] = &domain.TaxIdentityBundle{TaxIdentityBundleID: "b-r", TenantID: gapsTenant, LegalEntityID: "ent-t", DataClassification: "RESTRICTED"}
	ms.bundles["b-i"] = &domain.TaxIdentityBundle{TaxIdentityBundleID: "b-i", TenantID: gapsTenant, LegalEntityID: "ent-t", DataClassification: "INTERNAL"}

	noPurpose := tenantCtx(gapsTenant)
	_, err := svc.GetTaxIdentityBundle(noPurpose, "b-r")
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	list, err := svc.ListTaxIdentityBundles(noPurpose, "ent-t")
	require.NoError(t, err)
	require.Len(t, list, 1, "the sensitive bundle is omitted, the ordinary one is not")
	assert.Equal(t, "b-i", list[0].TaxIdentityBundleID)

	withPurpose := envelope.WithEnvelope(noPurpose, envelope.Envelope{PurposeContext: "statutory-filing"})
	b, err := svc.GetTaxIdentityBundle(withPurpose, "b-r")
	require.NoError(t, err)
	assert.Equal(t, "b-r", b.TaxIdentityBundleID)
	list, _ = svc.ListTaxIdentityBundles(withPurpose, "ent-t")
	assert.Len(t, list, 2)

	// Purpose without the permission is not enough.
	denied := newSvc(t, ms, denyAllAuthZ{}, acceptAllJurisd{})
	_, err = denied.GetTaxIdentityBundle(withPurpose, "b-r")
	require.ErrorIs(t, err, registry.ErrUnauthorized)
}
