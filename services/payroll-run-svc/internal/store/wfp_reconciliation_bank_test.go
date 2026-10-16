package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

func TestWave8_BankingObservationsAreAppendOnlyDedupedAndStopWhenFinal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	res, d := reconciled(t, s, ctx, tenant)
	h := reachBanking(t, s, ctx, tenant, approved(t, s, ctx, tenant, d))

	calls := 0
	obs := func(source, raw, outcome string) store.ObserveFunc {
		inner := observe(source, raw, outcome)
		return func(c context.Context, in domain.PaymentInstruction) *domain.BankingObservation {
			calls++
			return inner(c, in)
		}
	}
	refresh := func(o store.ObserveFunc) int {
		n, err := s.RefreshBankingOutcomes(context.Background(), 10, o, reconEv())
		require.NoError(t, err)
		return n
	}

	assert.Equal(t, 1, refresh(obs(domain.ObsSourceAttempt, "SUBMITTED", domain.OutcomeInFlight)))
	assert.Equal(t, 0, refresh(obs(domain.ObsSourceAttempt, "SUBMITTED", domain.OutcomeInFlight)), "the same answer is not recorded twice")
	assert.Equal(t, 2, calls, "but Banking was asked both times")
	assert.Equal(t, 1, refresh(obs(domain.ObsSourceAttempt, "PENDING_UNKNOWN", domain.OutcomeUnresolved)), "unknown is recorded as unknown")
	assert.Equal(t, 0, refresh(func(context.Context, domain.PaymentInstruction) *domain.BankingObservation { return nil }),
		"Banking unreachable records nothing and invents nothing")
	assert.Equal(t, 1, refresh(obs(domain.ObsSourcePayment, "SETTLED", domain.OutcomeSettled)))

	before := calls
	assert.Equal(t, 0, refresh(obs(domain.ObsSourcePayment, "SETTLED", domain.OutcomeSettled)))
	assert.Equal(t, before, calls, "a settled payment is final: it is not asked about again")

	list, err := s.ListBankingObservations(ctx, h.InstructionID)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, domain.OutcomeSettled, list[0].Outcome, "newest first")
	assert.Equal(t, domain.OutcomeInFlight, list[2].Outcome)
	assert.Equal(t, 3, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollBankingOutcomeObserved))

	_, err = pool.Exec(context.Background(), `UPDATE wfp_instruction_banking_observations SET outcome = 'SETTLED' WHERE instruction_id = $1`, h.InstructionID)
	assert.Error(t, err, "observations are append-only")
	_, err = pool.Exec(context.Background(), `DELETE FROM wfp_instruction_banking_observations WHERE instruction_id = $1`, h.InstructionID)
	assert.Error(t, err)

	// A settled payment reconciles: the whole population matches.
	run, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	assert.Equal(t, domain.ReconOutcomeMatched, run.Run.Outcome)
}

func TestWave8_PendingAndUnknownOutcomesAreVisibleAndBlockReconciliation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	res, d := reconciled(t, s, ctx, tenant)
	reachBanking(t, s, ctx, tenant, approved(t, s, ctx, tenant, d))

	none, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	require.Equal(t, []string{domain.ReconNoBankingObservation}, codesOf(none.Created))
	assert.Equal(t, domain.SeverityWarning, none.Created[0].Severity, "no observation yet is a warning")

	_, err = s.RefreshBankingOutcomes(context.Background(), 10, observe(domain.ObsSourceAttempt, "PENDING_UNKNOWN", domain.OutcomeUnresolved), reconEv())
	require.NoError(t, err)
	unk, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", true, reconEv())
	require.NoError(t, err)
	assert.Equal(t, []string{domain.ReconOutcomeUnresolved}, codesOf(unk.Created))
	assert.Equal(t, domain.SeverityBlocking, unk.Created[0].Severity, "unknown blocks, and is never reported as failed or paid")
	assert.Equal(t, []string{domain.ReconNoBankingObservation}, codesOf(unk.Resolved))
	_, err = s.CertifyReconRun(ctx, unk.Run.ReconRunID, "p-4", reconEv())
	assert.ErrorIs(t, err, domain.ErrReconNotMatched)
}

func TestWave8_AReturnIsAnExplicitCommandBankingMustHaveReportedAndItFreesTheAmount(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	res, d := reconciled(t, s, ctx, tenant)
	h := reachBanking(t, s, ctx, tenant, approved(t, s, ctx, tenant, d))

	_, err := s.MarkInstructionReturned(ctx, h.InstructionID, h.Version, "x", reconEv())
	assert.ErrorIs(t, err, domain.ErrInstructionNotReturned, "no Banking observation at all")
	_, err = s.RefreshBankingOutcomes(context.Background(), 10, observe(domain.ObsSourceAttempt, "SUBMITTED", domain.OutcomeInFlight), reconEv())
	require.NoError(t, err)
	_, err = s.MarkInstructionReturned(ctx, h.InstructionID, h.Version, "x", reconEv())
	assert.ErrorIs(t, err, domain.ErrInstructionNotReturned, "in flight is not returned")
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payment_instructions SET state = 'RETURNED', returned_at = now(), return_reason = 'forced' WHERE instruction_id = $1`, h.InstructionID)
	assert.Error(t, err, "the database refuses RETURNED without Banking's RETURNED")

	_, err = s.RefreshBankingOutcomes(context.Background(), 10, observe(domain.ObsSourcePayment, "RETURNED", domain.OutcomeReturned), reconEv())
	require.NoError(t, err)
	_, err = s.MarkInstructionReturned(ctx, h.InstructionID, 99, "x", reconEv())
	assert.ErrorIs(t, err, domain.ErrInstructionVersionConflict)

	// While the returned payment is acknowledged by nobody, a replacement is not possible: the slot is taken.
	ws, err := s.ListResultWorkers(ctx, res.Ref.RunRefID)
	require.NoError(t, err)
	w := ws[0].WorkerID
	dup := &domain.PaymentInstruction{
		InstructionID: uuid.NewString(), TenantID: tenant, LegalEntityID: "le-1", InstructionType: domain.PayTypeNetPay,
		RunRefID: res.Ref.RunRefID, ResultID: res.Result.ResultID, WorkerID: &w, PayeeRef: "payee-1", DestinationID: "dest-1",
		DestinationVersion: "v1", DestinationFingerprint: "bank-fp-v1", Amount: ws[0].NetPay, Currency: ws[0].Currency, ValueDate: "2026-09-30",
		PayerAccountRef: "acct-1", PaymentMethod: domain.PayMethodBank, PaymentReference: "Payroll 2026-09", CorrelationID: "corr", CreatedBy: "p-2",
	}
	dup.Fingerprint = domain.ComputeInstructionFingerprint(dup)
	again, created, err := s.CreatePaymentInstruction(ctx, dup, payEvents(tenant))
	require.NoError(t, err)
	assert.False(t, created, "the original (returned, unacknowledged) instruction still stands")
	assert.Equal(t, h.InstructionID, again.InstructionID)

	ret, err := s.MarkInstructionReturned(ctx, h.InstructionID, h.Version, "Banking returned it: account closed", reconEv())
	require.NoError(t, err)
	assert.Equal(t, domain.PayReturned, ret.State)
	require.NotNil(t, ret.ReturnedAt)
	require.NotNil(t, ret.ReturnReason)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollPaymentInstrReturned))
	_, err = s.MarkInstructionReturned(ctx, h.InstructionID, ret.Version, "again", reconEv())
	assert.ErrorIs(t, err, domain.ErrInstructionState)
	_, err = s.CancelPaymentInstruction(ctx, h.InstructionID, ret.Version, "x", nil)
	assert.ErrorIs(t, err, domain.ErrInstructionState, "a returned instruction is final")

	// Now the worker can be instructed again, and reconciliation sees exactly that.
	fresh := netPayDraft(t, s, ctx, tenant, res)
	assert.NotEqual(t, h.InstructionID, fresh.InstructionID)
	n, err := s.RefreshBankingOutcomes(context.Background(), 10, observe(domain.ObsSourceAttempt, "SUBMITTED", domain.OutcomeInFlight), reconEv())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a returned instruction is not asked about again")
}

func TestWave8_AReturnedPaymentThatAlreadySettledALiabilityIsRefusedNotFaked(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	paye := liabOf(t, out, "PAYE")
	ld, err := liabDraft(t, s, ctx, tenant, res, paye, "600.10")
	require.NoError(t, err)
	h := reachBanking(t, s, ctx, tenant, approved(t, s, ctx, tenant, ld))
	_, err = s.SettleLiability(ctx, paye.LiabilityID, domain.SettleLiabilityRequest{PaymentID: "pay-1", Amount: "600.10", Currency: "GBP", InstructionID: h.InstructionID},
		settled, "p-5", liabEvents(tenant))
	require.NoError(t, err)
	_, _, err = s.RecordBankingObservation(ctx, h.InstructionID, &domain.BankingObservation{Source: domain.ObsSourcePayment, RawStatus: "RETURNED", Outcome: domain.OutcomeReturned},
		"p-8", reconEv())
	require.NoError(t, err)

	_, err = s.MarkInstructionReturned(ctx, h.InstructionID, h.Version, "returned after settlement", reconEv())
	assert.ErrorIs(t, err, domain.ErrReturnedAfterSettlement, "reversing a settlement is not modelled; it must be adjusted explicitly")
	got, err := s.GetPaymentInstruction(ctx, h.InstructionID)
	require.NoError(t, err)
	assert.Equal(t, domain.PayHandedOff, got.State)
}

func TestWave8_ALinkedPaymentIsSetOnceAndOnlyForAnInstructionThatReachedBanking(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	_, d := reconciled(t, s, ctx, tenant)

	_, err := s.LinkInstructionPayment(ctx, d.InstructionID, "pay-1")
	assert.ErrorIs(t, err, domain.ErrInstructionState, "a draft has no payment")
	h := reachBanking(t, s, ctx, tenant, approved(t, s, ctx, tenant, d))
	l1, err := s.LinkInstructionPayment(ctx, h.InstructionID, "pay-1")
	require.NoError(t, err)
	require.NotNil(t, l1.PaymentID)
	assert.Equal(t, "pay-1", *l1.PaymentID)
	same, err := s.LinkInstructionPayment(ctx, h.InstructionID, "pay-1")
	require.NoError(t, err)
	assert.Equal(t, l1.Version, same.Version, "linking the same payment again changes nothing")
	_, err = s.LinkInstructionPayment(ctx, h.InstructionID, "pay-2")
	assert.ErrorIs(t, err, domain.ErrPaymentLinkMismatch)
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payment_instructions SET payment_id = 'pay-2' WHERE instruction_id = $1`, h.InstructionID)
	assert.Error(t, err, "the database keeps the link too")
}

// A replacement net-pay instruction for the same worker and period (a correction result) must wait
// until the original Banking attempt is resolved: an unknown outcome is not a licence to pay again.
func TestWave8_ACorrectionCannotBePaidWhileTheOriginalPaymentIsUnresolved(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	worker := uuid.NewString()
	mk := func(run string) *store.ReceiveResultOutput {
		req := resultRequest(mid, run, 1, resultWorker(worker))
		out, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
		require.NoError(t, err)
		require.Equal(t, domain.ResultAccepted, out.Result.Outcome)
		return out
	}
	orig, corr := mk("RUN-1"), mk("RUN-2")
	h := reachBanking(t, s, ctx, tenant, approved(t, s, ctx, tenant, netPayDraft(t, s, ctx, tenant, orig)))

	draft := func() (*domain.PaymentInstruction, error) {
		ws, err := s.ListResultWorkers(ctx, corr.Ref.RunRefID)
		require.NoError(t, err)
		w := ws[0].WorkerID
		in := &domain.PaymentInstruction{
			InstructionID: uuid.NewString(), TenantID: tenant, LegalEntityID: "le-1", InstructionType: domain.PayTypeNetPay,
			RunRefID: corr.Ref.RunRefID, ResultID: corr.Result.ResultID, WorkerID: &w, PayeeRef: "payee-1", DestinationID: "dest-1",
			DestinationVersion: "v1", DestinationFingerprint: "bank-fp-v1", Amount: ws[0].NetPay, Currency: ws[0].Currency, ValueDate: "2026-09-30",
			PayerAccountRef: "acct-1", PaymentMethod: domain.PayMethodBank, PaymentReference: "Payroll 2026-09 correction", CorrelationID: "corr", CreatedBy: "p-2",
		}
		in.Fingerprint = domain.ComputeInstructionFingerprint(in)
		out, _, err := s.CreatePaymentInstruction(ctx, in, payEvents(tenant))
		return out, err
	}

	_, err := draft()
	assert.ErrorIs(t, err, domain.ErrOutcomeUnresolved, "no observation yet: the original may have been paid")
	_, _, err = s.RecordBankingObservation(ctx, h.InstructionID, &domain.BankingObservation{Source: domain.ObsSourceAttempt, RawStatus: "PENDING_UNKNOWN", Outcome: domain.OutcomeUnresolved}, "p-8", reconEv())
	require.NoError(t, err)
	_, err = draft()
	assert.ErrorIs(t, err, domain.ErrOutcomeUnresolved, "unknown")
	_, _, err = s.RecordBankingObservation(ctx, h.InstructionID, &domain.BankingObservation{Source: domain.ObsSourcePayment, RawStatus: "RETURNED", Outcome: domain.OutcomeReturned}, "p-8", reconEv())
	require.NoError(t, err)
	got, err := draft()
	require.NoError(t, err, "Banking says it came back: the replacement may be raised")
	assert.Equal(t, corr.Result.ResultID, got.ResultID)
}

func TestWave8_CorrectionCompletenessTracksEveryDownstreamEffect(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	recognize(t, s, ctx, tenant, v1.Ref.RunRefID, nil, "2026-09-30")
	b, _, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, v1.Ref.RunRefID, "p-9"), accEvents(tenant))
	require.NoError(t, err)
	netPayDraft(t, s, ctx, tenant, v1)
	run, err := s.ExecuteReconciliation(ctx, v1.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	require.NotEmpty(t, run.Open)

	_, err = s.CorrectionCompletenessFor(ctx, v1.Ref.RunRefID)
	assert.ErrorIs(t, err, domain.ErrResultNotSuperseded, "an accepted result has no correction to complete")

	acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 2) // corrects v1
	c, err := s.CorrectionCompletenessFor(ctx, v1.Ref.RunRefID)
	require.NoError(t, err)
	assert.False(t, c.Complete)
	pending := map[string]int{}
	for _, e := range c.Effects {
		pending[e.Effect] = e.Pending
	}
	assert.Equal(t, map[string]int{"ACCOUNTING": 1, "LIABILITIES": 3, "PAYMENTS": 1}, pending)

	_, err = s.WithdrawAccountingBridge(ctx, b.BridgeID, b.Version, "superseded", accEvents(tenant))
	require.NoError(t, err)
	_, err = s.SupersedeLiabilitiesForRun(ctx, v1.Ref.RunRefID, liabEvents(tenant))
	require.NoError(t, err)
	_, err = s.SupersedeInstructionsForRun(ctx, v1.Ref.RunRefID, payEvents(tenant))
	require.NoError(t, err)
	resolved, err := s.ResolveExceptionsForSupersededResult(ctx, v1.Ref.RunRefID, reconEv())
	require.NoError(t, err)
	assert.Len(t, resolved, len(run.Open), "the superseded result's exceptions close as RESULT_SUPERSEDED")
	for _, e := range resolved {
		require.NotNil(t, e.Resolution)
		assert.Equal(t, domain.ResolutionResultSuperseded, *e.Resolution)
	}
	again, err := s.ResolveExceptionsForSupersededResult(ctx, v1.Ref.RunRefID, reconEv())
	require.NoError(t, err)
	assert.Empty(t, again, "idempotent")

	c, err = s.CorrectionCompletenessFor(ctx, v1.Ref.RunRefID)
	require.NoError(t, err)
	assert.True(t, c.Complete, "%+v", c.Effects)
}

func TestWave8_ADueSweepMovesEveryTenantsLiabilitiesAndRepeatsHarmlessly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, ctxA := wfpTenant()
	tenantB, ctxB := wfpTenant()
	for _, x := range []struct {
		tenant string
		ctx    context.Context
	}{{tenantA, ctxA}, {tenantB, ctxB}} {
		registerStandardClasses(t, s, x.ctx)
		mid := registerManifest(t, s, x.ctx)
		res := acceptedResult(t, s, x.ctx, x.tenant, mid, "RUN-1", 1)
		recognize(t, s, x.ctx, x.tenant, res.Ref.RunRefID, nil, "2026-09-30")
	}
	n, err := s.SweepDueAllTenants(context.Background(), "2026-09-30", liabEvents(""))
	require.NoError(t, err)
	assert.Equal(t, 0, n, "nothing is due on the pay date: PAYE is due on 22 October")

	n, err = s.SweepDueAllTenants(context.Background(), "2026-10-22", liabEvents(""))
	require.NoError(t, err)
	assert.Equal(t, 6, n, "all three liabilities of both tenants are due by 22 October")
	for _, c := range []struct {
		tenant string
		ctx    context.Context
	}{{tenantA, ctxA}, {tenantB, ctxB}} {
		list, err := s.ListLiabilities(c.ctx, store.LiabilityFilter{})
		require.NoError(t, err)
		require.Len(t, list, 3)
		for _, l := range list {
			assert.Equal(t, domain.LiabDue, l.State)
		}
		assert.Equal(t, 3, countType(t, rEventTypes(t, pool, c.tenant), domain.EventPayrollLiabilityDue))
	}
	n, err = s.SweepDueAllTenants(context.Background(), "2026-10-22", liabEvents(""))
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a second sweep (another replica) finds nothing to do")
}

func TestWave8_WorksUnderRowLevelSecurity(t *testing.T) {
	admin := openTestPool(t)
	app := appPool(t, admin)
	s := store.New(app, "")
	tenantA, ctxA := wfpTenant()
	tenantB, ctxB := wfpTenant()
	resA, dA := reconciled(t, s, ctxA, tenantA)
	resB, _ := reconciled(t, s, ctxB, tenantB)
	hA := reachBanking(t, s, ctxA, tenantA, approved(t, s, ctxA, tenantA, dA))

	// Background refresh: finds the work across tenants, writes inside the instruction's own tenant.
	n, err := s.RefreshBankingOutcomes(context.Background(), 10, observe(domain.ObsSourcePayment, "RETURNED", domain.OutcomeReturned), reconEv())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	obsA, err := s.ListBankingObservations(ctxA, hA.InstructionID)
	require.NoError(t, err)
	assert.Len(t, obsA, 1)
	obsB, err := s.ListBankingObservations(ctxB, hA.InstructionID)
	require.NoError(t, err)
	assert.Empty(t, obsB, "another tenant sees none of it")

	runA, err := s.ExecuteReconciliation(ctxA, resA.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	assert.Equal(t, []string{domain.ReconPaymentReturned}, codesOf(runA.Created))
	_, err = s.ExecuteReconciliation(ctxB, resA.Ref.RunRefID, "p-3", "corr", false, reconEv())
	assert.Error(t, err, "tenant B cannot reconcile tenant A's result")
	runB, err := s.ExecuteReconciliation(ctxB, resB.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	assert.Equal(t, domain.ReconOutcomeMatched, runB.Run.Outcome, "B's draft instruction reconciles; A's returned payment does not touch it")

	gA, err := s.CloseGateFor(ctxA, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, gA.Open)
	gB, err := s.CloseGateFor(ctxB, "le-1", "2026-09")
	require.NoError(t, err)
	assert.True(t, gB.Open, "%+v", gB.Blockers)

	exA, err := s.ListReconExceptions(ctxB, "", "", "")
	require.NoError(t, err)
	assert.Empty(t, exA, "B has no exceptions, and cannot read A's")

	m, err := s.SweepDueAllTenants(context.Background(), "2026-10-22", liabEvents(""))
	require.NoError(t, err)
	assert.Equal(t, 6, m, "the due sweep reaches both tenants under a non-superuser role")

	var rows int
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM wfp_reconciliation_runs`).Scan(&rows))
	assert.Equal(t, 0, rows, "no tenant context, no rows")
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM wfp_instruction_banking_observations`).Scan(&rows))
	assert.Equal(t, 0, rows)
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM wfp_reconciliation_exceptions`).Scan(&rows))
	assert.Equal(t, 0, rows)
}

// A waiver must be effective: once every blocking difference of a current run is waived with
// evidence, the gate opens, even though the result is not posted. The gate does not add its own
// not-posted blocker over a current run; it speaks through the run exceptions.
func TestWave8_WaivingEveryBlockingExceptionOpensTheGateButNeverWithoutARun(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	g, err := s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open, "no run: shut")

	run, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)
	require.NotEmpty(t, run.Created)
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open, "open blocking exceptions: shut")

	for _, e := range run.Created {
		_, err := s.WaiveReconException(ctx, e.ExceptionID, "p-5", "accepted for the close", "ticket-"+e.Code, reconEv())
		require.NoError(t, err)
	}
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.True(t, g.Open, "every difference is waived with evidence: %+v", g.Blockers)

	// Anything that changes the population afterwards makes the run stale again, and the gate is shut
	// until it is reperformed: a waiver covers what was seen, not what happens next.
	recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	g, err = s.CloseGateFor(ctx, "le-1", "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open)
	var codes []string
	for _, b := range g.Blockers {
		codes = append(codes, b.Code)
	}
	assert.Contains(t, codes, domain.GateStaleReconciliation)
	assert.Contains(t, codes, domain.GateNotPosted, "a stale run no longer speaks for the posting")
}
