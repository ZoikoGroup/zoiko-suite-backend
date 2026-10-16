package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	"zoiko.io/payroll-run-svc/internal/store"
)

// Real-Postgres tests for Wave 8: reconciliation runs, exceptions, the close gate, Banking
// outcome evidence, returned payments, correction completeness and the due sweep. Principals:
// p-1 received the result, p-2 submitted its posting and prepared the instruction, p-3 executes
// reconciliation, p-4 certifies, p-5 waives, p-7 approved the payment, p-9 submitted it.

func reconEv() store.ReconEventBuilder {
	return func(ch store.ReconChange) ([]domain.OutboxEvent, error) {
		return events.BuildReconEvents(ch.Kind, ch.Run, ch.Exception, ch.Instruction, ch.Observation, "actor", "corr", "")
	}
}

func postBridge(t *testing.T, s *store.PgStore, ctx context.Context, tenant, runRefID string) {
	t.Helper()
	b, _, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, runRefID, "p-9"), accEvents(tenant))
	require.NoError(t, err)
	_, err = s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", accEvents(tenant))
	require.NoError(t, err)
	fl := &fakeLedger{outcome: posted()}
	n, err := s.DispatchPending(ctx, 10, fl.post, accEvents(tenant))
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// reachBanking takes an approved instruction to HANDED_OFF with a Banking attempt.
func reachBanking(t *testing.T, s *store.PgStore, ctx context.Context, tenant string, a *domain.PaymentInstruction) *domain.PaymentInstruction {
	t.Helper()
	sub, _, err := s.BeginSubmitToBanking(ctx, a.InstructionID, a.Version, "p-9")
	require.NoError(t, err)
	done, err := s.RecordBankingOutcome(ctx, sub.InstructionID, "attempt-"+sub.InstructionID[:8], "SUBMITTED", "", payEvents(tenant))
	require.NoError(t, err)
	require.Equal(t, domain.PayHandedOff, done.State)
	return done
}

func observe(source, raw, outcome string) store.ObserveFunc {
	return func(_ context.Context, in domain.PaymentInstruction) *domain.BankingObservation {
		return &domain.BankingObservation{Source: source, RawStatus: raw, Outcome: outcome, AttemptID: in.AttemptID}
	}
}

func codesOf(es []domain.ReconException) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, e.Code)
	}
	return out
}

// reconciled builds a fully reconciled population: an accepted result, recognized liabilities,
// a POSTED journal, and a net-pay instruction for the only worker.
func reconciled(t *testing.T, s *store.PgStore, ctx context.Context, tenant string) (*store.ReceiveResultOutput, *domain.PaymentInstruction) {
	t.Helper()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	postBridge(t, s, ctx, tenant, res.Ref.RunRefID)
	return res, netPayDraft(t, s, ctx, tenant, res)
}

func TestWave8_ReconciliationRaisesExceptionsAndClosesThemWhenTheCauseClears(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	// Nothing downstream exists yet: not posted, no liabilities, nobody paid.
	first, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	assert.Equal(t, domain.ReconOutcomeBreak, first.Run.Outcome)
	codes := codesOf(first.Created)
	assert.Contains(t, codes, domain.ReconResultNotPosted)
	assert.Contains(t, codes, domain.ReconLiabilityMismatch)
	assert.Contains(t, codes, domain.ReconNetPayNotInstructed)
	assert.Equal(t, len(first.Created), first.Run.ExceptionCount)
	for _, e := range first.Created {
		assert.Equal(t, domain.ReconStateOpen, e.State)
		assert.Equal(t, "p-3", e.RaisedBy)
	}

	// The same difference is one open exception, not two.
	again, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", true, reconEv())
	require.NoError(t, err)
	assert.Empty(t, again.Created)
	assert.Empty(t, again.Resolved)
	assert.Len(t, again.Open, len(first.Created))
	require.NotNil(t, again.Run.ReperformedFrom)
	assert.Equal(t, first.Run.ReconRunID, *again.Run.ReperformedFrom)
	assert.Equal(t, first.Run.PopulationHash, again.Run.PopulationHash, "the same population hashes the same")

	// Recognize and post: those causes clear and their exceptions are RESOLVED, never dismissed.
	recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	postBridge(t, s, ctx, tenant, res.Ref.RunRefID)
	mid2, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", true, reconEv())
	require.NoError(t, err)
	assert.Equal(t, []string{domain.ReconNetPayNotInstructed}, codesOf(mid2.Open))
	assert.ElementsMatch(t, []string{domain.ReconResultNotPosted, domain.ReconLiabilityMismatch, domain.ReconLiabilityMismatch, domain.ReconLiabilityMismatch},
		codesOf(mid2.Resolved), "the three payable keys and the missing posting")
	for _, e := range mid2.Resolved {
		assert.Equal(t, domain.ReconStateResolved, e.State)
		require.NotNil(t, e.Resolution)
		assert.Equal(t, domain.ResolutionCauseCleared, *e.Resolution)
	}

	// Instruct the worker's net pay: everything reconciles.
	netPayDraft(t, s, ctx, tenant, res)
	done, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", true, reconEv())
	require.NoError(t, err)
	assert.Equal(t, domain.ReconOutcomeMatched, done.Run.Outcome)
	assert.Empty(t, done.Open)
	assert.Equal(t, 0, done.Run.ExceptionCount)
	for _, a := range done.Run.Assertions {
		assert.True(t, a.Matched, a.Name)
	}
	open, err := s.ListReconExceptions(ctx, res.Ref.RunRefID, "", "OPEN")
	require.NoError(t, err)
	assert.Empty(t, open)

	types := rEventTypes(t, pool, tenant)
	assert.Equal(t, 4, countType(t, types, domain.EventPayrollReconRunCompleted))
	assert.Equal(t, 5, countType(t, types, domain.EventPayrollReconExceptionResolved))
}

func TestWave8_OnlyAnAcceptedResultIsReconciled(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 2) // corrects v1
	_, err := s.ExecuteReconciliation(ctx, v1.Ref.RunRefID, "p-3", "corr", false, reconEv())
	assert.ErrorIs(t, err, domain.ErrReconResultNotAccepted, "a superseded result is not reconciled")
	_, err = s.ExecuteReconciliation(ctx, uuid.NewString(), "p-3", "corr", false, reconEv())
	assert.ErrorIs(t, err, domain.ErrRunRefNotFound)
}

func TestWave8_CertificationNeedsAMatchedRunADifferentPrincipalAndAnUnchangedPopulation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	res, instr := reconciled(t, s, ctx, tenant)

	// A MATCHED run, executed by p-3.
	run, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	require.Equal(t, domain.ReconOutcomeMatched, run.Run.Outcome)

	_, err = s.CertifyReconRun(ctx, run.Run.ReconRunID, "p-3", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconSoD, "the executor cannot certify their own run")
	_, err = s.CertifyReconRun(ctx, uuid.NewString(), "p-4", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconRunNotFound)

	// The population changes after the run: certification is refused, the run must be reperformed.
	_, err = s.CancelPaymentInstruction(ctx, instr.InstructionID, instr.Version, "changed my mind", payEvents(tenant))
	require.NoError(t, err)
	_, err = s.CertifyReconRun(ctx, run.Run.ReconRunID, "p-4", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconRunStale)

	// Put the population right and reperform; the new run certifies, and only once.
	netPayDraft(t, s, ctx, tenant, res)
	rerun, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", true, reconEv())
	require.NoError(t, err)
	require.Equal(t, domain.ReconOutcomeMatched, rerun.Run.Outcome)
	_, err = s.CertifyReconRun(ctx, run.Run.ReconRunID, "p-4", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconRunStale, "an older run is not certified once a newer one exists")
	cert, err := s.CertifyReconRun(ctx, rerun.Run.ReconRunID, "p-4", reconEv())
	require.NoError(t, err)
	require.NotNil(t, cert.CertifiedBy)
	assert.Equal(t, "p-4", *cert.CertifiedBy)
	_, err = s.CertifyReconRun(ctx, rerun.Run.ReconRunID, "p-5", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconAlreadyCertified)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollReconRunCertified))
}

func TestWave8_ABreakCannotBeCertified(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	run, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	_, err = s.CertifyReconRun(ctx, run.Run.ReconRunID, "p-4", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconNotMatched)
	_, err = pool.Exec(context.Background(), `UPDATE wfp_reconciliation_runs SET certified_by = 'p-4', certified_at = now() WHERE recon_run_id = $1`, run.Run.ReconRunID)
	assert.Error(t, err, "the database refuses to certify a BREAK as well")
}

func TestWave8_AWaiverNeedsEvidenceADifferentPrincipalAndIsNotRaisedAgain(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	run, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	var np *domain.ReconException
	for i := range run.Created {
		if run.Created[i].Code == domain.ReconResultNotPosted {
			np = &run.Created[i]
		}
	}
	require.NotNil(t, np)

	own, err := s.WaiveReconException(ctx, np.ExceptionID, "p-3", "reason", "evidence-1", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconSoD, "the principal who raised it cannot waive it")
	assert.Nil(t, own)
	_, err = pool.Exec(context.Background(), `UPDATE wfp_reconciliation_exceptions SET state = 'WAIVED', resolved_at = now(), waived_by = 'p-3',
		waiver_reason = 'r', waiver_evidence_ref = 'e' WHERE exception_id = $1`, np.ExceptionID)
	assert.Error(t, err, "the database refuses a self-waiver too")

	asg, err := s.AssignReconException(ctx, np.ExceptionID, "finance-team")
	require.NoError(t, err)
	assert.Equal(t, domain.ReconStateAssigned, asg.State)
	require.NotNil(t, asg.Owner)
	assert.Equal(t, "finance-team", *asg.Owner)

	w, err := s.WaiveReconException(ctx, np.ExceptionID, "p-5", "posting handled in the legacy ledger", "ticket-77", reconEv())
	require.NoError(t, err)
	assert.Equal(t, domain.ReconStateWaived, w.State)
	require.NotNil(t, w.WaivedBy)
	assert.Equal(t, "p-5", *w.WaivedBy)
	_, err = s.WaiveReconException(ctx, np.ExceptionID, "p-6", "again", "ticket-78", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconExceptionClosed, "a waived exception is terminal")
	_, err = s.AssignReconException(ctx, np.ExceptionID, "someone")
	assert.ErrorIs(t, err, domain.ErrReconExceptionClosed)

	// The next run does not raise the waived difference again; the run still reports the break.
	next, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", true, reconEv())
	require.NoError(t, err)
	assert.NotContains(t, codesOf(next.Created), domain.ReconResultNotPosted)
	assert.Equal(t, domain.ReconOutcomeBreak, next.Run.Outcome)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollReconExceptionWaived))

	_, err = pool.Exec(context.Background(), `DELETE FROM wfp_reconciliation_exceptions WHERE exception_id = $1`, np.ExceptionID)
	assert.Error(t, err, "exceptions are never deleted")
	_, err = pool.Exec(context.Background(), `UPDATE wfp_reconciliation_exceptions SET state = 'OPEN', resolved_at = NULL WHERE exception_id = $1`, np.ExceptionID)
	assert.Error(t, err, "a waived exception cannot be reopened")
	_, err = pool.Exec(context.Background(), `UPDATE wfp_reconciliation_runs SET outcome = 'MATCHED' WHERE recon_run_id = $1`, run.Run.ReconRunID)
	assert.Error(t, err, "a run is immutable evidence")
	_, err = pool.Exec(context.Background(), `DELETE FROM wfp_reconciliation_runs WHERE recon_run_id = $1`, run.Run.ReconRunID)
	assert.Error(t, err)
}

func TestWave8_CloseGateIsFailClosedAndFollowsTheEvidence(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()

	empty, err := s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.True(t, empty.Open, "no payroll in the period: nothing to gate")
	assert.Equal(t, 0, empty.Results)

	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	g, err := s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open)
	var codes []string
	for _, b := range g.Blockers {
		codes = append(codes, b.Code)
	}
	assert.Contains(t, codes, domain.GateNotPosted)
	assert.Contains(t, codes, domain.GateNoReconRun)
	other, err := s.CloseGateFor(ctx, "le-1", "2026-10")
	require.NoError(t, err)
	assert.True(t, other.Open, "another period is not affected")
	byDate, err := s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.Equal(t, 1, byDate.Results)
	other2, err := s.CloseGateFor(ctx, "le-2", "2026-09")
	require.NoError(t, err)
	assert.True(t, other2.Open, "another legal entity is not affected")

	// Reconcile fully: open, with the missing certification as a warning only.
	recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	postBridge(t, s, ctx, tenant, res.Ref.RunRefID)
	d := netPayDraft(t, s, ctx, tenant, res)
	run, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	require.Equal(t, domain.ReconOutcomeMatched, run.Run.Outcome)
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.True(t, g.Open, "%+v", g.Blockers)
	require.Len(t, g.Warnings, 1)
	assert.Equal(t, domain.GateNotCertified, g.Warnings[0].Code)

	// Banking returns the payment after the run: the run is stale until reperformed.
	a := approved(t, s, ctx, tenant, d)
	h := reachBanking(t, s, ctx, tenant, a)
	n, err := s.RefreshBankingOutcomes(context.Background(), 10, observe(domain.ObsSourcePayment, "RETURNED", domain.OutcomeReturned), reconEv())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open)
	assert.Equal(t, domain.GateStaleReconciliation, g.Blockers[0].Code)

	rerun, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", true, reconEv())
	require.NoError(t, err)
	require.Equal(t, []string{domain.ReconPaymentReturned}, codesOf(rerun.Created))
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open)
	require.Len(t, g.Blockers, 1, "%+v", g.Blockers)
	assert.Equal(t, domain.GateOpenException, g.Blockers[0].Code)

	// Someone with authority accepts the returned payment for the close, with evidence: the gate opens.
	exc := rerun.Created[0]
	_, err = s.WaiveReconException(ctx, exc.ExceptionID, "p-5", "returned to the same account; re-sent manually", "ticket-9", reconEv())
	require.NoError(t, err)
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.True(t, g.Open, "%+v", g.Blockers)

	// Acknowledging the return changes the population again; the net pay is no longer instructed.
	ret, err := s.MarkInstructionReturned(ctx, h.InstructionID, h.Version, "Banking returned it", reconEv())
	require.NoError(t, err)
	assert.Equal(t, domain.PayReturned, ret.State)
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open)
	assert.Equal(t, domain.GateStaleReconciliation, g.Blockers[0].Code)
	after, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", true, reconEv())
	require.NoError(t, err)
	assert.Equal(t, []string{domain.ReconNetPayNotInstructed}, codesOf(after.Created))
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open, "the worker is owed and no live instruction covers it")
}
