//go:build integration

package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/engine"
	"zoiko.io/financial-control-svc/internal/source"
	"zoiko.io/financial-control-svc/internal/store"
)

func newWave3(t *testing.T) *wave2 {
	t.Helper()
	w := &wave2{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), src: newFakeSources(), defs: map[string]string{}}
	endpoints := map[string]string{}
	for _, sys := range []string{catalogue.SysInventory, catalogue.SysPayroll} {
		s := w.src.server(sys)
		t.Cleanup(s.Close)
		endpoints[sys] = s.URL
	}
	// The real inventory endpoint rejects period_id.
	w.src.noPeriod[catalogue.SysInventory+"/stock-count-lines"] = true
	w.exec = engine.New(testStore, source.NewHTTPFetcher(endpoints, nil), zap.NewNop()).WithClock(func() time.Time { return wave2Now })
	defs, err := catalogue.Wave3("2026-01-01")
	require.NoError(t, err)
	for _, d := range defs {
		def, err := testStore.CreateDefinition(ctx, w.tenant, "maker", "c", d)
		require.NoError(t, err, d.ControlCode)
		_, err = testStore.ApproveRuleVersion(ctx, w.tenant, def.ControlDefinitionID, 1, "checker")
		require.NoError(t, err)
		w.defs[d.ControlCode] = def.ControlDefinitionID
	}
	return w
}

func paySlip(id, run, status, shadow, gross, tax, ben, net string) domain.PopulationRecord {
	x := r(id, run, net)
	x.Date = "2026-09-25"
	return attrs(x, "run_id", run, "run_status", status, "is_shadow_run", shadow, "employee_number", "E-"+id,
		"gross_pay", gross, "tax_withheld", tax, "benefits_deductions", ben, "net_pay", net)
}

func payRun(run, status, shadow, total string) domain.PopulationRecord {
	x := r(run, run, total)
	x.Date = "2026-09-25"
	return attrs(x, "status", status, "is_shadow_run", shadow, "employee_count", "2")
}

// ── PAY-CTRL-001 gross-to-net ────────────────────────────────────────────────

func TestWave3_Payroll_GrossToNet_FootingPassesAndNonFinalAndShadowAreEvidencedExclusions(t *testing.T) {
	w := newWave3(t)
	w.src.set(catalogue.SysPayroll, "pay-slips?measure=net", "pr-wm",
		paySlip("s1", "RUN-1", "COMPLETED", "false", "5000", "1200", "300", "3500"),
		paySlip("s2", "RUN-1", "COMPLETED", "false", "4000", "900", "0", "3100"),
		paySlip("s3", "RUN-2", "BLOCKED", "false", "1", "1", "1", "999"),  // would fail, but the run is not final
		paySlip("s4", "RUN-3", "COMPLETED", "true", "1", "1", "1", "999")) // shadow payroll
	run, final := w.run("PAY-CTRL-001", func(r *domain.CreateRunRequest) { r.TriggerType = "EVENT" })
	assert.Equal(t, domain.ResultPass, final.ResultState)
	assert.Equal(t, domain.CertNotRequired, final.CertificationState)

	snaps, err := testStore.ListPopulationSnapshots(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, 2, snaps[0].RowCount)
	require.Len(t, snaps[0].Exclusions, 2, "both exclusion rules are evidenced")
	byAuth := map[int]bool{}
	for _, x := range snaps[0].Exclusions {
		assert.Equal(t, "PAYROLL_CONTROLLER", x.Authority)
		assert.Equal(t, 1, x.Count)
		byAuth[x.Count] = true
	}
	assert.Equal(t, "payroll-run/pay-slips?measure=net", snaps[0].SpecRef)
}

func TestWave3_Payroll_GrossToNet_OneBadSlipFailsEvenWhenRunTotalsTie(t *testing.T) {
	w := newWave3(t)
	w.src.set(catalogue.SysPayroll, "pay-slips?measure=net", "wm",
		paySlip("s1", "RUN-1", "COMPLETED", "false", "5000", "1000", "0", "4050"), // +50
		paySlip("s2", "RUN-1", "COMPLETED", "false", "5000", "1000", "0", "3950")) // -50: net total still ties
	run, final := w.run("PAY-CTRL-001", func(r *domain.CreateRunRequest) { r.TriggerType = "EVENT" })
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, e := range list {
		assert.Equal(t, domain.ReasonEquationViolation, e.ReasonCode)
		assert.Equal(t, "50", e.Exposure)
		assert.Equal(t, "PAYROLL_OWNER", e.OwnerRole)
	}
}

// ── PAY-CTRL-002/003 run totals to slips ─────────────────────────────────────

func TestWave3_Payroll_RunNetTotalTiesToItsSlips_Group(t *testing.T) {
	w := newWave3(t)
	w.src.set(catalogue.SysPayroll, "payroll-runs?measure=net", "run-wm",
		payRun("RUN-1", "COMPLETED", "false", "6600"),
		payRun("RUN-9", "BLOCKED", "false", "1")) // excluded: not final
	w.src.set(catalogue.SysPayroll, "pay-slips?measure=net", "slip-wm",
		paySlip("s1", "RUN-1", "COMPLETED", "false", "5000", "1200", "300", "3500"),
		paySlip("s2", "RUN-1", "COMPLETED", "false", "4000", "900", "0", "3100"),
		paySlip("s9", "RUN-9", "BLOCKED", "false", "1", "0", "0", "1"))
	run, final := w.run("PAY-CTRL-002", func(r *domain.CreateRunRequest) { r.TriggerType = "EVENT" })
	assert.Equal(t, domain.ResultPass, final.ResultState)
	matches, err := testStore.ListMatchResults(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, domain.MatchGroup, matches[0].Kind)
	assert.Equal(t, "RUN-1", matches[0].Reference)
	assert.Equal(t, []string{"s1", "s2"}, matches[0].SideBRecords, "the allocation names every slip in the run")

	snaps, err := testStore.ListPopulationSnapshots(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, 1, snaps[0].RowCount, "the BLOCKED run is excluded on side A via its own `status` attribute")
	assert.Equal(t, 2, snaps[1].RowCount, "and its slip via `run_status` on side B")
}

func TestWave3_Payroll_RunTotalDoesNotTieAndRunWithoutSlips(t *testing.T) {
	w := newWave3(t)
	w.src.set(catalogue.SysPayroll, "payroll-runs?measure=net", "wm",
		payRun("RUN-1", "COMPLETED", "false", "6700"), // declared 6700 but slips sum to 6600
		payRun("RUN-2", "COMPLETED", "false", "500"))  // no slips at all
	w.src.set(catalogue.SysPayroll, "pay-slips?measure=net", "wm",
		paySlip("s1", "RUN-1", "COMPLETED", "false", "5000", "1200", "300", "3500"),
		paySlip("s2", "RUN-1", "COMPLETED", "false", "4000", "900", "0", "3100"))
	run, final := w.run("PAY-CTRL-002", func(r *domain.CreateRunRequest) { r.TriggerType = "EVENT" })
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	reasons := map[string]string{}
	for _, e := range list {
		reasons[e.ReasonCode] = e.Exposure
	}
	assert.Equal(t, map[string]string{domain.ReasonGroupMismatch: "100", domain.ReasonMissingDownstream: "500"}, reasons)
}

func TestWave3_Payroll_GrossControlUsesTheGrossMeasure(t *testing.T) {
	w := newWave3(t)
	w.src.set(catalogue.SysPayroll, "payroll-runs?measure=gross", "wm", payRun("RUN-1", "COMPLETED", "false", "9000"))
	w.src.set(catalogue.SysPayroll, "pay-slips?measure=gross", "wm",
		withAmount(paySlip("s1", "RUN-1", "COMPLETED", "false", "5000", "1200", "300", "3500"), "5000"), // measure=gross: amount is gross
		withAmount(paySlip("s2", "RUN-1", "COMPLETED", "false", "4000", "900", "0", "3100"), "4000"))
	_, final := w.run("PAY-CTRL-003", func(r *domain.CreateRunRequest) { r.TriggerType = "EVENT" })
	assert.Equal(t, domain.ResultPass, final.ResultState)
	assert.Equal(t, "gross", w.src.lastQuery(catalogue.SysPayroll).Get("measure"))
}

// ── FIN-CTRL-013 physical count ──────────────────────────────────────────────

func countLine(id, ref, qty string) domain.PopulationRecord {
	x := r(id, ref, qty)
	x.Currency = "XXX"
	x.Date = "2026-09-28"
	return attrs(x, "uom", "EA", "count_status", "VARIANCE_REVIEW")
}

func TestWave3_PhysicalCount_VariancesAndUncountedLinesAreExceptions(t *testing.T) {
	w := newWave3(t)
	// Book quantity as of cut-off vs what was physically counted.
	w.src.set(catalogue.SysInventory, "stock-count-lines?side=book", "book-wm",
		countLine("l1", "C1|ITEM-A|LOC-1", "10"), countLine("l2", "C1|ITEM-B|LOC-1", "25"), countLine("l3", "C1|ITEM-C|LOC-2", "4"))
	w.src.set(catalogue.SysInventory, "stock-count-lines?side=physical", "cnt-wm",
		countLine("l1", "C1|ITEM-A|LOC-1", "10"), // agrees
		countLine("l2", "C1|ITEM-B|LOC-1", "23")) // 2 short; l3 was never counted (absent)

	run, final := w.run("FIN-CTRL-013", func(r *domain.CreateRunRequest) {
		r.TriggerType = "ON_DEMAND"
		r.Reason = "cycle count C1"
		r.PeriodID = ""
		r.Scope = json.RawMessage(`{"count_id":"C1"}`)
	})
	assert.Equal(t, domain.ResultFail, final.ResultState)
	q := w.src.lastQuery(catalogue.SysInventory)
	assert.Equal(t, "C1", q.Get("count_id"), "the count is bound from the RUN's scope")

	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	got := map[string]domain.ControlException{}
	for _, e := range list {
		got[e.ReasonCode] = e
	}
	require.Len(t, list, 2)
	assert.Equal(t, "2", got[domain.ReasonAmountMismatch].Exposure, "ITEM-B is 2 units short")
	assert.Equal(t, "XXX", got[domain.ReasonAmountMismatch].Currency, "quantities carry the non-currency code")
	assert.Equal(t, "4", got[domain.ReasonMissingDownstream].Exposure, "ITEM-C was never counted")
	assert.Equal(t, "INVENTORY_OWNER", got[domain.ReasonAmountMismatch].OwnerRole)
	assert.Equal(t, "inventory-management/stock-count-lines?count_id=C1&side=book", mustSnap(t, w, run.RunID)[0].SpecRef)
}

func mustSnap(t *testing.T, w *wave2, runID string) []domain.PopulationSnapshot {
	t.Helper()
	s, err := testStore.ListPopulationSnapshots(ctx, w.tenant, runID)
	require.NoError(t, err)
	return s
}

func TestWave3_PhysicalCount_CleanCountPassesAndUnboundRunFailsClosed(t *testing.T) {
	w := newWave3(t)
	w.src.set(catalogue.SysInventory, "stock-count-lines?side=book", "b", countLine("l1", "C1|A|L", "10"))
	w.src.set(catalogue.SysInventory, "stock-count-lines?side=physical", "p", countLine("l1", "C1|A|L", "10"))
	_, ok := w.run("FIN-CTRL-013", func(r *domain.CreateRunRequest) {
		r.TriggerType, r.Reason, r.PeriodID, r.Scope = "ON_DEMAND", "cycle count", "", json.RawMessage(`{"count_id":"C1"}`)
	})
	assert.Equal(t, domain.ResultPass, ok.ResultState)

	_, unbound := w.run("FIN-CTRL-013", func(r *domain.CreateRunRequest) {
		r.TriggerType, r.Reason, r.PeriodID = "ON_DEMAND", "cycle count", ""
	})
	assert.Equal(t, domain.LifecycleFailed, unbound.LifecycleState)
	assert.Equal(t, domain.ResultIndeterminate, unbound.ResultState, "a run that does not say which count it is about cannot pass")
}

func TestWave3_SeedIsIdempotentAndTenantScoped(t *testing.T) {
	w := newWave3(t)
	defs, err := testStore.ListDefinitions(ctx, w.tenant, 50)
	require.NoError(t, err)
	assert.Len(t, defs, 4)
	again, _ := catalogue.Wave3("2026-01-01")
	for _, d := range again {
		_, err := testStore.CreateDefinition(ctx, w.tenant, "maker", "c", d)
		assert.ErrorIs(t, err, domain.ErrDuplicate, d.ControlCode)
	}
	other, err := testStore.ListDefinitions(ctx, uuid.NewString(), 50)
	require.NoError(t, err)
	assert.Empty(t, other)
}

// withAmount sets the record amount, as the source does for the requested `measure`.
func withAmount(r domain.PopulationRecord, amount string) domain.PopulationRecord {
	r.Amount = amount
	return r
}
