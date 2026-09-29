package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ORG-02 §4.2 "home-region changes require maker-checker" — the control the
// 23 Sep 2026 audit could not score because no command existed.

const newRegion = "0a000000-0000-4000-8000-000000000001"

func homeRegionReq() domain.ChangeHomeRegionRequest {
	return domain.ChangeHomeRegionRequest{
		ResidencyRegionID:     newRegion,
		HomeRegionDecisionRef: "RES-DECISION-42",
		Reason:                "EU data-residency commitment",
	}
}

func TestChangeHomeRegion_IsFiledNeverApplied(t *testing.T) {
	svc, ms := baseSvc(t)
	before := ms.tenants[orgTenant].RecordVersion

	err := svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, homeRegionReq())
	a := pendingOf(t, err)

	assert.Equal(t, domain.ApprovalSubjectTenantHomeRegion, a.SubjectType)
	assert.Equal(t, before, ms.tenants[orgTenant].RecordVersion, "nothing changes until a second principal approves")
}

func TestChangeHomeRegion_MakerCannotApproveOwnChange(t *testing.T) {
	svc, _ := baseSvc(t)
	a := pendingOf(t, svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, homeRegionReq()))

	_, err := svc.ApproveRequest(tenantCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.ErrorIs(t, err, registry.ErrSelfApproval)
}

func TestChangeHomeRegion_ApprovalAppliesItWithEvidence(t *testing.T) {
	svc, ms := baseSvc(t)
	before := ms.tenants[orgTenant].RecordVersion
	a := pendingOf(t, svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, homeRegionReq()))

	approveByID(t, svc, orgTenant, a.ApprovalRequestID)

	assert.Equal(t, before+1, ms.tenants[orgTenant].RecordVersion)
	history, err := svc.ListTenantLifecycleHistory(tenantCtx(orgTenant), orgTenant)
	require.NoError(t, err)
	require.NotEmpty(t, history)
	last := history[len(history)-1]
	assert.Equal(t, domain.TenantCommandChangeHomeRegion, last.CommandName)
	require.NotNil(t, last.HomeRegionDecisionRef)
	assert.Equal(t, "RES-DECISION-42", *last.HomeRegionDecisionRef, "§4.2 evidence: the home-region decision")
	require.NotNil(t, last.ApprovedByPrincipalID)
	assert.NotEqual(t, last.ActorPrincipalID, *last.ApprovedByPrincipalID)
}

func TestChangeHomeRegion_IsAPlatformPermission(t *testing.T) {
	ms := newMemStore()
	rec := &recordingAuthZ{}
	svc := newSvc(t, ms, rec, acceptAllJurisd{})

	_ = svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, homeRegionReq())
	assert.Equal(t, testPlatformScope, rec.scopeID,
		"a hard isolation identifier: a tenant admin's own-tenant grant must not suffice")
	assert.Equal(t, "home-region.change", rec.action)
}

func TestChangeHomeRegion_RequiresTheDecisionEvidence(t *testing.T) {
	svc, _ := baseSvc(t)
	req := homeRegionReq()
	req.HomeRegionDecisionRef = ""
	err := svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, req)
	require.ErrorIs(t, err, registry.ErrSourceUnverified)
}

func TestChangeHomeRegion_RefusedForATerminatedTenantAndAStaleVersion(t *testing.T) {
	svc, ms := baseSvc(t)
	req := homeRegionReq()
	req.ExpectedVersion = ms.tenants[orgTenant].RecordVersion + 5
	require.ErrorIs(t, svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, req), registry.ErrVersionConflict)

	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleTerminated
	require.ErrorIs(t, svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, homeRegionReq()), registry.ErrInvalidTransition)
}
