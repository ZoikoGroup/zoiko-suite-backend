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
