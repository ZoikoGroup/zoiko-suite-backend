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

var wave6Pops = map[string]string{
	catalogue.SysFinancialClose: "migration-batch-tieout",
	catalogue.SysTaxAuthority:   "unacknowledged-filings",
	catalogue.SysPayeeBanking:   "destination-changes",
	catalogue.SysTreasury:       "bank-account-ownership",
	catalogue.SysDataIngestion:  "import-batch-tieout",
}

func newWave6(t *testing.T) *wave2 {
	t.Helper()
	w := &wave2{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), src: newFakeSources(), defs: map[string]string{}}
	endpoints := map[string]string{}
	for sys, pop := range wave6Pops {
		s := w.src.server(sys)
		t.Cleanup(s.Close)
		endpoints[sys] = s.URL
		w.src.noPeriod[sys+"/"+pop] = true // the real endpoints reject period_id
	}
	w.exec = engine.New(testStore, source.NewHTTPFetcher(endpoints, nil), zap.NewNop()).WithClock(func() time.Time { return wave2Now })
	defs, err := catalogue.Wave6("2026-01-01")
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

func scope(js string) func(*domain.CreateRunRequest) {
	return func(r *domain.CreateRunRequest) { r.Scope = json.RawMessage(js) }
}

func tieout(dr, cr, rows, xdr, xcr, xrows string) domain.PopulationRecord {
	x := r("b1", "b1", dr)
	x.Currency = "XXX"
	return attrs(x, "expected_total_debits", dr, "expected_total_credits", cr, "expected_row_count", rows,
		"crosswalk_debits", xdr, "crosswalk_credits", xcr, "crosswalk_row_count", xrows, "batch_status", "LOADED")
}

// batchRec is the raw import-batch facts a data-ingestion-svc batch reports; "" is an absent declared value.
func batchRec(declRows, rows, valid, invalid, imported, quarantined, failed, skipped, declTotal, compTotal string) domain.PopulationRecord {
	x := r("imb_1", "imb_1", "0")
	x.Currency = "XXX"
	return attrs(x, "batch_status", "COMPLETED", "declared_rows", declRows, "row_count", rows, "valid_rows", valid, "invalid_rows", invalid,
		"imported_items", imported, "quarantined_items", quarantined, "failed_items", failed, "skipped_items", skipped,
		"declared_control_total", declTotal, "computed_control_total", compTotal)
}

func TestWave6_ImportBatchCompleteness_EachTieBreaksOnItsOwn_AndAbsenceIsNeverAPass(t *testing.T) {
	w := newWave6(t)
	sc := scope(`{"batch_id":"imb_1"}`)

	// 10 rows: 9 valid + 1 invalid; 9 imported + 1 skipped. But "every row is imported" is a real tie, so even this
	// careful batch is a finding: the skipped (invalid) row never reached the owning domain.
	w.src.set(catalogue.SysDataIngestion, "import-batch-tieout", "wm", batchRec("10", "10", "9", "1", "9", "0", "0", "1", "500.00", "500.00"))
	run, final := w.run("FIN-CTRL-033", sc)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 1, "only the every-row-is-imported tie breaks")
	assert.Contains(t, ex[0].Detail, "every_row_is_imported")
	assert.Equal(t, "imb_1", w.src.lastQuery(catalogue.SysDataIngestion).Get("batch_id"))

	// A fully imported batch ties everywhere.
	w.src.set(catalogue.SysDataIngestion, "import-batch-tieout", "wm", batchRec("10", "10", "10", "0", "10", "0", "0", "0", "500.00", "500.00"))
	_, final = w.run("FIN-CTRL-033", sc)
	assert.Equal(t, domain.ResultPass, final.ResultState)

	// Each tie is detectable on its own.
	cases := map[string]domain.PopulationRecord{
		"declared rows differ":     batchRec("11", "10", "10", "0", "10", "0", "0", "0", "500.00", "500.00"),
		"valid+invalid miss a row": batchRec("10", "10", "9", "0", "10", "0", "0", "0", "500.00", "500.00"),
		"a row has no outcome":     batchRec("10", "10", "10", "0", "9", "0", "0", "0", "500.00", "500.00"),
		"a row was quarantined":    batchRec("10", "10", "10", "0", "9", "1", "0", "0", "500.00", "500.00"),
		"a row failed":             batchRec("10", "10", "10", "0", "9", "0", "1", "0", "500.00", "500.00"),
		"control total differs":    batchRec("10", "10", "10", "0", "10", "0", "0", "0", "500.00", "499.99"),
	}
	for name, rec := range cases {
		w.src.set(catalogue.SysDataIngestion, "import-batch-tieout", "wm", rec)
		run, final := w.run("FIN-CTRL-033", sc)
		assert.Equal(t, domain.ResultFail, final.ResultState, name)
		list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
		require.NoError(t, err)
		assert.NotEmpty(t, list, name)
	}

	// A batch that declared nothing cannot be proven complete against its source: the empty operand is an exception,
	// never a silent pass.
	w.src.set(catalogue.SysDataIngestion, "import-batch-tieout", "wm", batchRec("", "10", "10", "0", "10", "0", "0", "0", "", ""))
	run, final = w.run("FIN-CTRL-033", sc)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	reasons := map[string]bool{}
	for _, x := range list {
		reasons[x.ReasonCode] = true
	}
	assert.True(t, reasons[domain.ReasonOperandInvalid], "declared rows and control total are missing: %v", reasons)

	// An unbound run cannot resolve batch_id and fails closed.
	_, final = w.run("FIN-CTRL-033", nil)
	assert.Equal(t, domain.ResultIndeterminate, final.ResultState)
}

func TestWave6_OpeningBalanceTieOut_TiesAndDetectsEachBreak(t *testing.T) {
	w := newWave6(t)
	sc := scope(`{"batch_id":"b1"}`)

	w.src.set(catalogue.SysFinancialClose, "migration-batch-tieout", "wm", tieout("1000.00", "1000.00", "4", "1000.00", "1000.00", "4"))
	_, final := w.run("FIN-CTRL-038", sc)
	assert.Equal(t, domain.ResultPass, final.ResultState)
	assert.Equal(t, "b1", w.src.lastQuery(catalogue.SysFinancialClose).Get("batch_id"))

	cases := map[string]domain.PopulationRecord{
		"debits":  tieout("1000.00", "1000.00", "4", "999.99", "1000.00", "4"),
		"credits": tieout("1000.00", "1000.00", "4", "1000.00", "999.00", "4"),
		"rows":    tieout("1000.00", "1000.00", "4", "1000.00", "1000.00", "3"),
	}
	for name, rec := range cases {
		w.src.set(catalogue.SysFinancialClose, "migration-batch-tieout", "wm", rec)
		run, final := w.run("FIN-CTRL-038", sc)
		assert.Equal(t, domain.ResultFail, final.ResultState, name)
		ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
		require.NoError(t, err)
		assert.NotEmpty(t, ex, name)
	}

	// Declared totals that are themselves unbalanced fail the balance equation.
	w.src.set(catalogue.SysFinancialClose, "migration-batch-tieout", "wm", tieout("900.00", "1000.00", "4", "900.00", "1000.00", "4"))
	_, final = w.run("FIN-CTRL-038", sc)
	assert.Equal(t, domain.ResultFail, final.ResultState)

	// An unbound run cannot resolve batch_id and fails closed.
	_, final = w.run("FIN-CTRL-038", nil)
	assert.Equal(t, domain.ResultIndeterminate, final.ResultState)
}

func TestWave6_TaxFilingAcknowledgement_EveryReturnedFilingIsAFinding(t *testing.T) {
	w := newWave6(t)
	sc := scope(`{"submitted_before":"2026-09-01"}`)
	w.src.set(catalogue.SysTaxAuthority, "unacknowledged-filings", "wm")
	_, final := w.run("FIN-CTRL-037", sc)
	assert.Equal(t, domain.ResultPass, final.ResultState)

	f := r("s1", "s1", "1200.00")
	f.Currency = "XXX"
	w.src.set(catalogue.SysTaxAuthority, "unacknowledged-filings", "wm", f)
	run, final := w.run("FIN-CTRL-037", sc)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 1)
	assert.Equal(t, "EXTERNAL_STATUS", ex[0].Category)
	assert.Equal(t, "FILING_NOT_ACKNOWLEDGED", ex[0].ReasonCode)
	assert.Equal(t, "2026-09-01", w.src.lastQuery(catalogue.SysTaxAuthority).Get("submitted_before"))
}

func TestWave6_CashAccountOwnership_OnlyGapsAreFindings(t *testing.T) {
	w := newWave6(t)
	mk := func(id, gap string) domain.PopulationRecord {
		x := r(id, id, "0")
		x.Currency = "XXX"
		return attrs(x, "account_status", "ACTIVE", "ownership_gap", gap)
	}
	w.src.set(catalogue.SysTreasury, "bank-account-ownership", "wm", mk("a1", ""), mk("a2", "OWNERSHIP_UNVERIFIED"), mk("a3", "SELF_VERIFIED"))
	run, final := w.run("FIN-CTRL-044", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 2, "only the two accounts with a gap are findings")
	assert.Equal(t, "AUTHORIZATION", ex[0].Category)
	assert.Equal(t, "BANK_ACCOUNT_OWNERSHIP_GAP", ex[0].ReasonCode)

	w.src.set(catalogue.SysTreasury, "bank-account-ownership", "wm", mk("a1", ""))
	_, final = w.run("FIN-CTRL-044", nil)
	assert.Equal(t, domain.ResultPass, final.ResultState)
}

func TestWave6_BankDetailChangeSoD_OnlyGapsAreFindings(t *testing.T) {
	w := newWave6(t)
	sc := scope(`{"changed_from":"2026-09-01","changed_to":"2026-09-30"}`)
	mk := func(id, gap string) domain.PopulationRecord {
		x := r(id, id, "0")
		x.Currency = "XXX"
		return attrs(x, "party_ref", "P-"+id, "sod_gap", gap)
	}
	w.src.set(catalogue.SysPayeeBanking, "destination-changes", "wm", mk("d1", ""), mk("d2", "SELF_APPROVED"), mk("d3", "NO_APPROVER"))
	run, final := w.run("FIN-CTRL-045", sc)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 2)
	assert.Equal(t, "AUTHORIZATION", ex[0].Category)
	q := w.src.lastQuery(catalogue.SysPayeeBanking)
	assert.Equal(t, "2026-09-01", q.Get("changed_from"))
	assert.Equal(t, "2026-09-30", q.Get("changed_to"))

	w.src.set(catalogue.SysPayeeBanking, "destination-changes", "wm", mk("d1", ""))
	_, final = w.run("FIN-CTRL-045", sc)
	assert.Equal(t, domain.ResultPass, final.ResultState)
}
