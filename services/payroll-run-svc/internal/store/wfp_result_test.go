package store_test

import (
	"context"
	"errors"
	"math/big"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

// Real-Postgres tests for WFP-04. The embedded superuser bypasses RLS, so tenant
// isolation proves the explicit tenant_id predicate; database guards use direct SQL.

func rcomp(code, kind, amount string) domain.ResultComponentInput {
	return domain.ResultComponentInput{Code: code, Kind: kind, Amount: amount}
}

// resultWorker reconciles exactly: gross 3100.50 = 3000 + 100.50, deductions 700.10 = 600.10 + 100,
// employer 250, net 2400.40.
func resultWorker(id string) domain.ResultWorkerInput {
	return domain.ResultWorkerInput{
		WorkerID: id, Currency: "GBP", Gross: "3100.50", TaxablePay: "3000", EmployeeDeductions: "700.10", EmployerLiabilities: "250", NetPay: "2400.40",
		Components: []domain.ResultComponentInput{
			rcomp("BASE", "EARNING", "3000"), rcomp("HRA", "EARNING", "100.5"), rcomp("TAX", "EMPLOYEE_TAX", "600.10"),
			rcomp("PEN", "EMPLOYEE_PENSION", "100"), rcomp("NI", "EMPLOYER_SOCIAL", "250"),
		},
	}
}

func resultRequest(manifestID, runID string, version int, workers ...domain.ResultWorkerInput) domain.ReceivePayrollResultRequest {
	n := len(workers)
	mul := func(unit float64) string { return domain.FormatDecimal(mulRat(unit, n)) }
	return domain.ReceivePayrollResultRequest{
		LegalEntityID: "le-1", SourceSystemID: "zp", SourceRunID: runID, SourceRunVersion: version, RunKind: "REGULAR",
		PayrollPeriodID: "2026-09", PayDate: "2026-09-30", RuleManifestID: manifestID, SourceRunStatus: "FINAL",
		SourceApprovalRef: "approval-1", CalculationRef: "engine-1", Currency: "GBP", Workers: workers,
		RunTotals: domain.ResultTotalsInput{
			WorkerCount: n, Gross: mul(3100.50), TaxablePay: mul(3000), EmployeeDeductions: mul(700.10),
			EmployerLiabilities: mul(250), NetPay: mul(2400.40),
		},
	}
}

func okContexts(req domain.ReceivePayrollResultRequest) map[string]domain.ResultWorkerContext {
	out := map[string]domain.ResultWorkerContext{}
	for _, w := range req.Workers {
		out[w.WorkerID] = domain.ResultWorkerContext{Valid: true, PayCurrencies: []string{"GBP"}}
	}
	return out
}

func resultEvents(tenant string) store.ResultEventBuilder {
	return func(ch store.ResultChange) ([]domain.OutboxEvent, error) {
		mk := func(ref *domain.PayrollRunRef, typ string) domain.OutboxEvent {
			return domain.OutboxEvent{TenantID: tenant, ObjectID: ref.SourceRunID, EventType: typ, Payload: []byte(`{}`)}
		}
		var out []domain.OutboxEvent
		if ch.Registered {
			out = append(out, mk(ch.Ref, domain.EventPayrollResultReceived))
		}
		if ch.Result.Outcome == domain.ResultAccepted {
			out = append(out, mk(ch.Ref, domain.EventPayrollResultAccepted))
		} else {
			out = append(out, mk(ch.Ref, domain.EventPayrollResultRejected))
		}
		if ch.Superseded != nil {
			out = append(out, mk(ch.Superseded, domain.EventPayrollResultSuperseded))
		}
		return out, nil
	}
}

func prepare(tenant string, req domain.ReceivePayrollResultRequest) store.ReceiveResultInput {
	ref := domain.PayrollRunRef{
		RunRefID: uuid.NewString(), LegalEntityID: req.LegalEntityID, SourceSystemID: req.SourceSystemID, SourceRunID: req.SourceRunID,
		SourceRunVersion: req.SourceRunVersion, RunKind: req.RunKind, PayrollPeriodID: req.PayrollPeriodID, PayDate: req.PayDate,
		RuleManifestID: req.RuleManifestID, IdentityFingerprint: domain.RunIdentityFingerprint(tenant, req.RunRef()),
		CorrelationID: "corr", CreatedBy: "p-1",
	}
	return store.ReceiveResultInput{
		Ref: ref, Request: req, Evaluation: domain.EvaluateResult(tenant, req, okContexts(req), ""), ReceivedBy: "p-1", CorrelationID: "corr",
	}
}

func registerManifest(t *testing.T, s *store.PgStore, ctx context.Context) string {
	t.Helper()
	m := newManifest("zp", "GB", "v-"+uuid.NewString()[:6], "a", "2026-01-01", nil)
	_, err := s.RegisterRuleManifest(ctx, m, nil)
	require.NoError(t, err)
	return m.ManifestID
}

func rEventTypes(t *testing.T, pool *pgxpool.Pool, tenant string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT event_type FROM wfp_outbox WHERE tenant_id = $1 ORDER BY outbox_id`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func TestWFP04_AcceptStoresTheResultAndItsDetailAtomically(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	w1, w2 := uuid.NewString(), uuid.NewString()
	req := resultRequest(mid, "RUN-1", 1, resultWorker(w1), resultWorker(w2))
	req.EvidenceFlags = []string{"PLACEHOLDER_TAX_RATE"}

	out, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
	require.NoError(t, err)
	assert.True(t, out.Created)
	assert.Equal(t, domain.AcceptanceAccepted, out.Ref.AcceptanceState)
	require.NotNil(t, out.Ref.ResultFingerprint)
	assert.Equal(t, out.Result.ResultFingerprint, *out.Ref.ResultFingerprint)
	assert.Equal(t, domain.ResultAccepted, out.Result.Outcome)
	assert.Equal(t, 2, out.Result.WorkerCount)
	assert.Equal(t, "6201.0000", out.Result.TotalGross)
	assert.Equal(t, "4800.8000", out.Result.TotalNetPay)
	assert.Equal(t, []string{"PLACEHOLDER_TAX_RATE"}, out.Result.EvidenceFlags)
	assert.Equal(t, "6201.0000", out.Result.DeclaredTotals.Gross)
	assert.Empty(t, out.Result.RejectionReasons)

	ws, err := s.ListResultWorkers(ctx, out.Ref.RunRefID)
	require.NoError(t, err)
	require.Len(t, ws, 2)
	assert.Equal(t, "2400.4000", ws[0].NetPay)
	cs, err := s.ListResultComponents(ctx, out.Ref.RunRefID, w1)
	require.NoError(t, err)
	require.Len(t, cs, 5)
	assert.Equal(t, "100.5000", cs[1].Amount, "amounts are stored as exact decimals")
	all, _ := s.ListResultComponents(ctx, out.Ref.RunRefID, "")
	assert.Len(t, all, 10)

	diffs, err := s.RecomputeResultTotals(ctx, out.Ref.RunRefID)
	require.NoError(t, err)
	assert.Empty(t, diffs, "the stored result still reconciles exactly from its own rows")

	assert.Equal(t, []string{domain.EventPayrollResultReceived, domain.EventPayrollResultAccepted}, rEventTypes(t, pool, tenant))
}

func TestWFP04_RejectedResultKeepsItsReasonsAsEvidenceAndNoDetail(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	w := uuid.NewString()
	bad := resultWorker(w)
	bad.NetPay = "2400.41"
	req := resultRequest(mid, "RUN-1", 1, bad)
	req.RunTotals.NetPay = "2400.41"

	out, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
	require.NoError(t, err)
	assert.True(t, out.Created)
	assert.Equal(t, domain.AcceptanceRejected, out.Ref.AcceptanceState)
	assert.Equal(t, domain.ResultRejected, out.Result.Outcome)
	require.Len(t, out.Result.RejectionReasons, 1)
	assert.Equal(t, domain.RsNetPayMismatch, out.Result.RejectionReasons[0].Code)
	assert.Equal(t, w, out.Result.RejectionReasons[0].WorkerID)
	assert.Nil(t, out.Ref.ResultFingerprint, "a rejected run version carries no acceptance fingerprint")

	ws, _ := s.ListResultWorkers(ctx, out.Ref.RunRefID)
	assert.Empty(t, ws, "a rejected result stores no worker detail")
	assert.Equal(t, []string{domain.EventPayrollResultReceived, domain.EventPayrollResultRejected}, rEventTypes(t, pool, tenant))

	// The same rejected result again is the same rejection, not a second one.
	again, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
	require.NoError(t, err)
	assert.False(t, again.Created)
	assert.Equal(t, out.Result.ResultID, again.Result.ResultID)

	// A corrected submission under the SAME version is a conflict: a changed result
	// is a new version.
	fixed := resultRequest(mid, "RUN-1", 1, resultWorker(w))
	_, err = s.ReceivePayrollResult(ctx, prepare(tenant, fixed), resultEvents(tenant))
	assert.ErrorIs(t, err, domain.ErrResultFingerprintConflict)

	// ...which is accepted as version 2, with no supersession (v1 was never accepted).
	v2 := resultRequest(mid, "RUN-1", 2, resultWorker(w))
	ok, err := s.ReceivePayrollResult(ctx, prepare(tenant, v2), resultEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.AcceptanceAccepted, ok.Ref.AcceptanceState)
	assert.Nil(t, ok.Superseded)
	chains, _, err := s.CorrectionChains(ctx, out.Ref.RunRefID)
	require.NoError(t, err)
	assert.Empty(t, chains)
}

func TestWFP04_ReplayIsIdempotentAndChangedContentIsAConflict(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	w := uuid.NewString()
	req := resultRequest(mid, "RUN-1", 1, resultWorker(w))

	first, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
	require.NoError(t, err)

	// A provider retry: spelled differently ("3100.5"), same content.
	retry := resultRequest(mid, "RUN-1", 1, resultWorker(w))
	retry.Workers[0].Gross = "3100.5"
	replay, err := s.ReceivePayrollResult(ctx, prepare(tenant, retry), resultEvents(tenant))
	require.NoError(t, err)
	assert.False(t, replay.Created)
	assert.Equal(t, first.Result.ResultID, replay.Result.ResultID)
	assert.Len(t, rEventTypes(t, pool, tenant), 2, "a replay emits nothing")

	// The same run version with a changed amount: refused (WFP-NP-015).
	changed := resultRequest(mid, "RUN-1", 1, resultWorker(w))
	changed.Workers[0].Components[1].Amount = "99"
	_, err = s.ReceivePayrollResult(ctx, prepare(tenant, changed), resultEvents(tenant))
	assert.ErrorIs(t, err, domain.ErrResultFingerprintConflict)

	// ...and the identity itself cannot be reused with a different rule manifest.
	otherManifest := registerManifest(t, s, ctx)
	clash := resultRequest(otherManifest, "RUN-1", 1, resultWorker(w))
	_, err = s.ReceivePayrollResult(ctx, prepare(tenant, clash), resultEvents(tenant))
	assert.ErrorIs(t, err, domain.ErrRunIdentityConflict)

	// Version 3 cannot skip version 2.
	_, err = s.ReceivePayrollResult(ctx, prepare(tenant, resultRequest(mid, "RUN-1", 3, resultWorker(w))), resultEvents(tenant))
	assert.ErrorIs(t, err, domain.ErrPriorVersionMissing)
}

func TestWFP04_ACorrectionSupersedesThroughAChainAndNeverEditsTheOriginal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	w := uuid.NewString()

	v1, err := s.ReceivePayrollResult(ctx, prepare(tenant, resultRequest(mid, "RUN-1", 1, resultWorker(w))), resultEvents(tenant))
	require.NoError(t, err)

	// v2: a retro correction (net pay differs, still reconciles).
	fixed := resultWorker(w)
	fixed.Gross, fixed.NetPay = "3200.50", "2500.40"
	fixed.Components[1].Amount = "200.5"
	v2req := resultRequest(mid, "RUN-1", 2, fixed)
	v2req.RunKind = "CORRECTION"
	v2req.RunTotals = domain.ResultTotalsInput{WorkerCount: 1, Gross: "3200.50", TaxablePay: "3000", EmployeeDeductions: "700.10", EmployerLiabilities: "250", NetPay: "2500.40"}
	in := prepare(tenant, v2req)
	in.SupersedeReason = "retro bonus"
	v2, err := s.ReceivePayrollResult(ctx, in, resultEvents(tenant))
	require.NoError(t, err)

	assert.Equal(t, domain.AcceptanceAccepted, v2.Ref.AcceptanceState)
	require.NotNil(t, v2.Superseded)
	assert.Equal(t, v1.Ref.RunRefID, v2.Superseded.RunRefID)
	assert.Equal(t, domain.AcceptanceSuperseded, v2.Superseded.AcceptanceState)
	require.NotNil(t, v2.Ref.SupersedesRunRefID)
	assert.Equal(t, v1.Ref.RunRefID, *v2.Ref.SupersedesRunRefID)

	asOriginal, _, err := s.CorrectionChains(ctx, v1.Ref.RunRefID)
	require.NoError(t, err)
	require.Len(t, asOriginal, 1)
	assert.Equal(t, v2.Ref.RunRefID, asOriginal[0].SupersedingRunRefID)
	assert.Equal(t, v1.Result.ResultID, asOriginal[0].OriginalResultID)
	assert.Equal(t, "retro bonus", asOriginal[0].Reason)
	_, asSuperseding, _ := s.CorrectionChains(ctx, v2.Ref.RunRefID)
	require.Len(t, asSuperseding, 1)

	// The original result is exactly as it was received.
	orig, err := s.GetPayrollResultByRef(ctx, v1.Ref.RunRefID)
	require.NoError(t, err)
	assert.Equal(t, "3100.5000", orig.TotalGross)
	assert.Equal(t, "2400.4000", orig.TotalNetPay)
	ws, _ := s.ListResultWorkers(ctx, v1.Ref.RunRefID)
	require.Len(t, ws, 1)
	assert.Equal(t, "2400.4000", ws[0].NetPay)
	cur, _ := s.GetPayrollResultByRef(ctx, v2.Ref.RunRefID)
	assert.Equal(t, "2500.4000", cur.TotalNetPay)

	assert.Equal(t, []string{
		domain.EventPayrollResultReceived, domain.EventPayrollResultAccepted,
		domain.EventPayrollResultReceived, domain.EventPayrollResultAccepted, domain.EventPayrollResultSuperseded,
	}, rEventTypes(t, pool, tenant))

	// Nothing can reach back and change history.
	for _, stmt := range []string{
		`UPDATE wfp_payroll_results SET total_gross = 1 WHERE run_ref_id = $1`,
		`DELETE FROM wfp_payroll_results WHERE run_ref_id = $1`,
		`UPDATE wfp_payroll_result_workers SET net_pay = 0 WHERE result_id = (SELECT result_id FROM wfp_payroll_results WHERE run_ref_id = $1)`,
		`DELETE FROM wfp_payroll_result_workers WHERE result_id = (SELECT result_id FROM wfp_payroll_results WHERE run_ref_id = $1)`,
		`UPDATE wfp_payroll_result_components SET amount = 1 WHERE result_id = (SELECT result_id FROM wfp_payroll_results WHERE run_ref_id = $1)`,
		`DELETE FROM wfp_payroll_result_components WHERE result_id = (SELECT result_id FROM wfp_payroll_results WHERE run_ref_id = $1)`,
		`UPDATE wfp_payroll_correction_chains SET reason = 'x' WHERE original_run_ref_id = $1`,
		`DELETE FROM wfp_payroll_correction_chains WHERE original_run_ref_id = $1`,
		`UPDATE wfp_payroll_run_refs SET acceptance_state = 'ACCEPTED' WHERE run_ref_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, v1.Ref.RunRefID)
		assert.Error(t, err, stmt)
	}

	// v3 supersedes v2, and v1 stays SUPERSEDED (one chain link per version).
	v3req := resultRequest(mid, "RUN-1", 3, resultWorker(w))
	v3, err := s.ReceivePayrollResult(ctx, prepare(tenant, v3req), resultEvents(tenant))
	require.NoError(t, err)
	require.NotNil(t, v3.Superseded)
	assert.Equal(t, v2.Ref.RunRefID, v3.Superseded.RunRefID)
}

func TestWFP04_TheDatabaseRefusesAnAcceptedResultThatDoesNotReconcile(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	ref := newRunRef(tenant, "zp", "RUN-X", 1, mid, "2026-09")
	_, err := s.RegisterPayrollRunRef(ctx, ref, nil)
	require.NoError(t, err)
	_, err = s.TransitionPayrollRunRef(ctx, ref.RunRefID, 1, domain.AcceptanceValidating, "", nil)
	require.NoError(t, err)

	insert := func(gross, ded, net, status string) error {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO wfp_payroll_results (result_id, tenant_id, run_ref_id, legal_entity_id, outcome, currency, source_run_status,
				source_approval_ref, calculation_ref, worker_count, total_gross, total_taxable_pay, total_employee_deductions,
				total_employer_liabilities, total_net_pay, declared_totals, result_fingerprint, received_by, correlation_id)
			VALUES (gen_random_uuid(), $1, $2::uuid, 'le-1', 'ACCEPTED', 'GBP', $6, 'a', 'c', 0, $3::numeric, 0, $4::numeric, 0, $5::numeric,
			        '{}'::jsonb, 'fp', 'p', 'c')`, tenant, ref.RunRefID, gross, ded, net, status)
		return err
	}
	assert.Error(t, insert("100", "10", "91", "FINAL"), "gross - deductions must equal net")
	assert.Error(t, insert("100", "10", "90", "DRAFT"), "an ACCEPTED result must come from a FINAL/APPROVED source run")
	assert.NoError(t, insert("100", "10", "90", "FINAL"))
}

func TestWFP04_PreRegisteredRunRefStillGetsItsResult(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	w := uuid.NewString()
	req := resultRequest(mid, "RUN-1", 1, resultWorker(w))

	pre := newRunRef(tenant, "zp", "RUN-1", 1, mid, "2026-09")
	_, err := s.RegisterPayrollRunRef(ctx, pre, nil)
	require.NoError(t, err)

	out, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
	require.NoError(t, err)
	assert.True(t, out.Created)
	assert.Equal(t, pre.RunRefID, out.Ref.RunRefID, "the pre-registered run reference is the one that is decided")
	assert.Equal(t, domain.AcceptanceAccepted, out.Ref.AcceptanceState)
	assert.Equal(t, []string{domain.EventPayrollResultAccepted}, rEventTypes(t, pool, tenant), "received was not re-emitted: the reference already existed")
}

func TestWFP04_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, ctxA := wfpTenant()
	_, ctxB := wfpTenant()
	mid := registerManifest(t, s, ctxA)
	w := uuid.NewString()
	out, err := s.ReceivePayrollResult(ctxA, prepare(tenantA, resultRequest(mid, "RUN-1", 1, resultWorker(w))), nil)
	require.NoError(t, err)

	_, err = s.GetPayrollResultByRef(ctxB, out.Ref.RunRefID)
	assert.ErrorIs(t, err, domain.ErrResultNotFound)
	ws, _ := s.ListResultWorkers(ctxB, out.Ref.RunRefID)
	assert.Empty(t, ws)
	cs, _ := s.ListResultComponents(ctxB, out.Ref.RunRefID, "")
	assert.Empty(t, cs)
	_, err = s.RecomputeResultTotals(ctxB, out.Ref.RunRefID)
	assert.ErrorIs(t, err, domain.ErrResultNotFound)
	v, err := s.LatestRunRefVersion(ctxB, "le-1", "zp", "RUN-1")
	require.NoError(t, err)
	assert.Zero(t, v)
	_, err = s.GetPayrollResultByRef(context.Background(), out.Ref.RunRefID)
	assert.ErrorIs(t, err, domain.ErrIdentityMissing)

	v, err = s.LatestRunRefVersion(ctxA, "le-1", "zp", "RUN-1")
	require.NoError(t, err)
	assert.Equal(t, 1, v)
}

func TestWFP04_OutboxIsAtomicWithTheDecision(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	w := uuid.NewString()
	req := resultRequest(mid, "RUN-1", 1, resultWorker(w))

	_, err := s.ReceivePayrollResult(ctx, prepare(tenant, req),
		func(store.ResultChange) ([]domain.OutboxEvent, error) { return nil, errors.New("boom") })
	require.Error(t, err)
	refs, _ := s.ListPayrollRunRefs(ctx, store.RunRefFilter{})
	assert.Empty(t, refs, "no run reference, result or detail without its events")
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM wfp_payroll_results WHERE tenant_id = $1`, tenant).Scan(&n))
	assert.Zero(t, n)

	_, err = s.ReceivePayrollResult(ctx, prepare(tenant, req), func(ch store.ResultChange) ([]domain.OutboxEvent, error) {
		return []domain.OutboxEvent{{TenantID: tenant, ObjectID: "x", EventType: "payroll_paid", Payload: []byte(`{}`)}}, nil
	})
	require.Error(t, err, "an uncatalogued event rolls the decision back")
	refs, _ = s.ListPayrollRunRefs(ctx, store.RunRefFilter{})
	assert.Empty(t, refs)

	out, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
	require.NoError(t, err)
	assert.True(t, out.Created)
}

func TestWFP04_CorrectionRunVersionLookupIsReplaySafe(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	w := uuid.NewString()
	correctionRun := uuid.NewString()

	_, err := s.ReceivePayrollResult(ctx, prepare(tenant, resultRequest(mid, "RUN-1", 1, resultWorker(w))), nil)
	require.NoError(t, err)
	_, found, err := s.RunRefVersionForPayrollRun(ctx, "le-1", "zp", "RUN-1", correctionRun)
	require.NoError(t, err)
	assert.False(t, found)

	req := resultRequest(mid, "RUN-1", 2, resultWorker(w))
	req.PayrollRunID = &correctionRun
	in := prepare(tenant, req)
	in.Ref.PayrollRunID = &correctionRun
	_, err = s.ReceivePayrollResult(ctx, in, nil)
	require.NoError(t, err)

	v, found, err := s.RunRefVersionForPayrollRun(ctx, "le-1", "zp", "RUN-1", correctionRun)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 2, v, "promoting the same correction run again reuses its version")
}

// ── legacy run lifecycle events now go through the outbox ────────────────────

func runEventBuilder(tenant, corr string) store.RunEventFunc {
	return func(run domain.PayrollRun) (domain.OutboxEvent, error) {
		return domain.OutboxEvent{TenantID: tenant, ObjectID: run.RunID, EventType: domain.EventRunInitiated, Payload: []byte(`{}`)}, nil
	}
}

func TestLegacyRunEvents_AreWrittenInTheSameTransactionAsTheStateChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	typed := func(typ string) context.Context {
		return store.WithRunEvent(ctx, func(run domain.PayrollRun) (domain.OutboxEvent, error) {
			return domain.OutboxEvent{TenantID: tenant, ObjectID: run.RunID, EventType: typ, Payload: []byte(`{}`)}, nil
		})
	}

	run := newTestRun(tenant, "corr-ev-1")
	created, err := s.CreatePayrollRun(typed(domain.EventRunInitiated), run)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, []string{domain.EventRunInitiated}, rEventTypes(t, pool, tenant))

	// A retried POST (same correlation id) returns the original run and emits nothing.
	dup := newTestRun(tenant, "corr-ev-1")
	created, err = s.CreatePayrollRun(typed(domain.EventRunInitiated), dup)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Len(t, rEventTypes(t, pool, tenant), 1, "a replay must not re-publish initiated")

	require.NoError(t, s.SaveCalculatedResults(typed(domain.EventRunCalculated), run.RunID, 100, 80, 20, 0, nil, nil))
	require.NoError(t, s.FinalizePayrollRun(typed(domain.EventRunCompleted), run.RunID, nil))
	assert.Equal(t, []string{domain.EventRunInitiated, domain.EventRunCalculated, domain.EventRunCompleted}, rEventTypes(t, pool, tenant))

	// The event is built from the run AS CHANGED (the calculated totals, the final status).
	var status string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT status FROM payroll_runs WHERE run_id = $1`, run.RunID).Scan(&status))
	assert.Equal(t, "COMPLETED", status)

	// A BLOCKED event has no state change of its own.
	require.NoError(t, s.EnqueueRunEvent(ctx, *run, func(r domain.PayrollRun) (domain.OutboxEvent, error) {
		return domain.OutboxEvent{TenantID: tenant, ObjectID: r.RunID, EventType: domain.EventRunBlocked, Payload: []byte(`{}`)}, nil
	}))
	assert.Equal(t, domain.EventRunBlocked, rEventTypes(t, pool, tenant)[3])
}

func TestLegacyRunEvents_AFailedEventRollsTheChangeBackAndNoEventMeansNoChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()

	failing := store.WithRunEvent(ctx, func(domain.PayrollRun) (domain.OutboxEvent, error) { return domain.OutboxEvent{}, errors.New("boom") })
	run := newTestRun(tenant, "corr-ev-2")
	_, err := s.CreatePayrollRun(failing, run)
	require.Error(t, err)
	_, err = s.GetPayrollRun(ctx, run.RunID)
	assert.ErrorIs(t, err, domain.ErrPayrollRunNotFound, "no run without its event")

	uncatalogued := store.WithRunEvent(ctx, func(r domain.PayrollRun) (domain.OutboxEvent, error) {
		return domain.OutboxEvent{TenantID: tenant, ObjectID: r.RunID, EventType: "payroll.run.exploded", Payload: []byte(`{}`)}, nil
	})
	_, err = s.CreatePayrollRun(uncatalogued, run)
	require.Error(t, err)
	_, err = s.GetPayrollRun(ctx, run.RunID)
	assert.ErrorIs(t, err, domain.ErrPayrollRunNotFound)

	// Without an attached event the legacy behaviour is unchanged.
	plain := newTestRun(tenant, "corr-ev-3")
	created, err := s.CreatePayrollRun(ctx, plain)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Empty(t, rEventTypes(t, pool, tenant))
}

// mulRat multiplies a 2-decimal figure by n exactly, for building declared totals.
func mulRat(unit float64, n int) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(unit, 'f', 2, 64))
	return r.Mul(r, big.NewRat(int64(n), 1))
}
