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

func newWave4(t *testing.T) *wave2 {
	t.Helper()
	w := &wave2{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), src: newFakeSources(), defs: map[string]string{}}
	endpoints := map[string]string{}
	for _, sys := range []string{catalogue.SysIntercompany, catalogue.SysGL, catalogue.SysConsolidation} {
		s := w.src.server(sys)
		t.Cleanup(s.Close)
		endpoints[sys] = s.URL
	}
	// The real Wave 4 endpoints all reject period_id (lifetime legs, or scoped by fiscal_period / run_id).
	for _, k := range []string{catalogue.SysIntercompany + "/entry-legs", catalogue.SysGL + "/journal-account-totals",
		catalogue.SysGL + "/trial-balance", catalogue.SysConsolidation + "/balance-contributions"} {
		w.src.noPeriod[k] = true
	}
	w.exec = engine.New(testStore, source.NewHTTPFetcher(endpoints, nil), zap.NewNop()).WithClock(func() time.Time { return wave2Now })
	defs, err := catalogue.Wave4("2026-01-01", catalogue.Wave4Options{ICReceivableAccounts: "1300", ICPayableAccounts: "2300"})
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

const (
	jSrc1 = "11111111-1111-1111-1111-111111111111"
	jSrc2 = "22222222-2222-2222-2222-222222222222"
	jTgt1 = "33333333-3333-3333-3333-333333333333"
)

// icLeg: an intercompany entry leg — record_id is the entry id, reference the LEG's journal id.
func icLeg(entryID, journalID, amount, ccy string) domain.PopulationRecord {
	x := r(entryID, journalID, amount)
	x.Currency = ccy
	return attrs(x, "intercompany_entry_id", entryID, "match_status", "MATCHED")
}

// glJournal: a per-journal total on the IC accounts — record_id is journal:ccy, reference the journal id.
func glJournal(journalID, ccy, amount string) domain.PopulationRecord {
	x := r(journalID+":"+ccy, journalID, amount)
	x.Currency = ccy
	return attrs(x, "journal_id", journalID)
}

func TestWave4_IC_SourceLegsTieToTheLedgerReceivableSide(t *testing.T) {
	w := newWave4(t)
	w.src.set(catalogue.SysIntercompany, "entry-legs?leg=source", "ic-wm",
		icLeg("e1", jSrc1, "1000.0000", "USD"), icLeg("e2", jSrc2, "250.5000", "USD"))
	w.src.set(catalogue.SysGL, "journal-account-totals", "gl-wm",
		glJournal(jSrc1, "USD", "1000.00"), glJournal(jSrc2, "USD", "250.50"))

	run, final := w.run("FIN-CTRL-014", nil)
	assert.Equal(t, domain.ResultPass, final.ResultState)
	assert.Equal(t, domain.CertPending, final.CertificationState, "key control")

	q := w.src.lastQuery(catalogue.SysGL)
	assert.Equal(t, "1300", q.Get("account_codes"))
	assert.Equal(t, "DEBIT", q.Get("normal_balance"), "receivable side")
	assert.Equal(t, w.entity, q.Get("legal_entity_id"))
	assert.Empty(t, q.Get("period_id"), "lifetime leg populations take no period")
	_ = run
}

func TestWave4_IC_SourceLegFindings(t *testing.T) {
	w := newWave4(t)
	w.src.set(catalogue.SysIntercompany, "entry-legs?leg=source", "wm",
		icLeg("e1", jSrc1, "1000.0000", "USD"), // GL has 900: amount mismatch
		icLeg("e2", jSrc2, "250.0000", "USD"))  // never posted in the ledger
	w.src.set(catalogue.SysGL, "journal-account-totals", "wm",
		glJournal(jSrc1, "USD", "900.00"),
		glJournal(jTgt1, "USD", "40.00")) // an IC posting nobody recorded as an entry
	run, final := w.run("FIN-CTRL-014", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	got := map[string]string{}
	for _, e := range list {
		got[e.ReasonCode] = e.Exposure
	}
	assert.Equal(t, map[string]string{
		domain.ReasonAmountMismatch:    "100",
		domain.ReasonMissingDownstream: "250",
		domain.ReasonMissingSource:     "40",
	}, got)
	for _, e := range list {
		if e.ReasonCode == domain.ReasonMissingSource {
			assert.Equal(t, "EXISTENCE", e.Assertion, "a ledger IC posting with no entry is an existence finding")
		}
	}
}

func TestWave4_IC_CurrencyIsNeverIgnored(t *testing.T) {
	w := newWave4(t)
	w.src.set(catalogue.SysIntercompany, "entry-legs?leg=source", "wm", icLeg("e1", jSrc1, "1000.0000", "USD"))
	w.src.set(catalogue.SysGL, "journal-account-totals", "wm", glJournal(jSrc1, "EUR", "1000.00")) // same number, other currency
	run, final := w.run("FIN-CTRL-014", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, domain.ReasonCurrencyMismatch, list[0].ReasonCode, "the intercompany service itself never compares currency; the control does")
}

func TestWave4_IC_TargetLegsTieToPayableSideWithCreditNormal(t *testing.T) {
	w := newWave4(t)
	w.src.set(catalogue.SysIntercompany, "entry-legs?leg=target", "wm", icLeg("e1", jTgt1, "1000.0000", "USD"))
	w.src.set(catalogue.SysGL, "journal-account-totals", "wm", glJournal(jTgt1, "USD", "1000.00"))
	_, final := w.run("FIN-CTRL-015", nil)
	assert.Equal(t, domain.ResultPass, final.ResultState)
	assert.Equal(t, domain.CertNotRequired, final.CertificationState, "standard control")
	q := w.src.lastQuery(catalogue.SysGL)
	assert.Equal(t, "2300", q.Get("account_codes"))
	assert.Equal(t, "CREDIT", q.Get("normal_balance"))
	assert.Equal(t, "target", w.src.lastQuery(catalogue.SysIntercompany).Get("leg"))
}

func TestWave4_IC_TwoLedgerJournalsForOneEntryAreADefectNotAnAllocation(t *testing.T) {
	w := newWave4(t)
	w.src.set(catalogue.SysIntercompany, "entry-legs?leg=source", "wm", icLeg("e1", jSrc1, "1000.0000", "USD"))
	// The same journal reference posted in two currencies: mixed-currency journal.
	w.src.set(catalogue.SysGL, "journal-account-totals", "wm",
		glJournal(jSrc1, "USD", "600.00"), glJournal(jSrc1, "EUR", "400.00"))
	_, final := w.run("FIN-CTRL-014", nil)
	assert.Equal(t, domain.ResultFail, final.ResultState, "groups are disabled for intercompany legs: no silent netting across currencies")
}

// ── FIN-CTRL-017 ─────────────────────────────────────────────────────────────

func tbLine(account, ccy, net string) domain.PopulationRecord {
	x := r(account+":"+ccy, account, net)
	x.Currency = ccy
	return attrs(x, "account_code", account, "fiscal_period", "2026-09")
}

func contribution(id, account, ccy, gross string) domain.PopulationRecord {
	x := r(id, account, gross)
	x.Currency = ccy
	return attrs(x, "consolidation_run_id", "run-1", "currency_basis", "target_currency_label")
}

func run017(w *wave2, scope string) (*domain.ControlRun, *domain.ControlRun) {
	return w.run("FIN-CTRL-017", func(r *domain.CreateRunRequest) { r.Scope = json.RawMessage(scope) })
}

func TestWave4_TrialBalanceToConsolidationInput_TiesAndBindsBothIdsFromTheRun(t *testing.T) {
	w := newWave4(t)
	w.src.set(catalogue.SysGL, "trial-balance", "gl-wm",
		tbLine("1000", "USD", "5000.00"), tbLine("4000", "USD", "-5000.00"))
	w.src.set(catalogue.SysConsolidation, "balance-contributions", "con-wm",
		contribution("c1", "1000", "USD", "5000.0000"), contribution("c2", "4000", "USD", "-5000.0000"))
	run, final := run017(w, `{"fiscal_period":"2026-09","consolidation_run_id":"run-1"}`)
	assert.Equal(t, domain.ResultPass, final.ResultState)
	assert.Equal(t, "2026-09", w.src.lastQuery(catalogue.SysGL).Get("fiscal_period"))
	assert.Equal(t, "run-1", w.src.lastQuery(catalogue.SysConsolidation).Get("run_id"))
	snaps, err := testStore.ListPopulationSnapshots(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, "consolidation/balance-contributions?run_id=run-1", snaps[1].SpecRef)
}

// The defect this control exists to expose: consolidation drops an account the ledger holds,
// and misstates another (e.g. it excluded a REVERSED original but kept the reversal).
func TestWave4_TrialBalanceToConsolidationInput_ExposesDroppedAndMisstatedAccounts(t *testing.T) {
	w := newWave4(t)
	w.src.set(catalogue.SysGL, "trial-balance", "wm",
		tbLine("1000", "USD", "5000.00"),
		tbLine("2000", "USD", "-1200.00"), // consolidation never received this account
		tbLine("4000", "USD", "0.00"))     // original + its reversal net to zero in the ledger
	w.src.set(catalogue.SysConsolidation, "balance-contributions", "wm",
		contribution("c1", "1000", "USD", "5000.0000"),
		contribution("c3", "4000", "USD", "-300.0000")) // reversal counted, reversed original dropped
	run, final := run017(w, `{"fiscal_period":"2026-09","consolidation_run_id":"run-1"}`)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	got := map[string]string{}
	for _, e := range list {
		got[e.ReasonCode] = e.Exposure
	}
	assert.Equal(t, map[string]string{domain.ReasonMissingDownstream: "1200", domain.ReasonAmountMismatch: "300"}, got)
}

func TestWave4_TrialBalanceToConsolidationInput_NothingIsTranslated(t *testing.T) {
	w := newWave4(t)
	w.src.set(catalogue.SysGL, "trial-balance", "wm", tbLine("1000", "EUR", "5000.00"))
	// Consolidation carries the run's TARGET currency label and translates nothing.
	w.src.set(catalogue.SysConsolidation, "balance-contributions", "wm", contribution("c1", "1000", "USD", "5000.0000"))
	run, final := run017(w, `{"fiscal_period":"2026-09","consolidation_run_id":"run-1"}`)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	list, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, domain.ReasonCurrencyMismatch, list[0].ReasonCode, "an untranslated EUR balance labelled USD is a finding, not a pass")
}

func TestWave4_TrialBalanceToConsolidationInput_UnboundRunFailsClosed(t *testing.T) {
	w := newWave4(t)
	_, final := run017(w, `{"fiscal_period":"2026-09"}`) // no consolidation_run_id
	assert.Equal(t, domain.LifecycleFailed, final.LifecycleState)
	assert.Equal(t, domain.ResultIndeterminate, final.ResultState)
	_, final = w.run("FIN-CTRL-017", nil) // no scope at all
	assert.Equal(t, domain.ResultIndeterminate, final.ResultState)
}

func TestWave4_SeedIsIdempotentAndIsolated(t *testing.T) {
	w := newWave4(t)
	defs, err := testStore.ListDefinitions(ctx, w.tenant, 50)
	require.NoError(t, err)
	assert.Len(t, defs, 3)
	other, err := testStore.ListDefinitions(ctx, uuid.NewString(), 50)
	require.NoError(t, err)
	assert.Empty(t, other)
}
