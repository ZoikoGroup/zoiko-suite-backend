//go:build integration

package store_test

// SPEC_DEVIATIONS A2/B7: the ControlCertification record (Invariant 9).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/store"
)

// testAuthority is the authority snapshot the handler would obtain from
// authorization-svc for actor (a fresh access decision each time).
func testAuthority(actor, _ string) *domain.AuthoritySnapshot {
	return &domain.AuthoritySnapshot{
		Source: "authorization-svc", PrincipalID: actor, ActionType: "FINCTRL_CERTIFY",
		AccessDecisionID: uuid.NewString(), DecisionOutcome: "GRANTED", Decision: "PERMIT",
		DecisionBasis: "rbac:role=CONTROL_CERTIFIER", PolicySetVersion: "cfg.42", DecidedAt: time.Now().UTC(),
	}
}

func TestCertification_RecordedWithEveryInvariant9Field(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	_, final := w.run("FIN-CTRL-020", fiscal)
	auth := testAuthority("checker", w.tenant)
	auth.LegalEntityID = w.entity
	out, err := certifyRun(ctx, w.tenant, final.RunID, "checker", "corr-cert", final.Version,
		domain.CertifyRequest{Decision: "CERTIFY", Reason: "reviewed"}, auth)
	require.NoError(t, err)

	c, err := testStore.GetCertification(ctx, w.tenant, final.RunID)
	require.NoError(t, err)
	assert.Equal(t, final.RunID, c.RunID)
	assert.Equal(t, "checker", c.Certifier)
	assert.Equal(t, out.Version, c.RunVersion, "the run version as certified")
	assert.Equal(t, domain.ResultPass, c.Result)
	assert.Equal(t, "reviewed", c.Reason)
	assert.False(t, c.CertifiedAt.IsZero())

	// Authority: the decision exactly as given.
	assert.Equal(t, auth.AccessDecisionID, c.Authority.AccessDecisionID)
	assert.Equal(t, "rbac:role=CONTROL_CERTIFIER", c.Authority.DecisionBasis)
	assert.Equal(t, "cfg.42", c.Authority.PolicySetVersion)
	assert.Equal(t, w.entity, c.Authority.LegalEntityID)

	// Evidence package: the one sealed at execution, by id and digest.
	pkg, err := testStore.GetLatestEvidence(ctx, w.tenant, final.RunID)
	require.NoError(t, err)
	assert.Equal(t, pkg.PackageID, c.EvidencePackageID)
	assert.Equal(t, pkg.Digest, c.EvidenceDigest)

	// It records the certification transition that a revocation references.
	trs, err := testStore.ListTransitions(ctx, w.tenant, final.RunID)
	require.NoError(t, err)
	var certTr int64
	for _, tr := range trs {
		if tr.Dimension == "CERTIFICATION" && tr.ToState == "CERTIFIED" {
			certTr = tr.TransitionID
		}
	}
	assert.Equal(t, certTr, c.CertificationTransitionID)

	// A passing run with no exceptions accepts none.
	assert.Empty(t, c.UnresolvedExceptions)
	assert.Empty(t, c.AssessedExposure.Currencies)

	// Append-only.
	_, err = testPool.Exec(ctx, `UPDATE control_certifications SET certifier = 'someone else' WHERE run_id = $1`, final.RunID)
	assert.Error(t, err)
	_, err = testPool.Exec(ctx, `DELETE FROM control_certifications WHERE run_id = $1`, final.RunID)
	assert.Error(t, err)
}

// A REJECT decision records no certification, and a certification that cannot
// identify its authority is refused rather than recorded without one.
func TestCertification_RejectRecordsNothingAndAuthorityIsRequired(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	_, final := w.run("FIN-CTRL-020", fiscal)

	for name, a := range map[string]*domain.AuthoritySnapshot{
		"no authority":          nil,
		"no access decision id": {PrincipalID: "checker", DecisionOutcome: "GRANTED"},
		"someone else's":        testAuthority("other", w.tenant),
	} {
		_, err := certifyRun(ctx, w.tenant, final.RunID, "checker", "c", final.Version, domain.CertifyRequest{Decision: "CERTIFY"}, a)
		require.ErrorIs(t, err, domain.ErrAuthorizationServiceUnavailable, name)
	}
	got, err := testStore.GetRun(ctx, w.tenant, final.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleReadyToCertify, got.LifecycleState, "a refused certification changes nothing")

	_, err = certifyRun(ctx, w.tenant, final.RunID, "checker", "c", final.Version,
		domain.CertifyRequest{Decision: "REJECT", Reason: "evidence incomplete"}, nil)
	require.NoError(t, err)
	_, err = testStore.GetCertification(ctx, w.tenant, final.RunID)
	require.ErrorIs(t, err, domain.ErrNotFound, "a rejection is not a certification")
}

// The exceptions a certification accepts unresolved, with the authority behind each,
// and their exposure assessed against the run's pinned materiality policy.
func TestCertification_RecordsAcceptedExceptionsAndAssessedExposure(t *testing.T) {
	w := newWave5(t)
	mat, err := testStore.CreateMaterialityPolicy(ctx, w.tenant, "maker", domain.CreateMaterialityPolicyRequest{
		LegalEntityID: w.entity, ReportingBasis: "US_GAAP", AmountThreshold: "1000", AggregateThreshold: "300",
		Currency: "USD", EffectiveFrom: "2026-01-01", ApprovedBy: "cfo"})
	require.NoError(t, err)

	w.src.set(catalogue.SysGL, "control-account-postings", "wm", r("e1", "J-1", "200.10"), r("e2", "J-2", "150.00"))
	run, final := w.run("FIN-CTRL-021", func(r *domain.CreateRunRequest) {
		fiscal(r)
		r.MaterialityID = mat.MaterialityID
	})
	require.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 2)

	// Both exceptions waived under authority by someone other than their owner,
	// so the run rolls up to PASS_WITH_APPROVED_EXCEPTIONS and READY_TO_CERTIFY.
	waiveAll(t, w, run.RunID, ex)
	ready, err := testStore.GetRun(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	require.Equal(t, domain.LifecycleReadyToCertify, ready.LifecycleState)
	require.Equal(t, domain.ResultPassWithApprovedEx, ready.ResultState)

	_, err = certifyRun(ctx, w.tenant, run.RunID, "checker", "c", ready.Version,
		domain.CertifyRequest{Decision: "CERTIFY"}, testAuthority("checker", w.tenant))
	require.NoError(t, err)

	c, err := testStore.GetCertification(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.ResultPassWithApprovedEx, c.Result)
	require.Len(t, c.UnresolvedExceptions, 2)
	for _, e := range c.UnresolvedExceptions {
		assert.Equal(t, string(domain.ExWaived), e.State)
		assert.Equal(t, "waiver:memo-7", e.AuthorityRef, "the authority behind the waiver")
		assert.Contains(t, []string{"200.1", "150"}, e.Exposure, "exact decimal, canonical")
	}
	require.Len(t, c.AssessedExposure.Currencies, 1)
	assert.Equal(t, "350.1", c.AssessedExposure.Currencies[0].Exposure)
	require.NotNil(t, c.AssessedExposure.Materiality, "assessed against the run's pinned policy")
	assert.Equal(t, *ready.MaterialityID, c.AssessedExposure.Materiality.MaterialityID)
	require.NotNil(t, c.AssessedExposure.AggregateMaterial)
	assert.True(t, *c.AssessedExposure.AggregateMaterial, "350.1 >= 300")
}

// waiveAll assigns every exception to olivia and has sam, who is independent of
// her, waive each one under authority "waiver:memo-7".
func waiveAll(t *testing.T, w *wave2, runID string, list []domain.ControlException) {
	t.Helper()
	for i := range list {
		e := list[i]
		a, err := testStore.AssignException(ctx, w.tenant, e.ExceptionID, "lead", "c", e.Version,
			domain.AssignExceptionRequest{OwnerPrincipalID: "olivia", Reason: "owner"})
		require.NoError(t, err)
		e = *a
		_, err = step(t, w, &e, "olivia", domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
		require.NoError(t, err)
		_, err = step(t, w, &e, "sam", domain.ResolveExceptionRequest{ToState: "WAIVED_UNDER_AUTHORITY", Reason: "immaterial",
			AuthorityRef: "waiver:memo-7"})
		require.NoError(t, err)
	}
}

// certifyRun, submitForCertification and resolveException call the store's
// idempotent commands with a fresh Idempotency-Key each time, for tests whose
// subject is not idempotency (see idempotency_integration_test.go for that).
func certifyRun(c context.Context, tenantID, runID, actor, correlationID string, expectedVersion int,
	req domain.CertifyRequest, authority *domain.AuthoritySnapshot) (*domain.ControlRun, error) {
	out, _, err := testStore.CertifyRun(c, tenantID, runID, actor, correlationID, "k-"+uuid.NewString(), expectedVersion, req, authority)
	return out, err
}

func submitForCertification(c context.Context, tenantID, runID, actor, correlationID string, expectedVersion int, reason string) (*domain.ControlRun, error) {
	out, _, err := testStore.SubmitForCertification(c, tenantID, runID, actor, correlationID, "k-"+uuid.NewString(), expectedVersion, reason)
	return out, err
}

func resolveException(c context.Context, tenantID, exceptionID, actor, correlationID string, expectedVersion int,
	req domain.ResolveExceptionRequest) (*domain.ControlException, error) {
	out, _, err := testStore.ResolveException(c, tenantID, exceptionID, actor, correlationID, "k-"+uuid.NewString(), expectedVersion, req)
	return out, err
}
