package store_test

// Store tests for migration 000008, against a real Postgres: the onboarding
// key really prevents a duplicate tenant, the schema refuses what the service
// refuses (malformed LEIs, self-verification, self-approved merges), and the
// compensating cleanup and merge lineage commit with the change they belong to.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

func keyedTenant(key, code string) (*domain.Tenant, *domain.DataResidencyPolicy) {
	tid, pid := uuid.New().String(), uuid.New().String()
	k := key
	t := &domain.Tenant{
		TenantID: tid, TenantCode: code, LegalName: "Keyed " + code, Status: domain.TenantStatusActive,
		DefaultCurrencyCode: "GBP", PrimaryTimezone: "UTC", PrimaryLocale: "en-GB",
		DefaultDataResidencyPolicyID: pid, LifecycleState: domain.TenantLifecycleOnboarding,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "p-maker",
		ExternalCustomerKey:     &k,
		ProvisioningFingerprint: domain.ProvisioningFingerprint(domain.ProvisionTenantRequest{TenantCode: code}),
	}
	p := &domain.DataResidencyPolicy{
		DataResidencyPolicyID: pid, TenantID: tid, PolicyName: "d", PolicyCode: "D-" + code,
		ResidencyMode: domain.ResidencyModePreferredRegion, ConflictResolutionMode: domain.ConflictResolutionFailClosed,
		ActiveFlag: true, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "p-maker",
	}
	return t, p
}

func TestOnboardingKeyStore_ReplayIsRefusedOnTheKeyNotTheCode(t *testing.T) {
	f := newORGFixture(t)
	key := "ck-" + uuid.New().String()[:8]
	t1, p1 := keyedTenant(key, "K"+uuid.New().String()[:6])
	require.NoError(t, f.s.CreateTenantWithDefaultResidencyPolicy(domain.WithTenant(context.Background(), t1.TenantID), t1, p1))

	// Same key, same tenant_code: a genuine retry. It must fail on the KEY,
	// so the service can answer "already done" rather than "code taken".
	t2, p2 := keyedTenant(key, t1.TenantCode)
	err := f.s.CreateTenantWithDefaultResidencyPolicy(domain.WithTenant(context.Background(), t2.TenantID), t2, p2)
	require.ErrorIs(t, err, registry.ErrOnboardingKeyExists)

	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenants WHERE external_customer_key = $1`, key).Scan(&n))
	assert.Equal(t, 1, n, "no second tenant was written")

	gotTenant, gotFP, err := f.s.ResolveOnboardingKey(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, t1.TenantID, gotTenant)
	assert.Equal(t, t1.ProvisioningFingerprint, gotFP)

	stored, err := f.s.GetTenantByID(domain.WithTenant(context.Background(), t1.TenantID), t1.TenantID)
	require.NoError(t, err)
	require.NotNil(t, stored.ExternalCustomerKey)
	assert.Equal(t, key, *stored.ExternalCustomerKey)
}

func TestProvisioningStore_FailRetryAndAbandon(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	t1, p1 := keyedTenant("ck-"+uuid.New().String()[:8], "F"+uuid.New().String()[:6])
	tctx := domain.WithTenant(ctx, t1.TenantID)
	require.NoError(t, f.s.CreateTenantWithDefaultResidencyPolicy(tctx, t1, p1))

	require.NoError(t, f.s.MarkProvisioningFailed(tctx, t1.TenantID, "approval store down", "p-maker"))
	got, _ := f.s.GetTenantByID(tctx, t1.TenantID)
	assert.Equal(t, domain.TenantLifecycleFailedProvisioning, got.LifecycleState)
	require.NotNil(t, got.ProvisioningFailureReason)
	// Only from ONBOARDING.
	require.ErrorIs(t, f.s.MarkProvisioningFailed(tctx, t1.TenantID, "again", "p-maker"), registry.ErrConflict)

	// Retry: back to ONBOARDING, guarded by version.
	require.ErrorIs(t, f.s.CompleteProvisioning(tctx, registry.ProvisioningCompletion{
		TenantID: t1.TenantID, FromFailed: true, ExpectedVersion: 99, ActorID: "p-op"}), registry.ErrConflict)
	require.NoError(t, f.s.CompleteProvisioning(tctx, registry.ProvisioningCompletion{
		TenantID: t1.TenantID, FromFailed: true, ExpectedVersion: got.RecordVersion, ActorID: "p-op", Reason: "retry"}))
	got, _ = f.s.GetTenantByID(tctx, t1.TenantID)
	assert.Equal(t, domain.TenantLifecycleOnboarding, got.LifecycleState)
	assert.Nil(t, got.ProvisioningFailureReason)

	// Fail again, then abandon: host bindings and policies deactivated in
	// the same transaction as TERMINATED.
	require.NoError(t, f.s.MarkProvisioningFailed(tctx, t1.TenantID, "again", "p-maker"))
	require.NoError(t, f.s.BindTenantHost(tctx, &domain.TenantHostBinding{
		HostBindingID: uuid.New().String(), TenantID: t1.TenantID, Hostname: "abandon-" + t1.TenantCode + ".example",
		IsPrimary: true, ActiveFlag: true, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "p-op",
	}))
	got, _ = f.s.GetTenantByID(tctx, t1.TenantID)
	_, err := f.s.ExecuteTenantCommand(tctx, registry.TenantCommandParams{
		TenantID: t1.TenantID, Command: domain.TenantCommandAbandonProvisioning,
		TargetState:     domain.TenantLifecycleTerminated,
		AllowedFrom:     []domain.TenantLifecycleState{domain.TenantLifecycleFailedProvisioning},
		ExpectedVersion: got.RecordVersion, Reason: "withdrawn", ActorID: "p-maker", ApprovedBy: "p-approver",
	}, nil)
	require.NoError(t, err)
	var activeBindings, activePolicies int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM tenant_host_bindings WHERE tenant_id=$1 AND active_flag`, t1.TenantID).Scan(&activeBindings))
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM data_residency_policies WHERE tenant_id=$1 AND active_flag`, t1.TenantID).Scan(&activePolicies))
	assert.Zero(t, activeBindings)
	assert.Zero(t, activePolicies)
	var rows int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM data_residency_policies WHERE tenant_id=$1`, t1.TenantID).Scan(&rows))
	assert.Equal(t, 1, rows, "retained, not deleted")
}

func TestLEIStore_SchemaRefusesMalformedOrBareLEIs(t *testing.T) {
	f := newORGFixture(t)
	insert := func(lei, source, status any) error {
		_, err := f.pool.Exec(context.Background(), `
			INSERT INTO legal_entity_profile_versions (
				profile_version_id, tenant_id, legal_entity_id, version_number, legal_name,
				effective_from, change_reason, created_by_principal_id, lei, lei_source, lei_status)
			VALUES ($1, $2, $3, (SELECT COALESCE(MAX(version_number),0)+1 FROM legal_entity_profile_versions WHERE legal_entity_id=$3),
			        'X', NOW(), 'AMENDMENT', 'p', $4, $5, $6)`,
			uuid.New().String(), f.tenantID, f.entityID, lei, source, status)
		return err
	}
	err := insert("not-an-lei", "GLEIF", "ISSUED")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lepv_lei_format")
	err = insert("5493001KJTIIGC8Y1R12", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lepv_lei_has_source_and_status")
	err = insert("5493001KJTIIGC8Y1R12", "GLEIF", "ACTIVE")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lepv_lei_status_known")
	require.NoError(t, insert("5493001KJTIIGC8Y1R12", "GLEIF", "ISSUED"))

	versions, err := f.s.ListEntityProfileVersions(f.ctx, f.entityID)
	require.NoError(t, err)
	require.NotNil(t, versions[0].LEI)
	assert.Equal(t, "5493001KJTIIGC8Y1R12", *versions[0].LEI)
}

func draftEntityInStore(t *testing.T, f *orgFixture, regNo string) string {
	t.Helper()
	id := uuid.New().String()
	require.NoError(t, f.s.CreateEntity(f.ctx, &domain.LegalEntity{
		LegalEntityID: id, TenantID: f.tenantID, EntityCode: "DR-" + id[:6], LegalName: "Draft Ltd",
		RegistrationNumber: &regNo, EntityType: domain.EntityTypeSubsidiary, DefaultCurrencyCode: "USD",
		FiscalCalendarID: uuid.New().String(), EntityStatus: domain.EntityStatusDraft,
		PrimaryJurisdictionID: uuid.New().String(), DataResidencyPolicyID: f.policyID,
		RecordVersion: 1, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: pMaker,
	}))
	return id
}

func TestVerificationStore_IndependentVerifierThenActivation(t *testing.T) {
	f := newORGFixture(t)
	id := draftEntityInStore(t, f, "RC-DR-"+uuid.New().String()[:6])

	// ActivateLegalEntity from DRAFT: refused.
	require.ErrorIs(t, f.s.ActivateLegalEntity(f.ctx, id, "p", 1, nil), registry.ErrConflict)

	// The creator verifying: refused by le_no_self_verification, approval rolled back.
	a := fileApproval(t, f, domain.ApprovalSubjectLegalEntityVerification, id, "VerifyLegalEntity", 1,
		domain.RequestEntityVerificationRequest{VerificationEvidenceRef: "x"})
	a.RequestedByPrincipalID = "p-clerk"
	_, err := f.pool.Exec(context.Background(), `UPDATE approval_requests SET requested_by_principal_id='p-clerk' WHERE approval_request_id=$1`, a.ApprovalRequestID)
	require.NoError(t, err)
	err = f.s.VerifyLegalEntity(f.ctx, registry.EntityVerification{
		LegalEntityID: id, VerifiedBy: pMaker, EvidenceRef: "x", ExpectedVersion: 1,
		Decision: domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pMaker},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "le_no_self_verification")
	assert.Equal(t, domain.ApprovalPending, approvalStatus(t, f, a.ApprovalRequestID))

	require.NoError(t, f.s.VerifyLegalEntity(f.ctx, registry.EntityVerification{
		LegalEntityID: id, VerifiedBy: pApprover, EvidenceRef: "registry-extract", ExpectedVersion: 1,
		Decision: domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover},
	}, nil))
	e, err := f.s.GetEntityByID(f.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, domain.EntityStatusVerified, e.EntityStatus)
	assert.Equal(t, pApprover, *e.VerifiedByPrincipalID)

	require.NoError(t, f.s.ActivateLegalEntity(f.ctx, id, "p-op", e.RecordVersion, nil))
	e, _ = f.s.GetEntityByID(f.ctx, id)
	assert.Equal(t, domain.EntityStatusActive, e.EntityStatus)
}

func TestRegistryProbe_ADraftIsAClaim(t *testing.T) {
	f := newORGFixture(t)
	reg := "RC-CLAIM-" + uuid.New().String()[:6]
	id := uuid.New().String()
	jur := uuid.New().String()
	require.NoError(t, f.s.CreateEntity(f.ctx, &domain.LegalEntity{
		LegalEntityID: id, TenantID: f.tenantID, EntityCode: "C-" + id[:6], LegalName: "Claim Ltd",
		RegistrationNumber: &reg, EntityType: domain.EntityTypeSubsidiary, DefaultCurrencyCode: "USD",
		FiscalCalendarID: uuid.New().String(), EntityStatus: domain.EntityStatusDraft,
		PrimaryJurisdictionID: jur, DataResidencyPolicyID: f.policyID,
		RecordVersion: 1, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: pMaker,
	}))
	got, err := f.s.FindActiveEntityByRegistry(f.ctx, reg, jur)
	require.NoError(t, err)
	require.NotNil(t, got, "a DRAFT holding a registry number blocks a second claim to it")
	assert.Equal(t, id, got.LegalEntityID)
}

func TestMergeStore_NonDestructiveLineageAndSchemaSoD(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	survivor := f.entityID
	dup := draftEntityInStore(t, f, "RC-MG-"+uuid.New().String()[:6])
	_, err := f.pool.Exec(ctx, `UPDATE legal_entities SET entity_status='ACTIVE' WHERE legal_entity_id=$1`, dup)
	require.NoError(t, err)

	a := fileApproval(t, f, domain.ApprovalSubjectLegalEntityMerge, dup, "MergeDuplicateCandidate", 1,
		domain.MergeDuplicateCandidateRequest{SurvivorLegalEntityID: survivor, Reason: "same"})
	rec := func(approver string) *domain.EntityMergeRecord {
		return &domain.EntityMergeRecord{
			MergeRecordID: uuid.New().String(), TenantID: f.tenantID,
			DuplicateLegalEntityID: dup, SurvivorLegalEntityID: survivor, Reason: "same",
			MergedByPrincipalID: pMaker, MergeApprovedByPrincipalID: approver, MergeApprovalRequestID: a.ApprovalRequestID,
		}
	}
	// emr_no_self_approval; the duplicate is left exactly as it was.
	err = f.s.MergeEntities(f.ctx, rec(pMaker),
		domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: "p-other"}, 1, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "emr_no_self_approval")
	e, _ := f.s.GetEntityByID(f.ctx, dup)
	assert.Equal(t, domain.EntityStatusActive, e.EntityStatus)
	assert.Nil(t, e.MergedIntoLegalEntityID)

	require.NoError(t, f.s.MergeEntities(f.ctx, rec(pApprover),
		domain.ApprovalDecision{ApprovalRequestID: a.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover}, 1, nil))
	e, _ = f.s.GetEntityByID(f.ctx, dup)
	assert.Equal(t, domain.EntityStatusDormant, e.EntityStatus)
	require.NotNil(t, e.MergedIntoLegalEntityID)
	assert.Equal(t, survivor, *e.MergedIntoLegalEntityID)

	u := fileApproval(t, f, domain.ApprovalSubjectLegalEntityUnmerge, dup, "UnmergeEntity", e.RecordVersion,
		domain.UnmergeEntityRequest{Reason: "wrong"})
	require.NoError(t, f.s.UnmergeEntity(f.ctx, dup, pMaker, "wrong",
		domain.ApprovalDecision{ApprovalRequestID: u.ApprovalRequestID, TenantID: f.tenantID, DecidedByPrincipalID: pApprover},
		e.RecordVersion, nil))
	e, _ = f.s.GetEntityByID(f.ctx, dup)
	assert.Equal(t, domain.EntityStatusActive, e.EntityStatus, "restored to its status before the merge")
	assert.Nil(t, e.MergedIntoLegalEntityID)

	recs, err := f.s.ListEntityMergeRecords(f.ctx, survivor)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NotNil(t, recs[0].UnmergedAt)
	assert.Equal(t, pApprover, *recs[0].UnmergeApprovedByPrincipalID)

	// The self-merge CHECK.
	_, err = f.pool.Exec(ctx, `UPDATE legal_entities SET merged_into_legal_entity_id = legal_entity_id WHERE legal_entity_id=$1`, dup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "le_not_merged_into_self")
}
