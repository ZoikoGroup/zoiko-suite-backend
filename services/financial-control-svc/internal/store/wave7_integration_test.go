//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/store"
)

// failedRunWithTwoExceptions runs FIN-CTRL-021 (KEY) over two findings and assigns both to olivia.
func failedRunWithTwoExceptions(t *testing.T, w *wave2) (*domain.ControlRun, []domain.ControlException) {
	t.Helper()
	w.src.set(catalogue.SysGL, "control-account-postings", "wm", r("e1", "J-1", "250.00"), r("e2", "J-2", "40.00"))
	run, final := w.run("FIN-CTRL-021", fiscal)
	require.Equal(t, domain.ResultFail, final.ResultState)
	require.Equal(t, domain.LifecycleExceptionReview, final.LifecycleState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 2)
	out := make([]domain.ControlException, 0, 2)
	for _, e := range list {
		a, err := testStore.AssignException(ctx, w.tenant, e.ExceptionID, "lead", "c", e.Version,
			domain.AssignExceptionRequest{OwnerPrincipalID: "olivia", Reason: "owner"})
		require.NoError(t, err)
		out = append(out, *a)
	}
	return run, out
}

func step(t *testing.T, w *wave2, e *domain.ControlException, actor string, req domain.ResolveExceptionRequest) (*domain.ControlException, error) {
	t.Helper()
	out, err := testStore.ResolveException(ctx, w.tenant, e.ExceptionID, actor, "c", e.Version, req)
	if err == nil {
		*e = *out
	}
	return out, err
}

func TestWave7_ExceptionLifecycle_MakerCheckerAndRollup(t *testing.T) {
	w := newWave5(t)
	run, exc := failedRunWithTwoExceptions(t, w)
	x, y := &exc[0], &exc[1]

	// Working an exception is the owner's job.
	_, err := step(t, w, x, "mallory", domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
	require.ErrorIs(t, err, domain.ErrNotOwner)
	_, err = step(t, w, x, "olivia", domain.ResolveExceptionRequest{ToState: "CLOSED", Reason: "skip ahead"})
	require.ErrorIs(t, err, domain.ErrInvalidTransition, "no shortcut to CLOSED")
	_, err = testStore.ResolveException(ctx, w.tenant, x.ExceptionID, "olivia", "c", x.Version+9, domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "r"})
	require.ErrorIs(t, err, domain.ErrConflict)

	// Full remediation path for x.
	_, err = step(t, w, x, "olivia", domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
	require.NoError(t, err)
	_, err = step(t, w, x, "olivia", domain.ResolveExceptionRequest{ToState: "AWAITING_ADJUSTMENT", Reason: "need a journal"})
	require.NoError(t, err)
	_, err = step(t, w, x, "olivia", domain.ResolveExceptionRequest{ToState: "REMEDIATED", Reason: "reclassified", EvidenceRef: "JE-9001"})
	require.NoError(t, err)
	_, err = step(t, w, x, "olivia", domain.ResolveExceptionRequest{ToState: "REPERFORMED", Reason: "self check", EvidenceRef: "run-2"})
	require.ErrorIs(t, err, domain.ErrNotIndependent, "the owner cannot reperform their own remediation")
	_, err = step(t, w, x, "rita", domain.ResolveExceptionRequest{ToState: "REPERFORMED", Reason: "rerun clean", EvidenceRef: "run-2"})
	require.NoError(t, err)
	_, err = step(t, w, x, "rita", domain.ResolveExceptionRequest{ToState: "CLOSED", Reason: "verified"})
	require.NoError(t, err)

	cur, err := testStore.GetRun(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleExceptionReview, cur.LifecycleState, "y is still open: the run does not roll up yet")

	// y is waived: the owner cannot excuse their own exception; someone independent can.
	_, err = step(t, w, y, "olivia", domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
	require.NoError(t, err)
	_, err = step(t, w, y, "olivia", domain.ResolveExceptionRequest{ToState: "WAIVED_UNDER_AUTHORITY", Reason: "immaterial", AuthorityRef: "CFO-2026-14"})
	require.ErrorIs(t, err, domain.ErrNotIndependent)
	_, err = step(t, w, y, "sam", domain.ResolveExceptionRequest{ToState: "WAIVED_UNDER_AUTHORITY", Reason: "immaterial", AuthorityRef: "CFO-2026-14"})
	require.NoError(t, err)

	cur, err = testStore.GetRun(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleReadyToCertify, cur.LifecycleState, "the last exception resolved: the run rolls up")
	assert.Equal(t, domain.ResultPassWithApprovedEx, cur.ResultState, "a waiver makes it PASS_WITH_APPROVED_EXCEPTIONS")

	hist, err := testStore.ListExceptionTransitions(ctx, w.tenant, y.ExceptionID)
	require.NoError(t, err)
	last := hist[len(hist)-1]
	assert.Equal(t, "CFO-2026-14", last.AuthorityRef, "the authority behind the waiver is on the record")
	assert.Equal(t, "sam", last.ActorID)

	// Certification follows: rejection returns the run, resubmission brings it back, and the
	// same certifier may then decide, but the operator who produced the result never can.
	rej, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", cur.Version, domain.CertifyRequest{Decision: "REJECT", Reason: "attach the waiver memo"})
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleExceptionReview, rej.LifecycleState)
	assert.Equal(t, domain.CertRejected, rej.CertificationState)

	_, err = testStore.SubmitForCertification(ctx, w.tenant, run.RunID, "prep", "c", rej.Version+3, "memo attached")
	require.ErrorIs(t, err, domain.ErrConflict)
	sub, err := testStore.SubmitForCertification(ctx, w.tenant, run.RunID, "prep", "c", rej.Version, "memo attached")
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleReadyToCertify, sub.LifecycleState)
	assert.Equal(t, domain.CertPending, sub.CertificationState)

	_, err = testStore.CertifyRun(ctx, w.tenant, run.RunID, "prep", "c", sub.Version, domain.CertifyRequest{Decision: "CERTIFY"})
	require.ErrorIs(t, err, domain.ErrSegregation)
	done, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", sub.Version, domain.CertifyRequest{Decision: "CERTIFY"})
	require.NoError(t, err, "the certifier who rejected may certify the corrected run")
	assert.Equal(t, domain.LifecycleCertified, done.LifecycleState)
	assert.Equal(t, domain.ResultPassWithApprovedEx, done.ResultState)
}

func TestWave7_SubmitForCertification_RefusedWhileExceptionsAreOpen(t *testing.T) {
	w := newWave5(t)
	run, _ := failedRunWithTwoExceptions(t, w)
	cur, err := testStore.GetRun(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	_, err = testStore.SubmitForCertification(ctx, w.tenant, run.RunID, "prep", "c", cur.Version, "trying")
	require.ErrorIs(t, err, domain.ErrInvalidTransition)
}

func TestWave7_CarryForwardRecordsThePeriodAndRollsUp(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "control-account-postings", "wm", r("e1", "J-1", "250.00"))
	run, _ := w.run("FIN-CTRL-021", fiscal)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	e, err := testStore.AssignException(ctx, w.tenant, list[0].ExceptionID, "lead", "c", list[0].Version,
		domain.AssignExceptionRequest{OwnerPrincipalID: "olivia", Reason: "owner"})
	require.NoError(t, err)
	_, err = step(t, w, e, "olivia", domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "timing"})
	require.NoError(t, err)
	_, err = step(t, w, e, "sam", domain.ResolveExceptionRequest{ToState: "CARRIED_FORWARD_UNDER_AUTHORITY", Reason: "clears next month",
		AuthorityRef: "CFO-2026-20", CarryToPeriod: "2026-10"})
	require.NoError(t, err)
	hist, err := testStore.ListExceptionTransitions(ctx, w.tenant, e.ExceptionID)
	require.NoError(t, err)
	assert.Equal(t, "2026-10", hist[len(hist)-1].CarryToPeriod)
	cur, err := testStore.GetRun(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.ResultPassWithApprovedEx, cur.ResultState)

	// Terminal states are final.
	_, err = step(t, w, e, "sam", domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "reopen"})
	require.ErrorIs(t, err, domain.ErrInvalidTransition)
}

func TestWave7_RerunSupersedesTheCertifiedRunAndOnlyThat(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	first, final := w.run("FIN-CTRL-020", fiscal)
	done, err := testStore.CertifyRun(ctx, w.tenant, first.RunID, "checker", "c", final.Version, domain.CertifyRequest{Decision: "CERTIFY"})
	require.NoError(t, err)
	require.Equal(t, domain.LifecycleCertified, done.LifecycleState)

	second, _ := w.run("FIN-CTRL-020", func(r *domain.CreateRunRequest) {
		fiscal(r)
		r.PriorRunID, r.Reason = first.RunID, "late journal posted"
	})
	old, err := testStore.GetRun(ctx, w.tenant, first.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleSuperseded, old.LifecycleState)
	assert.Equal(t, domain.CertSuperseded, old.CertificationState)
	require.NotNil(t, old.SupersededByRunID)
	assert.Equal(t, second.RunID, *old.SupersededByRunID)

	// A different period is only linked, never superseded.
	third, _ := w.run("FIN-CTRL-020", func(r *domain.CreateRunRequest) {
		fiscal(r)
		r.PeriodID = "2026-10"
		r.PriorRunID, r.Reason = second.RunID, "next period"
	})
	sec, err := testStore.GetRun(ctx, w.tenant, second.RunID)
	require.NoError(t, err)
	assert.NotEqual(t, domain.LifecycleSuperseded, sec.LifecycleState)
	assert.NotEmpty(t, third.RunID)
}

func TestWave7_Sweep_ExpiresIdleRunsAndAnnouncesEachSLABreachOnce(t *testing.T) {
	w := newWave5(t)
	_, exc := failedRunWithTwoExceptions(t, w)
	require.Len(t, exc, 2)

	// A run created and never executed.
	idle, _, err := testStore.CreateRun(ctx, w.tenant, "prep", "c", "idle-"+w.entity, domain.CreateRunRequest{
		ControlDefinitionID: w.defs["FIN-CTRL-020"], LegalEntityID: w.entity, TriggerType: "PERIOD_END", PeriodID: "2026-11"})
	require.NoError(t, err)

	future := time.Now().UTC().Add(60 * 24 * time.Hour)
	res, err := testStore.Sweep(ctx, future, 24*time.Hour, 500)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.ExpiredRuns, 1)
	assert.GreaterOrEqual(t, res.SLABreaches, 2)
	assert.Zero(t, res.Errors)

	got, err := testStore.GetRun(ctx, w.tenant, idle.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleExpired, got.LifecycleState)

	again, err := testStore.Sweep(ctx, future, 24*time.Hour, 500)
	require.NoError(t, err)
	assert.Zero(t, again.SLABreaches, "each breach is announced once")
	assert.Zero(t, again.ExpiredRuns)
}
