//go:build integration

package store_test

// SPEC_DEVIATIONS A7 against a real Postgres: late data after certification
// (ZS-CONTROL-001 Invariant 14, scenarios 05 and 21).

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/engine"
	"zoiko.io/financial-control-svc/internal/store"
)

// tenantSweep is the real store with candidate selection narrowed to one
// tenant. The sweep is cross-tenant, and a test's fake sources answer for any
// tenant, so an unscoped sweep would re-read other tests' runs through this
// test's sources and open late-data exceptions on their data.
type tenantSweep struct {
	*store.PgStore
	tenant string
}

func (s tenantSweep) LateDataCandidates(c context.Context, checkedBefore time.Time, batch int) ([]store.RunRef, error) {
	all, err := s.PgStore.LateDataCandidates(c, checkedBefore, 1_000_000)
	var out []store.RunRef
	for _, r := range all {
		if r.TenantID == s.tenant && len(out) < batch {
			out = append(out, r)
		}
	}
	return out, err
}

func sweeper(w *wave2) *engine.LateDataSweeper {
	return engine.NewLateDataSweeper(tenantSweep{testStore, w.tenant}, w.fetch, zap.NewNop(), 0)
}

// sweepAll runs the late-data sweep over every eligible run of w's tenant.
func sweepAll(t *testing.T, w *wave2) engine.LateDataResult {
	t.Helper()
	res, err := sweeper(w).Sweep(ctx, 100)
	require.NoError(t, err)
	require.Zero(t, res.Errors)
	return res
}

// lateExceptions are the late-data exceptions opened on a run.
func lateExceptions(t *testing.T, w *wave2, runID string) []domain.ControlException {
	t.Helper()
	all, err := testStore.ListExceptions(ctx, w.tenant, runID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	var out []domain.ControlException
	for _, e := range all {
		if e.ReasonCode == domain.ReasonLateDataAfterCertification {
			out = append(out, e)
		}
	}
	return out
}

func gateItem(t *testing.T, w *wave2, code string) (domain.CloseGate, domain.CloseGateItem) {
	t.Helper()
	g, err := testStore.CloseGate(ctx, w.tenant, w.entity, "2026-09")
	require.NoError(t, err)
	for _, it := range g.Items {
		if it.ControlCode == code {
			return *g, it
		}
	}
	t.Fatalf("no gate item for %s", code)
	return *g, domain.CloseGateItem{}
}

func checkRow(t *testing.T, runID string) (outcome string, observed, reported *string, errText string) {
	t.Helper()
	require.NoError(t, testPool.QueryRow(ctx, `SELECT outcome, observed_watermark, reported_watermark, error
		FROM control_watermark_checks WHERE run_id = $1 AND side = 'A'`, runID).Scan(&outcome, &observed, &reported, &errText))
	return
}

// certifiedState is everything about a certified run that must never change:
// the run row, its history, its frozen population and its certification.
func certifiedState(t *testing.T, runID string) []string {
	t.Helper()
	var out []string
	for _, q := range []string{
		`SELECT row_to_json(r)::text FROM control_runs r WHERE run_id = $1`,
		`SELECT COALESCE(json_agg(t ORDER BY transition_id)::text, '') FROM control_run_transitions t WHERE run_id = $1`,
		`SELECT COALESCE(json_agg(p ORDER BY side)::text, '') FROM population_snapshots p WHERE run_id = $1`,
		`SELECT COALESCE(json_agg(r ORDER BY seq)::text, '') FROM population_records r
			JOIN population_snapshots p ON p.population_id = r.population_id WHERE p.run_id = $1`,
		`SELECT COALESCE(json_agg(c)::text, '') FROM control_certifications c WHERE run_id = $1`,
		`SELECT COALESCE(json_agg(e ORDER BY sealed_at)::text, '') FROM evidence_packages e WHERE run_id = $1`,
	} {
		var s string
		require.NoError(t, testPool.QueryRow(ctx, q, runID).Scan(&s))
		out = append(out, s)
	}
	return out
}

// waiveLate has the exception owner investigate it and an independent principal
// waive it under authority, with the root cause a HIGH exception needs.
func waiveLate(t *testing.T, w *wave2, e domain.ControlException) {
	t.Helper()
	a, err := testStore.AssignException(ctx, w.tenant, e.ExceptionID, "lead", "c", e.Version,
		domain.AssignExceptionRequest{OwnerPrincipalID: "olivia", Reason: "owner"})
	require.NoError(t, err)
	e = *a
	_, err = step(t, w, &e, "olivia", domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
	require.NoError(t, err)
	_, err = step(t, w, &e, "sam", domain.ResolveExceptionRequest{ToState: "WAIVED_UNDER_AUTHORITY",
		Reason: "late journal is immaterial to the certified balance", AuthorityRef: "waiver:memo-9",
		RootCauseCode: "LATE_SUBLEDGER_FEED", RootCauseNote: "journal posted after cut-off"})
	require.NoError(t, err)
}

func TestLateData_MovedWatermarkOpensOneExceptionAndBlocksTheGate(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	w.src.set(catalogue.SysGL, "control-account-postings", "wm")
	w.src.set(catalogue.SysGL, "manual-journals", "wm", attrs(r("m1", "m1", "10"), "review_gap", ""))
	var run020 *domain.ControlRun
	for _, code := range []string{"FIN-CTRL-020", "FIN-CTRL-021", "FIN-CTRL-023"} {
		_, final := w.run(code, fiscal)
		out, err := certifyRun(ctx, w.tenant, final.RunID, "checker", "c", final.Version, domain.CertifyRequest{Decision: "CERTIFY"}, testAuthority("checker", w.tenant))
		require.NoError(t, err, code)
		if code == "FIN-CTRL-020" {
			run020 = out
		}
	}
	g, _ := gateItem(t, w, "FIN-CTRL-020")
	require.True(t, g.Open)

	// Nothing has changed at the source: the check records CURRENT and opens nothing.
	sweepAll(t, w)
	outcome, observed, reported, _ := checkRow(t, run020.RunID)
	assert.Equal(t, store.WatermarkCurrent, outcome)
	require.NotNil(t, observed)
	assert.Equal(t, "wm", *observed)
	assert.Nil(t, reported)
	assert.Empty(t, lateExceptions(t, w, run020.RunID))

	// The re-read is page 1 with limit=1, made as the run's executor, through the
	// same request the population was built with (fiscal_period bound from scope,
	// period_id withheld as the real GL source requires).
	w.src.mu.Lock()
	q, h := w.src.queries[catalogue.SysGL], w.src.requests[len(w.src.requests)-1]
	w.src.mu.Unlock()
	assert.Equal(t, "1", q.Get("limit"))
	assert.False(t, q.Has("cursor"))
	assert.False(t, q.Has("period_id"))
	assert.Equal(t, w.entity, q.Get("legal_entity_id"))
	assert.Equal(t, "prep", h.Get("X-Principal-Id"))
	assert.Equal(t, w.tenant, h.Get("X-Tenant-Id"))

	before := certifiedState(t, run020.RunID)

	// A late journal arrives after certification.
	w.src.set(catalogue.SysGL, "journal-balances", "wm-2", journalBal("j1", "5", "5"), journalBal("j2", "1", "1"))

	// Three replicas sweep at once: the run lock makes it one exception.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := sweeper(w).Sweep(ctx, 100)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	sweepAll(t, w) // and a later sweep still sees it as reported

	late := lateExceptions(t, w, run020.RunID)
	require.Len(t, late, 1)
	e := late[0]
	assert.Equal(t, "LATE_DATA", e.Category)
	assert.Equal(t, domain.ExOpen, e.State)
	assert.Equal(t, domain.SeverityHigh, e.Severity)
	assert.Equal(t, domain.SideA, e.Side)
	assert.Contains(t, e.Detail, `"wm-2"`)
	snaps, err := testStore.ListPopulationSnapshots(ctx, w.tenant, run020.RunID)
	require.NoError(t, err)
	assert.Equal(t, []string{snaps[0].PopulationID}, e.RecordIDs)
	outcome, observed, reported, _ = checkRow(t, run020.RunID)
	assert.Equal(t, store.WatermarkMoved, outcome)
	assert.Equal(t, "wm-2", *observed)
	require.NotNil(t, reported)
	assert.Equal(t, "wm-2", *reported)
	for _, other := range []string{"FIN-CTRL-021", "FIN-CTRL-023"} {
		_, it := gateItem(t, w, other)
		assert.Equal(t, domain.GatePassed, it.Status, "%s's source did not move", other)
	}

	// The opening is recorded and announced like any other exception.
	var actor, reason string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT actor_id, reason FROM exception_transitions
		WHERE exception_id = $1 AND to_state = 'OPEN'`, e.ExceptionID).Scan(&actor, &reason))
	assert.Equal(t, engine.LateDataActor, actor)
	var payload map[string]any
	require.NoError(t, testPool.QueryRow(ctx, `SELECT payload FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'control.exception.opened'`, e.ExceptionID).Scan(&payload))
	assert.Equal(t, "wm", payload["frozen_watermark"])
	assert.Equal(t, "wm-2", payload["observed_watermark"])
	assert.Equal(t, domain.ReasonLateDataAfterCertification, payload["reason_code"])

	// The gate reports it closed at the same decision point as every other reason.
	g, it := gateItem(t, w, "FIN-CTRL-020")
	assert.False(t, g.Open)
	assert.Equal(t, 1, g.Blocking)
	assert.Equal(t, domain.GateBlocking, it.Status)
	assert.Equal(t, domain.GateReasonLateDataAfterCertification, it.Reason)
	assert.Equal(t, e.ExceptionID, it.LateDataExceptionID)
	assert.NotEmpty(t, it.LateDataDetectedAt)
	assert.Equal(t, "CERTIFIED", it.Certificatio, "the certification itself is untouched")

	// Scenario 05: the certified population and certification were not mutated.
	assert.Equal(t, before, certifiedState(t, run020.RunID))
}

// A late-data exception resolved under authority clears the gate; the same
// watermark is not reported twice, a further move is; and a new run that
// supersedes the certified one is what the gate reads from then on.
func TestLateData_ResolvedUnderAuthorityOrSupersededClearsTheGate(t *testing.T) {
	w := newWave5(t)
	run := certifiedRun(t, w)
	w.src.set(catalogue.SysGL, "journal-balances", "wm-2", journalBal("j1", "5", "5"), journalBal("j2", "1", "1"))
	sweepAll(t, w)
	late := lateExceptions(t, w, run.RunID)
	require.Len(t, late, 1)
	_, it := gateItem(t, w, "FIN-CTRL-020")
	require.Equal(t, domain.GateReasonLateDataAfterCertification, it.Reason)

	waiveLate(t, w, late[0])
	_, it = gateItem(t, w, "FIN-CTRL-020")
	assert.Equal(t, domain.GatePassed, it.Status, "a waiver under authority accepts the late data")
	assert.Empty(t, it.LateDataExceptionID)

	sweepAll(t, w)
	assert.Len(t, lateExceptions(t, w, run.RunID), 1, "the waived watermark is not reported again")

	w.src.set(catalogue.SysGL, "journal-balances", "wm-3", journalBal("j1", "5", "5"), journalBal("j2", "1", "1"), journalBal("j3", "2", "2"))
	sweepAll(t, w)
	late = lateExceptions(t, w, run.RunID)
	require.Len(t, late, 2, "a further move is new late data")
	_, it = gateItem(t, w, "FIN-CTRL-020")
	assert.Equal(t, domain.GateReasonLateDataAfterCertification, it.Reason)

	// Scenario 21: a new run naming the certified one as prior replaces it.
	_, next := w.run("FIN-CTRL-020", func(r *domain.CreateRunRequest) {
		fiscal(r)
		r.PriorRunID, r.Reason = run.RunID, "late journals after certification"
	})
	require.Equal(t, domain.LifecycleReadyToCertify, next.LifecycleState)
	next, err := certifyRun(ctx, w.tenant, next.RunID, "checker", "c", next.Version, domain.CertifyRequest{Decision: "CERTIFY"}, testAuthority("checker", w.tenant))
	require.NoError(t, err)
	_, it = gateItem(t, w, "FIN-CTRL-020")
	assert.Equal(t, domain.GatePassed, it.Status)
	assert.Equal(t, next.RunID, it.RunID)

	sweepAll(t, w)
	assert.Empty(t, lateExceptions(t, w, next.RunID), "the new run froze wm-3 and the source is still there")
	assert.Len(t, lateExceptions(t, w, run.RunID), 2, "the superseded run is no longer swept")
}

// A source the sweep cannot read is recorded and changes nothing else; a revoked
// certification is not swept (it already blocks the gate).
func TestLateData_UnreadableSourceAndRevokedRunsChangeNothing(t *testing.T) {
	w := newWave5(t)
	run := certifiedRun(t, w)
	w.src.mu.Lock()
	w.src.down[catalogue.SysGL] = true
	w.src.mu.Unlock()
	sweepAll(t, w)
	outcome, observed, reported, errText := checkRow(t, run.RunID)
	assert.Equal(t, store.WatermarkUnverified, outcome)
	assert.Nil(t, observed)
	assert.Nil(t, reported)
	assert.Contains(t, errText, "503")
	assert.Empty(t, lateExceptions(t, w, run.RunID))
	_, it := gateItem(t, w, "FIN-CTRL-020")
	assert.Equal(t, domain.GatePassed, it.Status, "an unreadable source is not evidence of late data")

	w.src.mu.Lock()
	w.src.down[catalogue.SysGL] = false
	w.src.mu.Unlock()
	_, _, err := testStore.RevokeCertification(ctx, w.tenant, run.RunID, "auditor", "c", "k-"+uuid.NewString(), run.Version, revokeReq)
	require.NoError(t, err)
	w.src.set(catalogue.SysGL, "journal-balances", "wm-2", journalBal("j1", "5", "5"), journalBal("j2", "1", "1"))
	sweepAll(t, w)
	assert.Empty(t, lateExceptions(t, w, run.RunID))
	_, it = gateItem(t, w, "FIN-CTRL-020")
	assert.Equal(t, domain.GateReasonCertificationRevoked, it.Reason)
}

// 000009's down migration refuses while a check records a late-data exception it
// opened (that record is what stops a resolved one being reported again), and
// changes nothing when it refuses. The empty case is covered by
// TestMigrations_DownScriptsReverseCleanlyAndUpReapplies.
func TestLateData_DownMigrationRefusedWhileAReportExists(t *testing.T) {
	w := newWave5(t)
	run := certifiedRun(t, w)
	w.src.set(catalogue.SysGL, "journal-balances", "wm-2", journalBal("j1", "5", "5"), journalBal("j2", "1", "1"))
	sweepAll(t, w)
	require.Len(t, lateExceptions(t, w, run.RunID), 1)

	down, err := os.ReadFile("../../deployments/migrations/000009_late_data_checks.down.sql")
	require.NoError(t, err)
	tx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, string(down))
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "cannot roll back 000009"), err.Error())
	require.NoError(t, tx.Rollback(ctx))

	var n int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM control_watermark_checks WHERE run_id = $1`, run.RunID).Scan(&n))
	assert.Equal(t, 1, n, "a refused rollback drops nothing")
}
