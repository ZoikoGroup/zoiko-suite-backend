//go:build integration

package store_test

import (
	"encoding/json"
	"fmt"
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

// wave2 wires the seeded Wave 2 catalogue to contract-shaped fake source systems
// named exactly as the catalogue names them.
type wave2 struct {
	t      *testing.T
	tenant string
	entity string
	src    *fakeSources
	exec   *engine.Executor
	defs   map[string]string // control code -> definition id
}

var wave2Now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func newWave2(t *testing.T) *wave2 {
	t.Helper()
	w := &wave2{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), src: newFakeSources(), defs: map[string]string{}}
	endpoints := map[string]string{}
	for _, sys := range []string{catalogue.SysAR, catalogue.SysAP, catalogue.SysGL, catalogue.SysBanking} {
		s := w.src.server(sys)
		t.Cleanup(s.Close)
		endpoints[sys] = s.URL
	}
	w.exec = engine.New(testStore, source.NewHTTPFetcher(endpoints, nil), zap.NewNop()).WithClock(func() time.Time { return wave2Now })

	defs, err := catalogue.Wave2(catalogue.Wave2Options{ARControlAccounts: "1200", APControlAccounts: "2000", CashAccounts: "1000",
		BankAccountsExpected: []string{"ACC1"}, EffectiveFrom: "2026-01-01"})
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

func (w *wave2) run(code string, req func(*domain.CreateRunRequest)) (*domain.ControlRun, *domain.ControlRun) {
	w.t.Helper()
	r := domain.CreateRunRequest{ControlDefinitionID: w.defs[code], LegalEntityID: w.entity, TriggerType: "PERIOD_END", PeriodID: "2026-09"}
	if req != nil {
		req(&r)
	}
	key := code + uuid.NewString()
	run, _, err := testStore.CreateRun(ctx, w.tenant, "prep", "corr", "create-"+key, r)
	require.NoError(w.t, err)
	_, replay, err := testStore.BeginExecution(ctx, w.tenant, run.RunID, "exec-"+key, "prep", "corr", store.SkipVersionCheck)
	require.NoError(w.t, err)
	require.False(w.t, replay)
	final, err := w.exec.Execute(ctx, engine.Input{TenantID: w.tenant, RunID: run.RunID, Actor: "prep", CorrelationID: "corr"})
	require.NoError(w.t, err)
	return run, final
}

func attrs(r domain.PopulationRecord, kv ...string) domain.PopulationRecord {
	r.Attributes = map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Attributes[kv[i]] = kv[i+1]
	}
	return r
}

// ── FIN-CTRL-001: AR subledger to GL ─────────────────────────────────────────

func TestWave2_AR_to_GL_GroupAllocationPassesAndIsPersistedExplicitly(t *testing.T) {
	w := newWave2(t)
	w.src.set(catalogue.SysAR, "open-invoices", "ar-wm",
		r("inv1", "INV-000001", "0.00"),  // fully paid: outstanding 0
		r("inv2", "INV-000002", "40.00"), // partly paid
		r("inv3", "INV-000003", "75.50")) // untouched
	w.src.set(catalogue.SysGL, "account-postings", "gl-wm",
		r("l1", "INV-000001", "100.00"), r("l2", "INV-000001", "-100.00"), // invoice + full cash application
		r("l3", "INV-000002", "100.00"), r("l4", "INV-000002", "-60.00"),
		r("l5", "INV-000003", "75.50"))

	run, final := w.run("FIN-CTRL-001", nil)
	assert.Equal(t, domain.ResultPass, final.ResultState, "AR open balances tie to GL control-account postings")
	assert.Equal(t, domain.LifecycleReadyToCertify, final.LifecycleState)
	assert.Equal(t, domain.CertPending, final.CertificationState, "key control: a named certifier is still required")

	matches, err := testStore.ListMatchResults(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	require.Len(t, matches, 3)
	groups := 0
	for _, m := range matches {
		if m.Kind == domain.MatchGroup {
			groups++
			assert.Len(t, m.SideBRecords, 2, "the allocation lists every ledger line (%s)", m.Reference)
		}
	}
	assert.Equal(t, 2, groups)

	// The GL population was requested with the catalogue's params, and the spec identifies it exactly.
	snaps, err := testStore.ListPopulationSnapshots(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, "general-ledger/account-postings?account_codes=1200&normal_balance=DEBIT", snaps[1].SpecRef)
	pkg, err := testStore.GetLatestEvidence(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	require.NotNil(t, pkg.Verified)
	assert.True(t, *pkg.Verified)
	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(pkg.Content, &c))
	assert.Equal(t, 2, c.Execution.GroupMatchCount)
	assert.Equal(t, domain.KindMatch, c.Execution.Kind)
}

// Scenario 13/14: differences hidden by matching totals, and a doubly posted invoice, are exposed.
func TestWave2_AR_to_GL_ExposesOmissionsAndDoublePostings(t *testing.T) {
	w := newWave2(t)
	w.src.set(catalogue.SysAR, "open-invoices", "wm",
		r("inv1", "INV-000001", "100.00"), r("inv2", "INV-000002", "50.00"), r("inv3", "INV-000003", "30.00"))
	w.src.set(catalogue.SysGL, "account-postings", "wm",
		r("l1", "INV-000001", "100.00"), r("l1b", "INV-000001", "100.00"), // double posted
		r("l2", "INV-000002", "80.00"), // GL lumps INV-2 + INV-3; INV-3 has no posting of its own
	)
	run, final := w.run("FIN-CTRL-001", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	got := map[string]domain.ControlException{}
	for _, e := range list {
		got[e.ReasonCode+"|"+e.Exposure] = e
	}
	require.Len(t, list, 3, "%+v", list)
	assert.Contains(t, got, domain.ReasonGroupMismatch+"|100", "double posting: 200 in GL vs 100 open")
	assert.Contains(t, got, domain.ReasonAmountMismatch+"|30", "INV-2: 50 vs 80")
	assert.Contains(t, got, domain.ReasonMissingDownstream+"|30", "INV-3 has no GL posting")
	for _, e := range list {
		assert.Equal(t, "AR_ACCOUNTING_OWNER", e.OwnerRole)
		assert.Equal(t, domain.ExOpen, e.State)
	}
}

// ── FIN-CTRL-002: AP with credit-normal ledger ───────────────────────────────

func TestWave2_AP_to_GL_UsesCreditNormalPositiveBalances(t *testing.T) {
	w := newWave2(t)
	w.src.set(catalogue.SysAP, "open-invoices", "wm", r("b1", "BILL-1", "500.00"))
	w.src.set(catalogue.SysGL, "account-postings", "wm", r("g1", "BILL-1", "500.00"))
	_, final := w.run("FIN-CTRL-002", nil)
	assert.Equal(t, domain.ResultPass, final.ResultState)
	req := w.src.lastQuery(catalogue.SysGL)
	assert.Equal(t, "CREDIT", req.Get("normal_balance"), "the source is asked for credit-normal amounts for a payable")
	assert.Equal(t, "2000", req.Get("account_codes"))
	assert.Equal(t, w.entity, req.Get("legal_entity_id"))
	assert.Equal(t, "2026-09", req.Get("period_id"))
}

// ── FIN-CTRL-003: bank to cash GL ────────────────────────────────────────────

func TestWave2_BankToCash_ExclusionsAreEvidenced_AndOutstandingItemsAreAged(t *testing.T) {
	w := newWave2(t)
	w.src.set(catalogue.SysBanking, "bank-transactions", "bk-wm",
		attrs(r("t1", "REF-1", "1000.00"), "status", "NORMALIZED"),
		attrs(r("t1old", "REF-1", "1000.00"), "status", "SUPERSEDED"),               // replaced by t1
		attrs(recAt("t2", "DEP-9", "250.00", "2026-09-29"), "status", "NORMALIZED")) // deposit not yet in the books
	w.src.set(catalogue.SysGL, "account-postings", "gl-wm", r("g1", "REF-1", "1000.00"))

	run, final := w.run("FIN-CTRL-003", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState, "the reconciling item is disclosed")

	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	x := list[0]
	assert.Equal(t, domain.ReasonOutstandingItem, x.ReasonCode)
	assert.Equal(t, domain.CatTiming, x.Category)
	require.NotNil(t, x.ExpectedClearing)
	assert.Equal(t, "2026-10-04", *x.ExpectedClearing)
	assert.Equal(t, "2026-10-04", x.DueAt.UTC().Format("2006-01-02"), "due when expected to clear")
	assert.Equal(t, "TREASURY_OWNER", x.OwnerRole)

	snaps, err := testStore.ListPopulationSnapshots(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, 2, snaps[0].RowCount, "the superseded row is not in the frozen population")
	require.Len(t, snaps[0].Exclusions, 1)
	assert.Equal(t, 1, snaps[0].Exclusions[0].Count)
	assert.Equal(t, "1000", snaps[0].Exclusions[0].Value)
	assert.Equal(t, "TREASURER", snaps[0].Exclusions[0].Authority)
	assert.True(t, domain.ValidDigest(snaps[0].Exclusions[0].Digest))
	// The bank-side exclusion does not touch the GL side.
	assert.Empty(t, snaps[1].Exclusions)
}

func recAt(id, ref, amt, date string) domain.PopulationRecord {
	x := r(id, ref, amt)
	x.Date = date
	return x
}

// ── FIN-CTRL-027 / 028 / 032 ─────────────────────────────────────────────────

func TestWave2_InvoiceSequenceGaps(t *testing.T) {
	w := newWave2(t)
	var recs []domain.PopulationRecord
	for _, n := range []int{1, 2, 3, 6, 7, 10} {
		recs = append(recs, r(fmt.Sprintf("i%d", n), fmt.Sprintf("INV-%06d", n), "10.00"))
	}
	w.src.set(catalogue.SysAR, "open-invoices", "wm", recs...)
	run, final := w.run("FIN-CTRL-027", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	assert.Equal(t, domain.CertNotRequired, final.CertificationState, "a standard control is monitored, not certified")
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 2)
	gaps := []string{list[0].RecordIDs[0], list[1].RecordIDs[0]}
	assert.ElementsMatch(t, []string{"gap:INV-000004-INV-000005", "gap:INV-000008-INV-000009"}, gaps)
	snaps, err := testStore.ListPopulationSnapshots(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Len(t, snaps, 1, "single-population control freezes one side")
}

func TestWave2_SupplierInvoiceDuplicateControl(t *testing.T) {
	w := newWave2(t)
	w.src.set(catalogue.SysAP, "open-invoices", "wm",
		attrs(r("i1", "V1-1001", "250.00"), "vendor_id", "V1"),
		attrs(r("i2", "V1-1002", "250.00"), "vendor_id", "V1"), // same vendor/amount/date, new invoice number
		attrs(r("i3", "V2-9", "250.00"), "vendor_id", "V2"))
	run, final := w.run("FIN-CTRL-028", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, domain.ReasonDuplicateBusinessKey, list[0].ReasonCode)
	assert.Equal(t, []string{"i2"}, list[0].RecordIDs)
	assert.Equal(t, "250", list[0].Exposure)
}

// Scenario 16: a missing statement day fails the feed control.
func TestWave2_BankFeedCompleteness_MissingDayAndDeadAccount(t *testing.T) {
	w := newWave2(t)
	var recs []domain.PopulationRecord
	for d := 1; d <= 30; d++ {
		day := time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday || d == 16 { // 16 Sep 2026 is a Wednesday
			continue
		}
		ds := day.Format("2006-01-02")
		recs = append(recs, attrs(recAt(fmt.Sprintf("s%d", d), "ACC1|"+ds, "1000.00", ds), "bank_account_id", "ACC1"))
	}
	w.src.set(catalogue.SysBanking, "bank-statements", "wm", recs...)
	run, final := w.run("FIN-CTRL-032", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, []string{"gap:ACC1:2026-09-16..2026-09-16"}, list[0].RecordIDs)
	assert.Equal(t, domain.ReasonStatementDayMissing, list[0].ReasonCode)

	// The account that must report but sends NOTHING cannot pass either.
	w.src.set(catalogue.SysBanking, "bank-statements", "wm2")
	_, dead := w.run("FIN-CTRL-032", nil)
	assert.Equal(t, domain.ResultFail, dead.ResultState)
}

func TestWave2_SourceDownIsIndeterminateForEveryControl(t *testing.T) {
	w := newWave2(t)
	for _, sys := range []string{catalogue.SysAR, catalogue.SysGL} {
		w.src.down[sys] = true
	}
	_, final := w.run("FIN-CTRL-001", nil)
	assert.Equal(t, domain.LifecycleFailed, final.LifecycleState)
	assert.Equal(t, domain.ResultIndeterminate, final.ResultState)
}

func TestWave2_TenantIsolationAcrossSeededCatalogue(t *testing.T) {
	w := newWave2(t)
	other := uuid.NewString()
	list, err := testStore.ListDefinitions(ctx, other, 50)
	require.NoError(t, err)
	assert.Empty(t, list)
	_, _, err = testStore.CreateRun(ctx, other, "mallory", "c", "k", domain.CreateRunRequest{
		ControlDefinitionID: w.defs["FIN-CTRL-001"], LegalEntityID: w.entity, TriggerType: "DAILY"})
	assert.ErrorIs(t, err, domain.ErrNotFound, "another tenant cannot run this tenant's control")
	defs, err := testStore.ListDefinitions(ctx, w.tenant, 50)
	require.NoError(t, err)
	assert.Len(t, defs, 6)
}

// A rule version's logic cannot change under an approved/pinned run: a newer, looser
// version is separate and unapproved until an independent approver accepts it.
func TestWave2_PinnedLogicSurvivesANewRuleVersion(t *testing.T) {
	w := newWave2(t)
	w.src.set(catalogue.SysAR, "open-invoices", "wm", r("inv1", "INV-1", "40.00"))
	w.src.set(catalogue.SysGL, "account-postings", "wm", r("l1", "INV-1", "100.00"), r("l2", "INV-1", "-60.00"))
	run, _, err := testStore.CreateRun(ctx, w.tenant, "prep", "c", "pin-1", domain.CreateRunRequest{
		ControlDefinitionID: w.defs["FIN-CTRL-001"], LegalEntityID: w.entity, TriggerType: "PERIOD_END", PeriodID: "2026-09"})
	require.NoError(t, err)

	// Someone adds and approves a v2 that DISABLES group matching after the run was created.
	_, err = testStore.CreateRuleVersion(ctx, w.tenant, w.defs["FIN-CTRL-001"], "maker",
		domain.CreateRuleVersionRequest{Logic: json.RawMessage(`{"kind":"MATCH"}`), EffectiveFrom: "2026-01-01"})
	require.NoError(t, err)
	_, err = testStore.ApproveRuleVersion(ctx, w.tenant, w.defs["FIN-CTRL-001"], 2, "checker")
	require.NoError(t, err)

	_, _, err = testStore.BeginExecution(ctx, w.tenant, run.RunID, "pin-exec", "prep", "c", store.SkipVersionCheck)
	require.NoError(t, err)
	final, err := w.exec.Execute(ctx, engine.Input{TenantID: w.tenant, RunID: run.RunID, Actor: "prep", CorrelationID: "c"})
	require.NoError(t, err)
	assert.Equal(t, 1, run.RuleVersion)
	assert.Equal(t, domain.ResultPass, final.ResultState, "the run executes the logic of the version it pinned (v1: groups allowed)")
}
