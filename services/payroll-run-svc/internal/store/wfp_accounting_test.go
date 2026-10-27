package store_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

// Real-Postgres tests for WFP-05. The embedded superuser bypasses RLS, so tenant
// isolation proves the explicit tenant_id predicate (and that the dispatcher sets
// each subject's tenant); the database guards are exercised with direct SQL.

func accEvents(tenant string) store.AccountingEventBuilder {
	return func(ch store.AccountingChange) ([]domain.OutboxEvent, error) {
		typ := map[store.AccountingChangeKind]string{
			store.AccChangeCreated: domain.EventPayrollAccountingEventCreated, store.AccChangeSubmitted: domain.EventPayrollAccountingSubmitted,
			store.AccChangePosted: domain.EventPayrollAccountingPosted, store.AccChangeRejected: domain.EventPayrollAccountingRejected,
			store.AccChangeCorrected: domain.EventPayrollAccountingCorrected,
		}[ch.Kind]
		return []domain.OutboxEvent{{TenantID: tenant, ObjectID: ch.Bridge.ResultID, EventType: typ, Payload: []byte(`{}`)}}, nil
	}
}

// acceptedResult receives an ACCEPTED result (received_by p-1) and returns its run reference id.
func acceptedResult(t *testing.T, s *store.PgStore, ctx context.Context, tenant, manifestID, runID string, version int) *store.ReceiveResultOutput {
	t.Helper()
	req := resultRequest(manifestID, runID, version, resultWorker(uuid.NewString()))
	req.EvidenceFlags = []string{"PLACEHOLDER_TAX_RATE"}
	out, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
	require.NoError(t, err)
	require.Equal(t, domain.ResultAccepted, out.Result.Outcome)
	return out
}

// buildBridge composes the subject the way the handler does.
func buildBridge(t *testing.T, s *store.PgStore, ctx context.Context, runRefID, creator string) *domain.AccountingBridge {
	t.Helper()
	in, res, ref, err := s.ResultPostingInputFor(ctx, runRefID)
	require.NoError(t, err)
	lines, err := domain.BuildPostingLines(in)
	require.NoError(t, err)
	d, c, err := domain.SumLines(lines)
	require.NoError(t, err)
	return &domain.AccountingBridge{
		BridgeID: uuid.NewString(), RunRefID: ref.RunRefID, ResultID: res.ResultID, LegalEntityID: ref.LegalEntityID,
		AccountingBookID: domain.DefaultAccountingBook, PostingPurpose: domain.PurposeResult,
		SourceEventID: domain.SourceEventID(res.ResultID, domain.DefaultAccountingBook, domain.PurposeResult),
		FiscalPeriod:  domain.FiscalPeriodOf(ref.PayDate), DocumentDate: ref.PayDate, PostingDate: ref.PayDate,
		TransactionCurrency: res.Currency, Description: "Payroll", Lines: lines, LineCount: len(lines),
		TotalDebit: domain.FormatDecimal(d), TotalCredit: domain.FormatDecimal(c), EvidenceFlags: res.EvidenceFlags,
		CreatedBy: creator, CorrelationID: "corr",
	}
}

func evTypes(t *testing.T, pool *pgxpool.Pool, tenant string) []string {
	return rEventTypes(t, pool, tenant)
}

type fakeLedger struct {
	calls     atomic.Int32
	outcome   store.PostOutcome
	lastKind  string
	lastOrig  string
	lastKeys  []string
	perSource map[string]store.PostOutcome
}

func (f *fakeLedger) post(_ context.Context, b domain.AccountingBridge, originalJournal string) store.PostOutcome {
	f.calls.Add(1)
	f.lastKind, f.lastOrig = b.PostingPurpose, originalJournal
	f.lastKeys = nil
	for _, l := range b.Lines {
		f.lastKeys = append(f.lastKeys, l.MappingKey)
	}
	if o, ok := f.perSource[b.SourceEventID]; ok {
		return o
	}
	return f.outcome
}

func posted() store.PostOutcome {
	return store.PostOutcome{Posted: true, ExecutionID: "exec-" + uuid.NewString()[:8], JournalID: "jrn-" + uuid.NewString()[:8]}
}

func TestWFP05_CreateIsOneCanonicalSubjectPerResultBookAndPurpose(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	b, created, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-1"), accEvents(tenant))
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, domain.BridgeEventCreated, b.State)
	assert.Equal(t, "3350.5000", b.TotalDebit, "gross + employer social")
	assert.Equal(t, b.TotalDebit, b.TotalCredit)
	assert.Equal(t, 6, b.LineCount)
	assert.Equal(t, []string{"PLACEHOLDER_TAX_RATE"}, b.EvidenceFlags, "the evidence flags travel with the subject")
	assert.Equal(t, "2026-09", b.FiscalPeriod)
	require.Len(t, b.Lines, 6)

	// The same subject requested again is the same subject, not a second posting.
	again, created, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-2"), accEvents(tenant))
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, b.BridgeID, again.BridgeID)
	assert.Equal(t, []string{domain.EventPayrollAccountingEventCreated}, evTypes(t, pool, tenant)[len(evTypes(t, pool, tenant))-1:])

	// Another book is a different subject.
	other := buildBridge(t, s, ctx, res.Ref.RunRefID, "p-1")
	other.AccountingBookID = "STATUTORY"
	other.SourceEventID = domain.SourceEventID(other.ResultID, "STATUTORY", domain.PurposeResult)
	b2, created, err := s.CreateAccountingBridge(ctx, other, nil)
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEqual(t, b.BridgeID, b2.BridgeID)

	all, err := s.ListAccountingBridges(ctx, res.Ref.RunRefID)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestWFP05_AnUnbalancedOrNonAcceptedSubjectIsImpossible(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	ok := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	// An unbalanced proposal cannot be stored, even by direct SQL.
	bad := buildBridge(t, s, ctx, ok.Ref.RunRefID, "p-1")
	bad.TotalCredit = "1.0000"
	_, _, err := s.CreateAccountingBridge(ctx, bad, nil)
	assert.Error(t, err, "total_debit must equal total_credit at the database")

	// A REJECTED result has no accounting consequence.
	w := uuid.NewString()
	badWorker := resultWorker(w)
	badWorker.NetPay = "2400.41"
	rejReq := resultRequest(mid, "RUN-REJ", 1, badWorker)
	rejReq.RunTotals.NetPay = "2400.41"
	rej, err := s.ReceivePayrollResult(ctx, prepare(tenant, rejReq), nil)
	require.NoError(t, err)
	require.Equal(t, domain.ResultRejected, rej.Result.Outcome)
	forced := buildBridge(t, s, ctx, ok.Ref.RunRefID, "p-1")
	forced.RunRefID, forced.ResultID = rej.Ref.RunRefID, rej.Result.ResultID
	forced.SourceEventID = domain.SourceEventID(forced.ResultID, forced.AccountingBookID, forced.PostingPurpose)
	_, _, err = s.CreateAccountingBridge(ctx, forced, nil)
	assert.Error(t, err, "a rejected result cannot be posted")

	// A SUPERSEDED result cannot get a new subject.
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-S", 1)
	acceptedResult(t, s, ctx, tenant, mid, "RUN-S", 2)
	_, _, err = s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, v1.Ref.RunRefID, "p-1"), nil)
	assert.ErrorIs(t, err, domain.ErrResultNotPostable)
}

func TestWFP05_SeparationOfDutiesIsEnforcedByTheDatabase(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1) // received_by = p-1
	b, _, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-9"), accEvents(tenant))
	require.NoError(t, err)

	// The principal who received the result cannot submit its posting.
	_, err = s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-1", accEvents(tenant))
	assert.ErrorIs(t, err, domain.ErrSoDSamePrincipal)

	// Nor can anyone bypass the store.
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payroll_accounting_bridges SET state = 'SUBMITTED', submitted_by = 'p-1' WHERE bridge_id = $1`, b.BridgeID)
	assert.Error(t, err)
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payroll_accounting_bridges SET state = 'SUBMITTED', submitted_by = NULL WHERE bridge_id = $1`, b.BridgeID)
	assert.Error(t, err)

	_, err = s.SubmitAccountingBridge(ctx, b.BridgeID, 99, "p-2", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeVersionConflict)
	sub, err := s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", accEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.BridgeSubmitted, sub.State)
	require.NotNil(t, sub.SubmittedBy)
	assert.Equal(t, "p-2", *sub.SubmittedBy)

	_, err = s.SubmitAccountingBridge(ctx, b.BridgeID, sub.Version, "p-2", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeNotSubmittable, "already submitted")
}

func TestWFP05_DispatcherOutcomes(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)

	submit := func(runID string) *domain.AccountingBridge {
		res := acceptedResult(t, s, ctx, tenant, mid, runID, 1)
		b, _, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-9"), accEvents(tenant))
		require.NoError(t, err)
		sub, err := s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", accEvents(tenant))
		require.NoError(t, err)
		return sub
	}
	okSub, refusedSub, flakySub := submit("RUN-OK"), submit("RUN-REFUSED"), submit("RUN-FLAKY")
	fl := &fakeLedger{perSource: map[string]store.PostOutcome{
		okSub.SourceEventID:      {Posted: true, ExecutionID: "exec-1", JournalID: "jrn-1"},
		refusedSub.SourceEventID: {Final: true, Err: "ledger refused (422): no account mapping for PAYROLL_NET_PAY_PAYABLE"},
		flakySub.SourceEventID:   {Err: "ledger answered 503"},
	}}

	n, err := s.DispatchPending(ctx, 10, fl.post, accEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "one subject posted")
	assert.EqualValues(t, 3, fl.calls.Load())
	assert.Contains(t, fl.lastKeys, domain.KeyGrossExpense, "the ledger was asked in mapping keys")

	got, _ := s.GetAccountingBridge(ctx, okSub.BridgeID)
	assert.Equal(t, domain.BridgePosted, got.State)
	require.NotNil(t, got.JournalID)
	assert.Equal(t, "jrn-1", *got.JournalID)
	assert.Equal(t, "exec-1", *got.ExecutionID)
	require.NotNil(t, got.PostedAt)

	got, _ = s.GetAccountingBridge(ctx, refusedSub.BridgeID)
	assert.Equal(t, domain.BridgeRejected, got.State, "a refusal the ledger will not change is a rejection, not a retry")
	require.NotNil(t, got.RejectionReason)
	assert.Contains(t, *got.RejectionReason, "no account mapping")

	got, _ = s.GetAccountingBridge(ctx, flakySub.BridgeID)
	assert.Equal(t, domain.BridgeSubmitted, got.State, "a transient failure leaves it queued")
	assert.Equal(t, 1, got.Attempts)
	require.NotNil(t, got.LastError)

	// Only SUBMITTED subjects are claimed; the posted and rejected ones are left alone.
	fl.calls.Store(0)
	fl.perSource = map[string]store.PostOutcome{flakySub.SourceEventID: {Err: "still down"}}
	_, err = s.DispatchPending(ctx, 10, fl.post, accEvents(tenant))
	require.NoError(t, err)
	assert.EqualValues(t, 1, fl.calls.Load())

	// After MaxPostingAttempts it waits for an operator instead of hammering the ledger.
	for i := 0; i < domain.MaxPostingAttempts; i++ {
		_, _ = s.DispatchPending(ctx, 10, fl.post, nil)
	}
	got, _ = s.GetAccountingBridge(ctx, flakySub.BridgeID)
	assert.Equal(t, domain.MaxPostingAttempts, got.Attempts)
	fl.calls.Store(0)
	_, _ = s.DispatchPending(ctx, 10, fl.post, nil)
	assert.Zero(t, fl.calls.Load(), "an exhausted subject is not retried until requeued")

	// The refused subject is requeued after the mapping is fixed, by a different principal.
	fl.perSource = map[string]store.PostOutcome{refusedSub.SourceEventID: {Posted: true, ExecutionID: "exec-2", JournalID: "jrn-2"}}
	rej, _ := s.GetAccountingBridge(ctx, refusedSub.BridgeID)
	re, err := s.SubmitAccountingBridge(ctx, rej.BridgeID, rej.Version, "p-3", accEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, 0, re.Attempts)
	assert.Nil(t, re.RejectionReason)
	n, err = s.DispatchPending(ctx, 10, fl.post, accEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, _ = s.GetAccountingBridge(ctx, refusedSub.BridgeID)
	assert.Equal(t, domain.BridgePosted, got.State)
}

func TestWFP05_APostedSubjectIsImmutableAndNeverDeleted(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	b, _, _ := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-9"), nil)
	sub, err := s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", nil)
	require.NoError(t, err)
	fl := &fakeLedger{outcome: posted()}
	_, err = s.DispatchPending(ctx, 10, fl.post, nil)
	require.NoError(t, err)

	for _, stmt := range []string{
		`UPDATE wfp_payroll_accounting_bridges SET total_debit = 1, total_credit = 1 WHERE bridge_id = $1`,
		`UPDATE wfp_payroll_accounting_bridges SET lines = '[]'::jsonb WHERE bridge_id = $1`,
		`UPDATE wfp_payroll_accounting_bridges SET fiscal_period = '2026-10' WHERE bridge_id = $1`,
		`UPDATE wfp_payroll_accounting_bridges SET source_event_id = 'other' WHERE bridge_id = $1`,
		`UPDATE wfp_payroll_accounting_bridges SET state = 'EVENT_CREATED' WHERE bridge_id = $1`,
		`UPDATE wfp_payroll_accounting_bridges SET state = 'REJECTED' WHERE bridge_id = $1`,
		`UPDATE wfp_payroll_accounting_bridges SET state = 'CORRECTED', correction_kind = 'REVERSED' WHERE bridge_id = $1`,
		`DELETE FROM wfp_payroll_accounting_bridges WHERE bridge_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, b.BridgeID)
		assert.Error(t, err, stmt)
	}
	_ = sub
}

func TestWFP05_ASupersededResultNeverReachesTheLedger(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	b, _, _ := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, v1.Ref.RunRefID, "p-9"), accEvents(tenant))
	sub, err := s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", accEvents(tenant))
	require.NoError(t, err)

	// The result is superseded AFTER submission and BEFORE the dispatcher runs.
	acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 2)

	fl := &fakeLedger{outcome: posted()}
	n, err := s.DispatchPending(ctx, 10, fl.post, accEvents(tenant))
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Zero(t, fl.calls.Load(), "a superseded result is never sent to the ledger")
	got, _ := s.GetAccountingBridge(ctx, sub.BridgeID)
	assert.Equal(t, domain.BridgeCorrected, got.State)
	require.NotNil(t, got.CorrectionKind)
	assert.Equal(t, domain.CorrectionWithdrawn, *got.CorrectionKind)
	assert.Contains(t, evTypes(t, pool, tenant), domain.EventPayrollAccountingCorrected)

	// And a superseded result cannot be submitted at all.
	v1b := acceptedResult(t, s, ctx, tenant, mid, "RUN-2", 1)
	b2, _, _ := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, v1b.Ref.RunRefID, "p-9"), nil)
	acceptedResult(t, s, ctx, tenant, mid, "RUN-2", 2)
	_, err = s.SubmitAccountingBridge(ctx, b2.BridgeID, b2.Version, "p-2", nil)
	assert.ErrorIs(t, err, domain.ErrResultNotPostable)
}

func TestWFP05_WithdrawOnlyForSupersededUnpostedSubjects(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	b, _, _ := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, v1.Ref.RunRefID, "p-9"), accEvents(tenant))

	_, err := s.WithdrawAccountingBridge(ctx, b.BridgeID, b.Version, "x", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeNotWithdrawable, "the result is still ACCEPTED")

	acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 2)
	_, err = s.WithdrawAccountingBridge(ctx, b.BridgeID, 99, "x", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeVersionConflict)
	w, err := s.WithdrawAccountingBridge(ctx, b.BridgeID, b.Version, "superseded by v2", accEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.BridgeCorrected, w.State)
	assert.Equal(t, domain.CorrectionWithdrawn, *w.CorrectionKind)

	_, err = s.WithdrawAccountingBridge(ctx, b.BridgeID, w.Version, "again", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeNotWithdrawable)
}

func TestWFP05_ReversalOfASupersededPostedSubject(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	b, _, _ := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, v1.Ref.RunRefID, "p-9"), accEvents(tenant))
	_, err := s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", accEvents(tenant))
	require.NoError(t, err)
	fl := &fakeLedger{outcome: store.PostOutcome{Posted: true, ExecutionID: "exec-A", JournalID: "jrn-A"}}
	_, err = s.DispatchPending(ctx, 10, fl.post, accEvents(tenant))
	require.NoError(t, err)

	// While the result is still ACCEPTED there is nothing to reverse.
	_, _, err = s.CreateReversalBridge(ctx, b.BridgeID, "x", "p-5", "corr", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeNotReversible)

	// The correction arrives: v2 supersedes v1.
	v2 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 2)
	rev, created, err := s.CreateReversalBridge(ctx, b.BridgeID, "retro correction", "p-5", "corr", accEvents(tenant))
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, domain.PurposeReversal, rev.PostingPurpose)
	assert.Equal(t, b.BridgeID, *rev.OriginalBridgeID)
	assert.Equal(t, b.TotalDebit, rev.TotalDebit, "the reversal mirrors the original's totals")
	assert.Empty(t, rev.Lines, "the ledger builds the reversing journal from the original journal")
	assert.Equal(t, domain.SourceEventID(b.ResultID, b.AccountingBookID, domain.PurposeReversal), rev.SourceEventID)
	again, created, err := s.CreateReversalBridge(ctx, b.BridgeID, "retro correction", "p-5", "corr", nil)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, rev.BridgeID, again.BridgeID, "one reversal per original")

	// The reversal is governed too: a different principal submits it.
	_, err = s.SubmitAccountingBridge(ctx, rev.BridgeID, rev.Version, "p-1", nil)
	assert.ErrorIs(t, err, domain.ErrSoDSamePrincipal)
	sub, err := s.SubmitAccountingBridge(ctx, rev.BridgeID, rev.Version, "p-2", accEvents(tenant))
	require.NoError(t, err)

	fl.outcome = store.PostOutcome{Posted: true, ExecutionID: "exec-R", JournalID: "jrn-R"}
	n, err := s.DispatchPending(ctx, 10, fl.post, accEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, domain.PurposeReversal, fl.lastKind)
	assert.Equal(t, "jrn-A", fl.lastOrig, "the ledger is told WHICH journal to reverse")

	gotRev, _ := s.GetAccountingBridge(ctx, sub.BridgeID)
	assert.Equal(t, domain.BridgePosted, gotRev.State)
	assert.Equal(t, "jrn-R", *gotRev.JournalID)
	orig, _ := s.GetAccountingBridge(ctx, b.BridgeID)
	assert.Equal(t, domain.BridgeCorrected, orig.State)
	assert.Equal(t, domain.CorrectionReversed, *orig.CorrectionKind)
	assert.Equal(t, "jrn-A", *orig.JournalID, "the original journal reference is kept as it was")

	// The replacement result gets its own subject, with its own journal.
	b2, created, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, v2.Ref.RunRefID, "p-9"), accEvents(tenant))
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEqual(t, b.SourceEventID, b2.SourceEventID)

	// A reversed subject cannot be reversed or withdrawn again.
	_, _, err = s.CreateReversalBridge(ctx, b.BridgeID, "x", "p-5", "corr", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeNotReversible)
	_, err = s.WithdrawAccountingBridge(ctx, b.BridgeID, orig.Version, "x", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeNotWithdrawable)

	assert.Contains(t, evTypes(t, pool, tenant), domain.EventPayrollAccountingCorrected)
}

func TestWFP05_RecordReconciliation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	b, _, _ := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-9"), nil)
	assert.Equal(t, domain.ReconNotReconciled, b.ReconciliationState)

	out, err := s.RecordReconciliation(ctx, b.BridgeID, domain.ReconMismatch, map[string]any{"differences": []string{"debits differ"}})
	require.NoError(t, err)
	assert.Equal(t, domain.ReconMismatch, out.ReconciliationState)
	require.NotNil(t, out.ReconciledAt)
	assert.Contains(t, string(out.ReconciliationDetail), "debits differ")
	assert.Equal(t, b.State, out.State, "reconciling never changes the posting state")

	_, err = s.RecordReconciliation(ctx, b.BridgeID, "WRONG", map[string]any{})
	assert.Error(t, err, "only the known reconciliation states are storable")
}

func TestWFP05_TenantIsolationAndMultiTenantDispatch(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, ctxA := wfpTenant()
	tenantB, ctxB := wfpTenant()
	mA, mB := registerManifest(t, s, ctxA), registerManifest(t, s, ctxB)
	resA := acceptedResult(t, s, ctxA, tenantA, mA, "RUN-1", 1)
	resB := acceptedResult(t, s, ctxB, tenantB, mB, "RUN-1", 1)
	bA, _, _ := s.CreateAccountingBridge(ctxA, buildBridge(t, s, ctxA, resA.Ref.RunRefID, "p-9"), nil)
	bB, _, _ := s.CreateAccountingBridge(ctxB, buildBridge(t, s, ctxB, resB.Ref.RunRefID, "p-9"), nil)

	_, err := s.GetAccountingBridge(ctxB, bA.BridgeID)
	assert.ErrorIs(t, err, domain.ErrBridgeNotFound)
	list, _ := s.ListAccountingBridges(ctxB, resA.Ref.RunRefID)
	assert.Empty(t, list)
	_, err = s.SubmitAccountingBridge(ctxB, bA.BridgeID, 1, "p-2", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeNotFound)
	_, _, err = s.CreateReversalBridge(ctxB, bA.BridgeID, "x", "p", "c", nil)
	assert.ErrorIs(t, err, domain.ErrBridgeNotFound)
	_, err = s.GetAccountingBridge(context.Background(), bA.BridgeID)
	assert.ErrorIs(t, err, domain.ErrIdentityMissing)

	// One dispatcher drains BOTH tenants' subjects, each under its own tenant.
	_, err = s.SubmitAccountingBridge(ctxA, bA.BridgeID, bA.Version, "p-2", nil)
	require.NoError(t, err)
	_, err = s.SubmitAccountingBridge(ctxB, bB.BridgeID, bB.Version, "p-2", nil)
	require.NoError(t, err)
	fl := &fakeLedger{outcome: posted()}
	fl.perSource = map[string]store.PostOutcome{
		bA.SourceEventID: {Posted: true, ExecutionID: "ea", JournalID: "ja"},
		bB.SourceEventID: {Posted: true, ExecutionID: "eb", JournalID: "jb"},
	}
	n, err := s.DispatchPending(context.Background(), 10, fl.post, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	gotA, _ := s.GetAccountingBridge(ctxA, bA.BridgeID)
	gotB, _ := s.GetAccountingBridge(ctxB, bB.BridgeID)
	assert.Equal(t, "ja", *gotA.JournalID, "each tenant's subject carries its own journal reference")
	assert.Equal(t, "jb", *gotB.JournalID)
}

func TestWFP05_EventsAreAtomicWithTheChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	boom := func(store.AccountingChange) ([]domain.OutboxEvent, error) { return nil, errors.New("boom") }

	_, _, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-9"), boom)
	require.Error(t, err)
	list, _ := s.ListAccountingBridges(ctx, res.Ref.RunRefID)
	assert.Empty(t, list, "no subject without its event")

	b, _, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-9"), nil)
	require.NoError(t, err)
	_, err = s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", boom)
	require.Error(t, err)
	got, _ := s.GetAccountingBridge(ctx, b.BridgeID)
	assert.Equal(t, domain.BridgeEventCreated, got.State, "a submit whose event cannot be written did not happen")

	// An uncatalogued event type rolls the change back as well.
	_, err = s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", func(ch store.AccountingChange) ([]domain.OutboxEvent, error) {
		return []domain.OutboxEvent{{TenantID: tenant, ObjectID: "x", EventType: "payroll_paid", Payload: []byte(`{}`)}}, nil
	})
	require.Error(t, err)
	got, _ = s.GetAccountingBridge(ctx, b.BridgeID)
	assert.Equal(t, domain.BridgeEventCreated, got.State)
}

// appPool connects as a NON-superuser that cannot bypass row-level security, which
// is how the service runs in production. The embedded superuser would skip every
// policy, so the dispatcher's cross-tenant claim is only really tested here.
func appPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	_, _ = admin.Exec(ctx, `DROP OWNED BY wfp_app_test`)
	_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS wfp_app_test`)
	_, err := admin.Exec(ctx, `CREATE ROLE wfp_app_test LOGIN PASSWORD 'wfp' NOSUPERUSER NOBYPASSRLS`)
	require.NoError(t, err)
	for _, q := range []string{
		`GRANT USAGE ON SCHEMA public TO wfp_app_test`,
		`GRANT ALL ON ALL TABLES IN SCHEMA public TO wfp_app_test`,
		`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO wfp_app_test`,
	} {
		_, err := admin.Exec(ctx, q)
		require.NoError(t, err, q)
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "wfp_app_test", "wfp"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return p
}

func TestWFP05_DispatcherWorksUnderRowLevelSecurity(t *testing.T) {
	admin := openTestPool(t)
	app := appPool(t, admin)
	s := store.New(app, "")
	tenantA, ctxA := wfpTenant()
	tenantB, ctxB := wfpTenant()
	mA, mB := registerManifest(t, s, ctxA), registerManifest(t, s, ctxB)
	resA := acceptedResult(t, s, ctxA, tenantA, mA, "RUN-1", 1)
	resB := acceptedResult(t, s, ctxB, tenantB, mB, "RUN-1", 1)
	bA, _, err := s.CreateAccountingBridge(ctxA, buildBridge(t, s, ctxA, resA.Ref.RunRefID, "p-9"), accEvents(tenantA))
	require.NoError(t, err)
	bB, _, err := s.CreateAccountingBridge(ctxB, buildBridge(t, s, ctxB, resB.Ref.RunRefID, "p-9"), accEvents(tenantB))
	require.NoError(t, err)

	// A tenant sees only its own subjects, enforced by the policy itself.
	tx, err := app.Begin(context.Background())
	require.NoError(t, err)
	_, err = tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", tenantA)
	require.NoError(t, err)
	var visible int
	require.NoError(t, tx.QueryRow(context.Background(), `SELECT count(*) FROM wfp_payroll_accounting_bridges`).Scan(&visible))
	require.NoError(t, tx.Rollback(context.Background()))
	assert.Equal(t, 1, visible, "row-level security confines a tenant to its own subjects")

	_, err = s.SubmitAccountingBridge(ctxA, bA.BridgeID, bA.Version, "p-2", accEvents(tenantA))
	require.NoError(t, err)
	_, err = s.SubmitAccountingBridge(ctxB, bB.BridgeID, bB.Version, "p-2", accEvents(tenantB))
	require.NoError(t, err)

	// The dispatcher is not a request: with no tenant it must still drain both.
	fl := &fakeLedger{perSource: map[string]store.PostOutcome{
		bA.SourceEventID: {Posted: true, ExecutionID: "ea", JournalID: "ja"},
		bB.SourceEventID: {Posted: true, ExecutionID: "eb", JournalID: "jb"},
	}}
	n, err := s.DispatchPending(context.Background(), 10, fl.post, func(ch store.AccountingChange) ([]domain.OutboxEvent, error) {
		return []domain.OutboxEvent{{TenantID: ch.Bridge.TenantID, ObjectID: ch.Bridge.ResultID, EventType: domain.EventPayrollAccountingPosted, Payload: []byte(`{}`)}}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both tenants' subjects posted by one dispatcher")

	gotA, err := s.GetAccountingBridge(ctxA, bA.BridgeID)
	require.NoError(t, err)
	assert.Equal(t, "ja", *gotA.JournalID)
	gotB, err := s.GetAccountingBridge(ctxB, bB.BridgeID)
	require.NoError(t, err)
	assert.Equal(t, "jb", *gotB.JournalID)
	assert.Equal(t, 1, countEvents(t, admin, tenantA, domain.EventPayrollAccountingPosted), "each tenant got its own posted event")
	assert.Equal(t, 1, countEvents(t, admin, tenantB, domain.EventPayrollAccountingPosted))

	// A request-path caller still cannot see another tenant's subject.
	_, err = s.GetAccountingBridge(ctxB, bA.BridgeID)
	assert.ErrorIs(t, err, domain.ErrBridgeNotFound)
}

func countEvents(t *testing.T, pool *pgxpool.Pool, tenant, typ string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM wfp_outbox WHERE tenant_id = $1 AND event_type = $2`, tenant, typ).Scan(&n))
	return n
}

// The Wave 4 result path (register, validate, store, supersede) also runs as a
// non-superuser: its triggers read the run reference and result tables, which
// row-level security must still let them see inside the same tenant context.
func TestWFP04_ResultPathWorksUnderRowLevelSecurity(t *testing.T) {
	admin := openTestPool(t)
	app := appPool(t, admin)
	s := store.New(app, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)

	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	v2 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 2)
	require.NotNil(t, v2.Superseded)
	assert.Equal(t, v1.Ref.RunRefID, v2.Superseded.RunRefID)

	b, _, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, v2.Ref.RunRefID, "p-9"), accEvents(tenant))
	require.NoError(t, err)
	sub, err := s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", accEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.BridgeSubmitted, sub.State)

	// Separation of duties and result guards hold under RLS too.
	_, err = s.SubmitAccountingBridge(ctx, b.BridgeID, sub.Version, "p-1", nil)
	assert.Error(t, err)
}
