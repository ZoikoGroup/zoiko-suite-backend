package store_test

// Approval-request store tests, against a real Postgres.
//
// The service tests prove the service refuses self-approval. These prove the
// DATABASE refuses it too — ar_no_self_decision, lepv_no_self_approval,
// erc_no_self_approval — so a future caller that skips the service still
// cannot write one; and that an approval and the command it releases commit
// or roll back together.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	pMaker    = "p-maker"
	pApprover = "p-approver"
)

// fileApproval stores a PENDING request for subject, as the service would.
func fileApproval(t *testing.T, f *orgFixture, st domain.ApprovalSubjectType, subjectID, command string, version int64, payload any) *domain.ApprovalRequest {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	fp, err := domain.ApprovalFingerprint(st, subjectID, command, version, payload)
	require.NoError(t, err)
	now := time.Now().UTC()
	a := &domain.ApprovalRequest{
		ApprovalRequestID: uuid.New().String(), TenantID: f.tenantID,
		SubjectType: st, SubjectID: subjectID, CommandName: command,
		Payload: raw, ExpectedVersion: version, PayloadFingerprint: fp, Reason: "r",
		RequestedByPrincipalID: pMaker, RequestedAt: now, ExpiresAt: now.Add(time.Hour),
		Status: domain.ApprovalPending,
	}
	require.NoError(t, f.s.CreateApprovalRequest(f.ctx, a))
	return a
}

func approvalStatus(t *testing.T, f *orgFixture, id string) domain.ApprovalStatus {
	t.Helper()
	a, err := f.s.GetApprovalRequest(f.ctx, id)
	require.NoError(t, err)
	require.NotNil(t, a)
	return a.Status
}

func terminationParams(f *orgFixture, version int64, d *domain.ApprovalDecision) registry.TenantCommandParams {
	return registry.TenantCommandParams{
		TenantID: f.tenantID, Command: domain.TenantCommandInitiateTermination,
		TargetState:     domain.TenantLifecycleOffboarding,
		AllowedFrom:     []domain.TenantLifecycleState{domain.TenantLifecycleActive, domain.TenantLifecycleSuspended},
		ExpectedVersion: version, Reason: "contract ended",
		ActorID: pMaker, ApprovedBy: pApprover, Approval: d,
	}
}

// The control itself, in the schema: whatever the service does, the database
// will not record a request decided by the principal who made it.
func TestApprovalStore_DatabaseRefusesSelfDecision(t *testing.T) {
	f := newORGFixture(t)
	a := fileApproval(t, f, domain.ApprovalSubjectTenantCommand, f.tenantID, "InitiateTermination", 1,
		domain.ExecuteTenantCommandRequest{Reason: "r"})

	_, err := f.pool.Exec(context.Background(), `
		UPDATE approval_requests
		   SET status = 'APPROVED', decided_by_principal_id = requested_by_principal_id, decided_at = NOW()
		 WHERE approval_request_id = $1`, a.ApprovalRequestID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ar_no_self_decision")

	// And through the store's own decision path.
	err = f.s.DecideApprovalRequest(f.ctx, domain.ApprovalDecision{
		ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pMaker,
	}, domain.ApprovalApproved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ar_no_self_decision")
	assert.Equal(t, domain.ApprovalPending, approvalStatus(t, f, a.ApprovalRequestID))

	// An APPROVED with no decider at all is refused too.
	_, err = f.pool.Exec(context.Background(), `
		UPDATE approval_requests SET status = 'APPROVED', decided_at = NOW()
		 WHERE approval_request_id = $1`, a.ApprovalRequestID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ar_decision_has_decider")
}

func TestApprovalStore_ApprovalAndCommandCommitTogether(t *testing.T) {
	f := newORGFixture(t)
	a := fileApproval(t, f, domain.ApprovalSubjectTenantCommand, f.tenantID, "InitiateTermination", 1,
		domain.ExecuteTenantCommandRequest{Reason: "contract ended"})
	d := &domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover}

	_, err := f.s.ExecuteTenantCommand(f.ctx, terminationParams(f, 1, d), nil)
	require.NoError(t, err)
	assert.Equal(t, domain.ApprovalApproved, approvalStatus(t, f, a.ApprovalRequestID))

	history, err := f.s.ListTenantLifecycleHistory(f.ctx, f.tenantID)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.NotNil(t, history[0].ApprovalRequestID)
	assert.Equal(t, a.ApprovalRequestID, *history[0].ApprovalRequestID, "the fact links to the approval that released it")
	assert.Equal(t, pApprover, *history[0].ApprovedByPrincipalID)
}

func TestApprovalStore_FailedCommandRollsTheApprovalBack(t *testing.T) {
	f := newORGFixture(t)
	a := fileApproval(t, f, domain.ApprovalSubjectTenantCommand, f.tenantID, "InitiateTermination", 1,
		domain.ExecuteTenantCommandRequest{Reason: "contract ended"})
	d := &domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover}

	// Stale version: the tenant UPDATE matches nothing.
	_, err := f.s.ExecuteTenantCommand(f.ctx, terminationParams(f, 99, d), nil)
	require.ErrorIs(t, err, registry.ErrConflict)
	assert.Equal(t, domain.ApprovalPending, approvalStatus(t, f, a.ApprovalRequestID),
		"an approval must never read APPROVED for a command that did not run")
	history, _ := f.s.ListTenantLifecycleHistory(f.ctx, f.tenantID)
	assert.Empty(t, history)
}

func TestApprovalStore_AnApprovalReleasesOneCommandOnce(t *testing.T) {
	f := newORGFixture(t)
	a := fileApproval(t, f, domain.ApprovalSubjectTenantCommand, f.tenantID, "InitiateTermination", 1,
		domain.ExecuteTenantCommandRequest{Reason: "r"})
	d := domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover}

	require.NoError(t, f.s.DecideApprovalRequest(f.ctx, d, domain.ApprovalRejected))
	require.ErrorIs(t, f.s.DecideApprovalRequest(f.ctx, d, domain.ApprovalApproved), registry.ErrApprovalNotPending)

	// A decided approval cannot release a command either.
	_, err := f.s.ExecuteTenantCommand(f.ctx, terminationParams(f, 1, &d), nil)
	require.ErrorIs(t, err, registry.ErrApprovalNotPending)
	tn, _ := f.s.GetTenantByID(f.ctx, f.tenantID)
	assert.Equal(t, domain.TenantLifecycleActive, tn.LifecycleState)
}

func TestApprovalStore_ExpiredRequestCannotBeApprovedAndDoesNotBlock(t *testing.T) {
	f := newORGFixture(t)
	a := fileApproval(t, f, domain.ApprovalSubjectTenantCommand, f.tenantID, "InitiateTermination", 1,
		domain.ExecuteTenantCommandRequest{Reason: "r"})
	_, err := f.pool.Exec(context.Background(), `
		UPDATE approval_requests
		   SET requested_at = NOW() - interval '2 hours', expires_at = NOW() - interval '1 hour'
		 WHERE approval_request_id = $1`, a.ApprovalRequestID)
	require.NoError(t, err)

	require.ErrorIs(t, f.s.DecideApprovalRequest(f.ctx, domain.ApprovalDecision{
		ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover,
	}, domain.ApprovalApproved), registry.ErrApprovalNotPending)

	pending, err := f.s.ListApprovalRequests(f.ctx, true)
	require.NoError(t, err)
	assert.Empty(t, pending, "an expired request is not awaiting anyone")

	// A fresh proposal retires it rather than colliding with it.
	b := fileApproval(t, f, domain.ApprovalSubjectTenantCommand, f.tenantID, "InitiateTermination", 1,
		domain.ExecuteTenantCommandRequest{Reason: "again"})
	assert.Equal(t, domain.ApprovalExpired, approvalStatus(t, f, a.ApprovalRequestID))
	assert.Equal(t, domain.ApprovalPending, approvalStatus(t, f, b.ApprovalRequestID))
}

func TestApprovalStore_OneLiveProposalPerSubject(t *testing.T) {
	f := newORGFixture(t)
	fileApproval(t, f, domain.ApprovalSubjectTenantCommand, f.tenantID, "InitiateTermination", 1,
		domain.ExecuteTenantCommandRequest{Reason: "r"})

	raw, _ := json.Marshal(domain.ExecuteTenantCommandRequest{Reason: "dup"})
	now := time.Now().UTC()
	err := f.s.CreateApprovalRequest(f.ctx, &domain.ApprovalRequest{
		ApprovalRequestID: uuid.New().String(), TenantID: f.tenantID,
		SubjectType: domain.ApprovalSubjectTenantCommand, SubjectID: f.tenantID,
		CommandName: "InitiateTermination", Payload: raw, ExpectedVersion: 1,
		PayloadFingerprint: "f", Reason: "dup", RequestedByPrincipalID: "p-other",
		RequestedAt: now, ExpiresAt: now.Add(time.Hour), Status: domain.ApprovalPending,
	})
	require.ErrorIs(t, err, registry.ErrApprovalPending)
}

// JSONB reorders object keys. The fingerprint is therefore computed over the
// re-marshalled typed struct, and must survive the round trip — otherwise
// every legitimate approval would be refused as tampered.
func TestApprovalStore_FingerprintSurvivesTheJSONBRoundTrip(t *testing.T) {
	f := newORGFixture(t)
	name := "Renamed Plc"
	reg := "RC-NEW"
	req := domain.AmendLegalProfileRequest{
		LegalName: &name, RegistrationNumber: &reg,
		EffectiveFrom:     time.Date(2026, 3, 31, 12, 30, 15, 123456789, time.UTC),
		ChangeReason:      domain.ProfileChangeLegalNameChange,
		SourceEvidenceRef: "filing/NM01", ExpectedVersion: 1,
	}
	a := fileApproval(t, f, domain.ApprovalSubjectLegalProfileAmendment, f.entityID, "ChangeLegalName", 1, req)

	got, err := f.s.GetApprovalRequest(f.ctx, a.ApprovalRequestID)
	require.NoError(t, err)
	var decoded domain.AmendLegalProfileRequest
	require.NoError(t, json.Unmarshal(got.Payload, &decoded))
	fp, err := domain.ApprovalFingerprint(got.SubjectType, got.SubjectID, got.CommandName, got.ExpectedVersion, decoded)
	require.NoError(t, err)
	assert.Equal(t, a.PayloadFingerprint, fp)
	assert.Equal(t, a.PayloadFingerprint, got.PayloadFingerprint)
}

func TestApprovalStore_ApprovedAmendmentLinksAndRefusesSelfApproval(t *testing.T) {
	f := newORGFixture(t)
	name := "Renamed Ltd"
	a := fileApproval(t, f, domain.ApprovalSubjectLegalProfileAmendment, f.entityID, "ChangeLegalName", 1,
		domain.AmendLegalProfileRequest{LegalName: &name})

	// lepv_no_self_approval: a version whose approver is its creator.
	self := pMaker
	_, err := f.s.AmendLegalProfile(f.ctx, f.entityID, &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.New().String(), TenantID: f.tenantID, LegalEntityID: f.entityID,
		LegalName: name, EffectiveFrom: time.Now().UTC().Add(-time.Minute),
		ChangeReason: domain.ProfileChangeLegalNameChange, CreatedByPrincipalID: pMaker,
		ApprovedByPrincipalID: &self,
	}, 1, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lepv_no_self_approval")

	approver := pApprover
	v, err := f.s.AmendLegalProfile(f.ctx, f.entityID, &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.New().String(), TenantID: f.tenantID, LegalEntityID: f.entityID,
		LegalName: name, EffectiveFrom: time.Now().UTC().Add(-time.Minute),
		ChangeReason: domain.ProfileChangeLegalNameChange, CreatedByPrincipalID: pMaker,
		ApprovedByPrincipalID: &approver, ApprovalRequestID: &a.ApprovalRequestID,
		Approval: &domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover},
	}, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, v.ApprovalRequestID)
	assert.Equal(t, a.ApprovalRequestID, *v.ApprovalRequestID)
	assert.Equal(t, domain.ApprovalApproved, approvalStatus(t, f, a.ApprovalRequestID))
}

func TestApprovalStore_ConflictResolutionApproverIsIndependentInTheSchema(t *testing.T) {
	f := newORGFixture(t)
	conflictID := uuid.New().String()
	require.NoError(t, f.s.RecordRegistryConflict(f.ctx, &domain.EntityRegistryConflict{
		ConflictID: conflictID, TenantID: f.tenantID,
		RegistrationNumber: "RC-DUP", JurisdictionID: uuid.New().String(),
		ExistingLegalEntityID: f.entityID, AttemptedPayload: map[string]any{},
		DetectedByPrincipalID: "p-claimant",
	}))
	a := fileApproval(t, f, domain.ApprovalSubjectRegistryConflictResolution, conflictID, "ResolveRegistryConflict", 0,
		domain.ResolveRegistryConflictRequest{Status: domain.RegistryConflictResolvedDuplicate, ResolutionNote: "same"})

	// The claimant approving: refused by erc_no_self_approval, and the
	// approval decision rolls back with it.
	err := f.s.ResolveRegistryConflictApproved(f.ctx, conflictID, domain.RegistryConflictResolvedDuplicate, "same", pMaker,
		domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: "p-claimant"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "erc_no_self_approval")
	assert.Equal(t, domain.ApprovalPending, approvalStatus(t, f, a.ApprovalRequestID))

	require.NoError(t, f.s.ResolveRegistryConflictApproved(f.ctx, conflictID, domain.RegistryConflictResolvedDuplicate, "same", pMaker,
		domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover}))
	c, err := f.s.GetRegistryConflict(f.ctx, conflictID)
	require.NoError(t, err)
	assert.Equal(t, domain.RegistryConflictResolvedDuplicate, c.Status)
	assert.Equal(t, pMaker, *c.ResolvedByPrincipalID)
	assert.Equal(t, pApprover, *c.ApprovedByPrincipalID)
	assert.Equal(t, domain.ApprovalApproved, approvalStatus(t, f, a.ApprovalRequestID))
}

func TestApprovalStore_AnotherTenantCannotReadAnApproval(t *testing.T) {
	f := newORGFixture(t)
	a := fileApproval(t, f, domain.ApprovalSubjectTenantCommand, f.tenantID, "InitiateTermination", 1,
		domain.ExecuteTenantCommandRequest{Reason: "r"})

	other := domain.WithTenant(context.Background(), uuid.New().String())
	got, err := f.s.GetApprovalRequest(other, a.ApprovalRequestID)
	require.NoError(t, err)
	assert.Nil(t, got)
	list, err := f.s.ListApprovalRequests(other, false)
	require.NoError(t, err)
	assert.Empty(t, list)
}
