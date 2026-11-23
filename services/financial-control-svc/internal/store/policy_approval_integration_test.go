//go:build integration

// Real-Postgres tests for SPEC_DEVIATIONS A4: tolerance and materiality
// policies are created pending and approved through their own,
// independently-authorized command — mirroring control_rule_versions'
// already-governed ApproveRuleVersion (see TestRuleVersion_ApprovalRules).
//
// Run: go test -v -tags=integration -count=1 -timeout=180s ./internal/store/
package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

func TestTolerancePolicy_ApprovalRules(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	p, err := testStore.CreateTolerancePolicy(ctx, tenant, "maker", domain.CreateTolerancePolicyRequest{
		LegalEntityID: entity, Metric: "FIN-CTRL-001", AbsoluteTolerance: "0.01", Currency: "USD",
		Rationale: "cent rounding", EffectiveFrom: "2026-01-01"})
	require.NoError(t, err)
	assert.Nil(t, p.ApprovedBy, "created pending: no caller-declared approver (A4)")

	_, err = testStore.ApproveTolerancePolicy(ctx, tenant, entity, p.ToleranceID, "maker")
	assert.ErrorIs(t, err, domain.ErrSelfApproval, "the creator cannot approve their own policy")

	approved, err := testStore.ApproveTolerancePolicy(ctx, tenant, entity, p.ToleranceID, "checker")
	require.NoError(t, err)
	require.NotNil(t, approved.ApprovedBy)
	assert.Equal(t, "checker", *approved.ApprovedBy)
	assert.NotNil(t, approved.ApprovedAt)

	_, err = testStore.ApproveTolerancePolicy(ctx, tenant, entity, p.ToleranceID, "another")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition, "an approved policy cannot be re-approved")

	_, err = testPool.Exec(ctx, `UPDATE tolerance_policies SET absolute_tolerance = 999 WHERE tolerance_id = $1`, p.ToleranceID)
	assert.Error(t, err, "approved tolerance rows are immutable (Invariant 4)")

	_, err = testStore.ApproveTolerancePolicy(ctx, tenant, entity, uuid.NewString(), "checker")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMaterialityPolicy_ApprovalRules(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	p, err := testStore.CreateMaterialityPolicy(ctx, tenant, "maker", domain.CreateMaterialityPolicyRequest{
		LegalEntityID: entity, ReportingBasis: "US_GAAP", AmountThreshold: "1000", AggregateThreshold: "5000",
		Currency: "USD", EffectiveFrom: "2026-01-01"})
	require.NoError(t, err)
	assert.Nil(t, p.ApprovedBy)

	_, err = testStore.ApproveMaterialityPolicy(ctx, tenant, entity, p.MaterialityID, "maker")
	assert.ErrorIs(t, err, domain.ErrSelfApproval)

	approved, err := testStore.ApproveMaterialityPolicy(ctx, tenant, entity, p.MaterialityID, "cfo")
	require.NoError(t, err)
	require.NotNil(t, approved.ApprovedBy)
	assert.Equal(t, "cfo", *approved.ApprovedBy)

	_, err = testStore.ApproveMaterialityPolicy(ctx, tenant, entity, p.MaterialityID, "another")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition)
}

// TestRun_OnlyApprovedPolicyIsEverResolved is the correctness property A4 exists
// for: a pending tolerance/materiality must never be pinned to a run, whether
// named explicitly or resolved as "latest effective" — only CreateRun's rule
// version resolution enforced this before; policies did not.
func TestRun_OnlyApprovedPolicyIsEverResolved(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")

	pendingTol, err := testStore.CreateTolerancePolicy(ctx, tenant, "maker", domain.CreateTolerancePolicyRequest{
		LegalEntityID: entity, Metric: "FIN-CTRL-001", AbsoluteTolerance: "0.01", Currency: "USD",
		Rationale: "r", EffectiveFrom: "2026-01-01"})
	require.NoError(t, err)

	// Fallback resolution (no explicit tolerance_id): a pending-only policy
	// resolves to none, same as if it never existed.
	run, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "fallback-pending", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	assert.Nil(t, run.ToleranceID, "a pending tolerance must never be resolved onto a run")

	// Named resolution (explicit tolerance_id): refused outright, not silently ignored.
	req := runReq(d.ControlDefinitionID, entity)
	req.ToleranceID = pendingTol.ToleranceID
	_, _, err = testStore.CreateRun(ctx, tenant, "prep", "c", "named-pending", req)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "naming a pending tolerance must be refused, not silently widened to none")

	// Once approved, both paths resolve it.
	_, err = testStore.ApproveTolerancePolicy(ctx, tenant, entity, pendingTol.ToleranceID, "checker")
	require.NoError(t, err)

	run2, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "fallback-approved", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	require.NotNil(t, run2.ToleranceID)
	assert.Equal(t, pendingTol.ToleranceID, *run2.ToleranceID)

	req2 := runReq(d.ControlDefinitionID, entity)
	req2.ToleranceID = pendingTol.ToleranceID
	run3, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "named-approved", req2)
	require.NoError(t, err)
	require.NotNil(t, run3.ToleranceID)
	assert.Equal(t, pendingTol.ToleranceID, *run3.ToleranceID)
}
