package registry_test

// Verified maker-checker — the negative paths.
//
// The audit's finding was that every "independently approved" control in
// ORG-02 and ORG-03 "is satisfiable by one person typing a second name". Each
// test below is one way a single person, or a stale or tampered approval,
// might still get a governed command through; each must fail.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// approverPrincipal is a second verified identity, distinct from testPrincipal.
const approverPrincipal = "approver-principal"

func approverCtx(tenantID string) context.Context {
	return domain.WithTenant(domain.WithPrincipal(context.Background(), approverPrincipal), tenantID)
}

// pendingOf asserts err is a filed-for-approval result and returns the request.
func pendingOf(t *testing.T, err error) *domain.ApprovalRequest {
	t.Helper()
	var p *registry.PendingApprovalError
	require.ErrorAs(t, err, &p, "expected the command to be filed for approval")
	require.Equal(t, domain.ApprovalPending, p.Request.Status)
	return p.Request
}

// approveByID approves as approverPrincipal, presenting the stored fingerprint.
func approveByID(t *testing.T, svc *registry.Service, tenantID, id string) *domain.ApprovalOutcome {
	t.Helper()
	a, err := svc.GetApprovalRequest(approverCtx(tenantID), id)
	require.NoError(t, err)
	out, err := svc.ApproveRequest(approverCtx(tenantID), id,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.NoError(t, err)
	require.Equal(t, domain.ApprovalApproved, out.ApprovalRequest.Status)
	return out
}

// approvePending is pendingOf followed by approveByID.
func approvePending(t *testing.T, svc *registry.Service, tenantID string, err error) *domain.ApprovalOutcome {
	t.Helper()
	return approveByID(t, svc, tenantID, pendingOf(t, err).ApprovalRequestID)
}

// seedApprovedCreation records that tenantID's creation was approved.
func seedApprovedCreation(ms *memStore, tenantID string) {
	now := time.Now().UTC()
	by := approverPrincipal
	ms.approvals()["apr-create-"+tenantID] = &domain.ApprovalRequest{
		ApprovalRequestID: "apr-create-" + tenantID, TenantID: tenantID,
		SubjectType: domain.ApprovalSubjectTenantCreation, SubjectID: tenantID,
		CommandName: string(domain.TenantCommandCreate), RequestedByPrincipalID: testPrincipal,
		RequestedAt: now, ExpiresAt: now.Add(time.Hour), Status: domain.ApprovalApproved,
		DecidedByPrincipalID: &by, DecidedAt: &now,
	}
}

// proposeTermination files InitiateTermination on an ACTIVE orgTenant.
func proposeTermination(t *testing.T, svc *registry.Service, ms *memStore) *domain.ApprovalRequest {
	t.Helper()
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandInitiateTermination, domain.ExecuteTenantCommandRequest{Reason: "contract ended"})
	return pendingOf(t, err)
}

// denyApproveAuthZ grants everything except the *.approve actions.
type denyApproveAuthZ struct{}

func (denyApproveAuthZ) Authorize(_ context.Context, _, _, _, action string) error {
	if strings.HasSuffix(action, ".approve") {
		return authz.ErrUnauthorized
	}
	return nil
}

// ---------------------------------------------------------------------------
// Gap 1 — the approver can no longer be self-asserted
// ---------------------------------------------------------------------------

func TestMakerChecker_BodyApproverIsRefusedOnEveryCommand(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	seedEntityFor(ms, "ent-ro", orgTenant, "", "JUR-US")

	// Not only on maker-checker commands: on a suspension it would have been
	// written into lifecycle history as an approver of a command nobody
	// approved.
	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant, domain.TenantCommandSuspend,
		domain.ExecuteTenantCommandRequest{Reason: "incident", ApprovedByPrincipalID: "a-colleague"})
	require.ErrorIs(t, err, registry.ErrApprovalRequired)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)

	_, err = svc.ChangeRegisteredOffice(tenantCtx(orgTenant), "ent-ro", domain.ChangeRegisteredOfficeRequest{
		RegisteredOffice: `{"line1":"x"}`, ApprovedByPrincipalID: "a-colleague",
	})
	require.ErrorIs(t, err, registry.ErrApprovalRequired)
	versions, _ := svc.ListEntityVersions(tenantCtx(orgTenant), "ent-ro")
	assert.Len(t, versions, 1, "no version was written")
}

func TestMakerChecker_ApprovalMustPresentTheReviewedFingerprint(t *testing.T) {
	svc, ms := baseSvc(t)
	a := proposeTermination(t, svc, ms)

	_, err := svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID, domain.ApproveRequestBody{})
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	_, err = svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: strings.Repeat("0", 64)})
	require.ErrorIs(t, err, registry.ErrApprovalFingerprintMismatch)

	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)
	still, _ := svc.GetApprovalRequest(tenantCtx(orgTenant), a.ApprovalRequestID)
	assert.Equal(t, domain.ApprovalPending, still.Status, "a wrong fingerprint is the approver's error, not the proposal's")
}

func TestMakerChecker_TamperedProposalIsRefusedAndGoesStale(t *testing.T) {
	svc, ms := baseSvc(t)
	a := proposeTermination(t, svc, ms)

	// The stored payload is changed after proposal; the fingerprint shown to
	// the approver is the original one.
	var req domain.ExecuteTenantCommandRequest
	require.NoError(t, json.Unmarshal(ms.approvals()[a.ApprovalRequestID].Payload, &req))
	req.Reason = "something else entirely"
	ms.approvals()[a.ApprovalRequestID].Payload, _ = json.Marshal(req)

	_, err := svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrApprovalFingerprintMismatch)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)
	assert.Equal(t, domain.ApprovalStale, ms.approvals()[a.ApprovalRequestID].Status)
}

func TestMakerChecker_ApproverNeedsTheApprovePermission(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, denyApproveAuthZ{}, acceptAllJurisd{})
	a := proposeTermination(t, svc, ms)

	// Being a different person is necessary, not sufficient.
	_, err := svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)
	assert.Equal(t, domain.ApprovalPending, ms.approvals()[a.ApprovalRequestID].Status)
}

func TestMakerChecker_ApprovalIsSpentOnce(t *testing.T) {
	svc, ms := baseSvc(t)
	a := proposeTermination(t, svc, ms)
	approveByID(t, svc, orgTenant, a.ApprovalRequestID)

	_, err := svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrApprovalNotPending)
	history, _ := svc.ListTenantLifecycleHistory(tenantCtx(orgTenant), orgTenant)
	assert.Len(t, history, 1, "the command ran exactly once")
}

func TestMakerChecker_ExpiredRequestCannotBeApproved(t *testing.T) {
	svc, ms := baseSvc(t)
	a := proposeTermination(t, svc, ms)
	ms.approvals()[a.ApprovalRequestID].ExpiresAt = time.Now().UTC().Add(-time.Minute)

	_, err := svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrApprovalNotPending)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)
	assert.Equal(t, domain.ApprovalExpired, ms.approvals()[a.ApprovalRequestID].Status)

	// And it no longer blocks a fresh proposal.
	_, err = svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandInitiateTermination, domain.ExecuteTenantCommandRequest{Reason: "again"})
	pendingOf(t, err)
}

func TestMakerChecker_SubjectMovedAfterProposalMarksItStale(t *testing.T) {
	svc, ms := baseSvc(t)
	a := proposeTermination(t, svc, ms)

	// Someone suspends the tenant while the termination awaits approval.
	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant, domain.TenantCommandSuspend,
		domain.ExecuteTenantCommandRequest{Reason: "incident"})
	require.NoError(t, err)

	_, err = svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrConflict)
	assert.Equal(t, domain.TenantLifecycleSuspended, ms.tenants[orgTenant].LifecycleState,
		"an approval of a proposal against version N is not an approval of version N+1")
	assert.Equal(t, domain.ApprovalStale, ms.approvals()[a.ApprovalRequestID].Status)
}

func TestMakerChecker_ApproverFromAnotherTenantSeesNothing(t *testing.T) {
	svc, ms := baseSvc(t)
	a := proposeTermination(t, svc, ms)

	_, err := svc.ApproveRequest(approverCtx("tenant-b"), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrNotFound)
	assert.Equal(t, domain.ApprovalPending, ms.approvals()[a.ApprovalRequestID].Status)
}

func TestMakerChecker_OneOpenProposalPerSubject(t *testing.T) {
	svc, ms := baseSvc(t)
	proposeTermination(t, svc, ms)

	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant,
		domain.TenantCommandInitiateTermination, domain.ExecuteTenantCommandRequest{Reason: "dup"})
	require.ErrorIs(t, err, registry.ErrApprovalPending)
}

func TestMakerChecker_RejectionIsIndependentToo(t *testing.T) {
	svc, ms := baseSvc(t)
	a := proposeTermination(t, svc, ms)

	_, err := svc.RejectRequest(approverCtx(orgTenant), a.ApprovalRequestID, domain.RejectRequestBody{})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a rejection nobody can explain is not reviewable")

	_, err = svc.RejectRequest(tenantCtx(orgTenant), a.ApprovalRequestID, domain.RejectRequestBody{Note: "withdraw"})
	require.ErrorIs(t, err, registry.ErrSelfApproval)

	got, err := svc.RejectRequest(approverCtx(orgTenant), a.ApprovalRequestID, domain.RejectRequestBody{Note: "not agreed"})
	require.NoError(t, err)
	assert.Equal(t, domain.ApprovalRejected, got.Status)
	require.NotNil(t, got.DecidedByPrincipalID)
	assert.Equal(t, approverPrincipal, *got.DecidedByPrincipalID)

	_, err = svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrApprovalNotPending)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)
}

// ---------------------------------------------------------------------------
// Gap 2 — tenant creation is maker-checker
// ---------------------------------------------------------------------------

func provisionTenant(t *testing.T, svc *registry.Service, code string) *domain.Tenant {
	t.Helper()
	tn, err := svc.ProvisionTenant(authCtx(), domain.ProvisionTenantRequest{
		TenantCode: code, LegalName: "Tenant " + code, DefaultCurrencyCode: "GBP",
		ExternalCustomerKey: "ck-" + code,
		PrimaryTimezone:     "Europe/London", PrimaryLocale: "en-GB",
	}, "corr-"+code)
	require.NoError(t, err)
	return tn
}

func activate(svc *registry.Service, tenantID string) error {
	_, err := svc.ExecuteTenantCommand(tenantCtx(tenantID), tenantID, domain.TenantCommandActivate,
		domain.ExecuteTenantCommandRequest{Reason: "go live"})
	return err
}

func TestTenantCreation_CannotGoLiveWithoutASecondPrincipal(t *testing.T) {
	svc, ms := baseSvc(t)
	tn := provisionTenant(t, svc, "MC1")
	require.NotNil(t, tn.CreationApprovalRequestID, "provisioning files the creation approval")
	a := ms.approvals()[*tn.CreationApprovalRequestID]
	assert.Equal(t, domain.ApprovalSubjectTenantCreation, a.SubjectType)
	assert.Equal(t, testPrincipal, a.RequestedByPrincipalID)

	// The creator alone cannot activate…
	require.ErrorIs(t, activate(svc, tn.TenantID), registry.ErrApprovalRequired)
	assert.Equal(t, domain.TenantLifecycleOnboarding, ms.tenants[tn.TenantID].LifecycleState)

	// …nor approve their own creation.
	_, err := svc.ApproveRequest(tenantCtx(tn.TenantID), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	require.ErrorIs(t, activate(svc, tn.TenantID), registry.ErrApprovalRequired)

	approveByID(t, svc, tn.TenantID, a.ApprovalRequestID)
	require.NoError(t, activate(svc, tn.TenantID))
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[tn.TenantID].LifecycleState)
}

func TestTenantCreation_GateFilesARequestForATenantThatHasNone(t *testing.T) {
	svc, ms := baseSvc(t)
	// A tenant provisioned before migration 000007: ONBOARDING, no request.
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleOnboarding
	ms.tenants[orgTenant].CreatedByPrincipalID = "original-creator"

	require.ErrorIs(t, activate(svc, orgTenant), registry.ErrApprovalRequired)
	filed, err := ms.LatestApprovalForSubject(context.Background(), domain.ApprovalSubjectTenantCreation, orgTenant)
	require.NoError(t, err)
	require.NotNil(t, filed, "the gate must not leave the tenant permanently unactivatable")
	assert.Equal(t, domain.ApprovalPending, filed.Status)
	assert.Equal(t, "original-creator", filed.RequestedByPrincipalID,
		"the maker of a creation is its creator, not whoever tried to activate it")
}

func TestTenantCreation_RejectedCreationKeepsTheTenantOnboarding(t *testing.T) {
	svc, ms := baseSvc(t)
	tn := provisionTenant(t, svc, "MC2")
	_, err := svc.RejectRequest(approverCtx(tn.TenantID), *tn.CreationApprovalRequestID,
		domain.RejectRequestBody{Note: "unknown customer"})
	require.NoError(t, err)

	require.ErrorIs(t, activate(svc, tn.TenantID), registry.ErrApprovalRequired)
	assert.Equal(t, domain.TenantLifecycleOnboarding, ms.tenants[tn.TenantID].LifecycleState)
}

// ---------------------------------------------------------------------------
// Doors around the named commands
// ---------------------------------------------------------------------------

func TestGenericLifecycleRoute_CannotTerminateWithoutApproval(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive

	err := svc.TransitionTenantLifecycle(tenantCtx(orgTenant), orgTenant,
		domain.TransitionTenantLifecycleRequest{TargetState: domain.TenantLifecycleOffboarding})
	require.ErrorIs(t, err, registry.ErrApprovalRequired)
	assert.Equal(t, domain.TenantLifecycleActive, ms.tenants[orgTenant].LifecycleState)
}

func TestUpdateEntity_LegalNameIsNotPatchable(t *testing.T) {
	svc, ms := baseSvc(t)
	seedEntityFor(ms, "ent-patch", orgTenant, "", "JUR-US")
	name := "Quietly Renamed Ltd"

	_, err := svc.UpdateEntity(tenantCtx(orgTenant), "ent-patch", domain.UpdateEntityRequest{LegalName: &name})
	require.ErrorIs(t, err, registry.ErrApprovalRequired)
	assert.Equal(t, "Original Name Ltd", ms.entities["ent-patch"].LegalName)
}

// ---------------------------------------------------------------------------
// Gap 4 — conflict resolution has SoD
// ---------------------------------------------------------------------------

func TestConflictResolution_NoSelfApprovalOfMerge(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.org().conflicts["c-sod"] = &domain.EntityRegistryConflict{
		ConflictID: "c-sod", TenantID: orgTenant, Status: domain.RegistryConflictOpen,
		DetectedByPrincipalID: "claimant",
	}
	c := ms.org().conflicts["c-sod"]

	err := svc.ResolveRegistryConflict(tenantCtx(orgTenant), "c-sod", domain.ResolveRegistryConflictRequest{
		Status: domain.RegistryConflictResolvedDuplicate, ResolutionNote: "same company",
	})
	a := pendingOf(t, err)
	assert.Equal(t, domain.RegistryConflictOpen, c.Status, "filed, not resolved")

	// The resolver cannot approve their own resolution.
	_, err = svc.ApproveRequest(tenantCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)

	// Nor can the principal whose claim was quarantined.
	claimant := domain.WithTenant(domain.WithPrincipal(context.Background(), "claimant"), orgTenant)
	_, err = svc.ApproveRequest(claimant, a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	assert.Equal(t, domain.RegistryConflictOpen, c.Status)
	assert.Equal(t, domain.ApprovalPending, ms.approvals()[a.ApprovalRequestID].Status)

	approveByID(t, svc, orgTenant, a.ApprovalRequestID)
	assert.Equal(t, domain.RegistryConflictResolvedDuplicate, c.Status)
	assert.Equal(t, testPrincipal, *c.ResolvedByPrincipalID)
	assert.Equal(t, approverPrincipal, *c.ApprovedByPrincipalID)
}

// ---------------------------------------------------------------------------
// Legacy mode — the migration aid, and only when asked for
// ---------------------------------------------------------------------------

func TestLegacyMode_RestoresTheBodyApproverOnlyWhenConfigured(t *testing.T) {
	svc, ms := baseSvc(t)
	svc.ConfigureMakerChecker(true, 0)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive

	res, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant, domain.TenantCommandInitiateTermination,
		domain.ExecuteTenantCommandRequest{Reason: "r", ApprovedByPrincipalID: "approver-1"})
	require.NoError(t, err)
	assert.Equal(t, domain.TenantLifecycleOffboarding, res.ToState)

	// Even in legacy mode, naming yourself is refused.
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	_, err = svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant, domain.TenantCommandInitiateTermination,
		domain.ExecuteTenantCommandRequest{Reason: "r", ApprovedByPrincipalID: testPrincipal})
	require.ErrorIs(t, err, registry.ErrApprovalRequired)
}

// The memstore mirrors ar_no_self_decision; this pins that the mirror is real,
// so the service-level tests above cannot pass on a store that would accept a
// self-decision. The Postgres CHECK itself is asserted in the store tests.
func TestApprovalStore_RefusesASelfDecisionEvenIfCalledDirectly(t *testing.T) {
	svc, ms := baseSvc(t)
	a := proposeTermination(t, svc, ms)
	err := ms.DecideApprovalRequest(context.Background(), domain.ApprovalDecision{
		ApprovalRequestID: a.ApprovalRequestID, TenantID: orgTenant,
		DecidedByPrincipalID: a.RequestedByPrincipalID,
	}, domain.ApprovalApproved)
	require.ErrorIs(t, err, errSelfDecisionCheck)
}
