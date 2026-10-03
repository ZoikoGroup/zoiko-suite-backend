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

func newWave5(t *testing.T) *wave2 {
	t.Helper()
	w := &wave2{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), src: newFakeSources(), defs: map[string]string{}}
	s := w.src.server(catalogue.SysGL)
	t.Cleanup(s.Close)
	// The real GL kernel populations reject period_id.
	for _, p := range []string{"journal-balances", "control-account-postings", "unposted-events", "manual-journals", "event-journal-breaks"} {
		w.src.noPeriod[catalogue.SysGL+"/"+p] = true
	}
	w.exec = engine.New(testStore, source.NewHTTPFetcher(map[string]string{catalogue.SysGL: s.URL}, nil), zap.NewNop()).
		WithClock(func() time.Time { return wave2Now })
	defs, err := catalogue.Wave5("2026-01-01")
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

func fiscal(r *domain.CreateRunRequest) { r.Scope = json.RawMessage(`{"fiscal_period":"2026-09"}`) }

func journalBal(id, debit, credit string) domain.PopulationRecord {
	return attrs(r(id, id, debit), "debit_total", debit, "credit_total", credit, "entry_count", "2")
}

func TestWave5_JournalBalance_UnbalancedJournalIsCaughtAndBalancedPasses(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "100.00", "100.00"), journalBal("j2", "10.10", "10.10"))
	_, final := w.run("FIN-CTRL-020", fiscal)
	assert.Equal(t, domain.ResultPass, final.ResultState)
	assert.Equal(t, domain.LifecycleReadyToCertify, final.LifecycleState)

	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "100.00", "100.00"), journalBal("j3", "100.00", "99.99"))
	run, final := w.run("FIN-CTRL-020", fiscal)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 1)
	assert.Equal(t, "EQUATION_VIOLATION", ex[0].ReasonCode)
	assert.Equal(t, []string{"j3"}, ex[0].RecordIDs)
}

func TestWave5_DirectControlPosting_EveryReturnedEntryIsAFinding(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "control-account-postings", "wm")
	_, final := w.run("FIN-CTRL-021", fiscal)
	assert.Equal(t, domain.ResultPass, final.ResultState, "an empty violation population passes")

	w.src.set(catalogue.SysGL, "control-account-postings", "wm", r("e1", "J-1", "250.00"), r("e2", "J-2", "-40.00"))
	run, final := w.run("FIN-CTRL-021", fiscal)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 2)
	for _, e := range ex {
		assert.Equal(t, "CLASSIFICATION", e.Category)
		assert.Equal(t, "DIRECT_CONTROL_ACCOUNT_POSTING", e.ReasonCode)
	}
}

func TestWave5_ManualJournalReview_OnlyGapsAreFindings(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "manual-journals", "wm",
		attrs(r("m1", "m1", "10"), "review_gap", ""),
		attrs(r("m2", "m2", "20"), "review_gap", "SELF_APPROVED"),
		attrs(r("m3", "m3", "30"), "review_gap", "NO_APPROVER"))
	run, final := w.run("FIN-CTRL-023", fiscal)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 2)
	assert.Equal(t, "AUTHORIZATION", ex[0].Category)
	snaps, err := testStore.ListPopulationSnapshots(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, 3, snaps[0].RowCount, "the whole population is frozen, not just the findings")
}

func TestWave5_UnpostedEvents_ScopeBindsTheCutoff(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "unposted-events", "wm", r("x1", "EVT-1", "0"))
	_, final := w.run("FIN-CTRL-022", func(r *domain.CreateRunRequest) { r.Scope = json.RawMessage(`{"created_before":"2026-09-30"}`) })
	assert.Equal(t, domain.ResultFail, final.ResultState)
	assert.Equal(t, "2026-09-30", w.src.lastQuery(catalogue.SysGL).Get("created_before"))
	// Without the scope key the run cannot be bound and fails closed.
	_, final = w.run("FIN-CTRL-022", nil)
	assert.Equal(t, domain.LifecycleFailed, final.LifecycleState)
	assert.Equal(t, domain.ResultIndeterminate, final.ResultState)
}

// ── certification (maker-checker) ────────────────────────────────────────────

func TestWave5_Certify_MakerCannotCertifyCheckerCan(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	run, final := w.run("FIN-CTRL-020", fiscal) // created and executed by "prep"
	require.Equal(t, domain.LifecycleReadyToCertify, final.LifecycleState)

	_, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "prep", "c", final.Version, domain.CertifyRequest{Decision: "CERTIFY"})
	require.ErrorIs(t, err, domain.ErrSegregation, "the creator/executor cannot certify")
	_, err = testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", final.Version+5, domain.CertifyRequest{Decision: "CERTIFY"})
	require.ErrorIs(t, err, domain.ErrConflict, "a stale ETag is refused")

	out, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", final.Version, domain.CertifyRequest{Decision: "CERTIFY", Reason: "reviewed"})
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleCertified, out.LifecycleState)
	assert.Equal(t, domain.CertCertified, out.CertificationState)

	trs, err := testStore.ListTransitions(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	var cert []string
	for _, tr := range trs {
		if tr.Dimension == "CERTIFICATION" {
			cert = append(cert, tr.FromState+">"+tr.ToState+"@"+tr.ActorID)
		}
	}
	require.NotEmpty(t, cert)
	assert.Equal(t, "PENDING>CERTIFIED@checker", cert[len(cert)-1], "the decision is attributed to the certifier")

	_, err = testStore.CertifyRun(ctx, w.tenant, run.RunID, "other", "c", store.SkipVersionCheck, domain.CertifyRequest{Decision: "CERTIFY"})
	require.ErrorIs(t, err, domain.ErrInvalidTransition, "a certified run is final")
}

func TestWave5_Certify_RejectReturnsRunToExceptionReview(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	run, final := w.run("FIN-CTRL-020", fiscal)
	out, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", final.Version, domain.CertifyRequest{Decision: "REJECT", Reason: "evidence incomplete"})
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleExceptionReview, out.LifecycleState)
	assert.Equal(t, domain.CertRejected, out.CertificationState)
}

func TestWave5_Certify_FailedRunCannotBeCertified(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "4"))
	run, final := w.run("FIN-CTRL-020", fiscal)
	require.Equal(t, domain.ResultFail, final.ResultState)
	_, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", store.SkipVersionCheck, domain.CertifyRequest{Decision: "CERTIFY"})
	require.ErrorIs(t, err, domain.ErrInvalidTransition)
}

// ── FIN-CTRL-041 close gate ──────────────────────────────────────────────────

func TestWave5_CloseGate_OpensOnlyWhenEveryMandatoryControlIsCertified(t *testing.T) {
	w := newWave5(t)
	g, err := testStore.CloseGate(ctx, uuid.NewString(), uuid.NewString(), "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open, "no mandatory controls configured is NOT open")
	assert.False(t, g.Configured)

	g, err = testStore.CloseGate(ctx, w.tenant, w.entity, "2026-09")
	require.NoError(t, err)
	assert.True(t, g.Configured)
	assert.False(t, g.Open)
	require.Len(t, g.Items, 3, "020, 021 and 023 are mandatory")
	for _, it := range g.Items {
		assert.Equal(t, "NO_RUN_FOR_PERIOD", it.Reason)
	}

	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	w.src.set(catalogue.SysGL, "control-account-postings", "wm")
	w.src.set(catalogue.SysGL, "manual-journals", "wm", attrs(r("m1", "m1", "10"), "review_gap", ""))
	for _, code := range []string{"FIN-CTRL-020", "FIN-CTRL-021", "FIN-CTRL-023"} {
		_, final := w.run(code, fiscal)
		require.Equal(t, domain.LifecycleReadyToCertify, final.LifecycleState, code)
		g, err = testStore.CloseGate(ctx, w.tenant, w.entity, "2026-09")
		require.NoError(t, err)
		assert.False(t, g.Open, "not open until the last control is certified (%s just executed)", code)
		_, err = testStore.CertifyRun(ctx, w.tenant, final.RunID, "checker", "c", final.Version, domain.CertifyRequest{Decision: "CERTIFY"})
		require.NoError(t, err, code)
	}
	g, err = testStore.CloseGate(ctx, w.tenant, w.entity, "2026-09")
	require.NoError(t, err)
	assert.True(t, g.Open)
	assert.Equal(t, 0, g.Blocking)

	// A later failing run supersedes the certified one for gating purposes.
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j9", "5", "4"))
	w.run("FIN-CTRL-020", fiscal)
	g, err = testStore.CloseGate(ctx, w.tenant, w.entity, "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open)
	assert.Equal(t, 1, g.Blocking)

	other, err := testStore.CloseGate(ctx, w.tenant, w.entity, "2026-10")
	require.NoError(t, err)
	assert.False(t, other.Open, "a different period has no certified runs")
}

// ── FIN-CTRL-042 exception aggregation ───────────────────────────────────────

func TestWave5_ExceptionSummary_AggregatesLatestRunsAgainstAggregateThreshold(t *testing.T) {
	w := newWave5(t)
	s, err := testStore.ExceptionSummary(ctx, w.tenant, w.entity, "2026-09")
	require.NoError(t, err)
	assert.Empty(t, s.Currencies)
	assert.Nil(t, s.AggregateMaterial, "no materiality policy => not assessable")

	_, err = testStore.CreateMaterialityPolicy(ctx, w.tenant, "maker", domain.CreateMaterialityPolicyRequest{
		LegalEntityID: w.entity, ReportingBasis: "US_GAAP", AmountThreshold: "1000", AggregateThreshold: "300",
		Currency: "USD", EffectiveFrom: "2026-01-01", ApprovedBy: "cfo"})
	require.NoError(t, err)

	// An older run's findings must not be double counted once a newer run exists.
	w.src.set(catalogue.SysGL, "control-account-postings", "wm", r("e0", "J-0", "9999"))
	w.run("FIN-CTRL-021", fiscal)
	w.src.set(catalogue.SysGL, "control-account-postings", "wm", r("e1", "J-1", "200.00"), r("e2", "J-2", "-150.50"))
	w.run("FIN-CTRL-021", fiscal)
	eur := r("e3", "J-3", "40")
	eur.Currency = "EUR"
	w.src.set(catalogue.SysGL, "manual-journals", "wm", attrs(eur, "review_gap", "NO_APPROVER"))
	w.run("FIN-CTRL-023", fiscal)

	s, err = testStore.ExceptionSummary(ctx, w.tenant, w.entity, "2026-09")
	require.NoError(t, err)
	require.Len(t, s.Currencies, 2)
	assert.Equal(t, "EUR", s.Currencies[0].Currency)
	assert.Equal(t, "USD", s.Currencies[1].Currency)
	assert.Equal(t, 2, s.Currencies[1].Count)
	assert.Equal(t, "350.5", s.Currencies[1].Exposure, "200.00 + 150.50, not the superseded 9999")
	require.NotNil(t, s.AggregateMaterial)
	assert.True(t, *s.AggregateMaterial, "350.5 >= 300")
	assert.Equal(t, []string{"EUR"}, s.UnassessedCurrencies, "no FX basis is assumed")

	other, err := testStore.ExceptionSummary(ctx, w.tenant, uuid.NewString(), "2026-09")
	require.NoError(t, err)
	assert.Empty(t, other.Currencies, "another entity's exceptions are not counted")
}

func TestWave5_EventToJournal_EveryBreakIsAFinding(t *testing.T) {
	w := newWave5(t)
	sc := func(r *domain.CreateRunRequest) { r.Scope = json.RawMessage(`{"created_before":"2026-09-30"}`) }
	w.src.set(catalogue.SysGL, "event-journal-breaks", "wm")
	_, final := w.run("FIN-CTRL-019", sc)
	assert.Equal(t, domain.ResultPass, final.ResultState, "no break, no finding")

	mk := func(id, typ, amount string) domain.PopulationRecord {
		return attrs(r(id, "EVT-"+id, amount), "break_type", typ, "source_event_id", "EVT-"+id)
	}
	w.src.set(catalogue.SysGL, "event-journal-breaks", "wm",
		mk("a", "EVENT_WITHOUT_JOURNAL", "0"), mk("b", "JOURNAL_WITHOUT_EVENT", "120.00"), mk("c", "DUPLICATE_JOURNAL_FOR_EVENT", "9.00"))
	run, final := w.run("FIN-CTRL-019", sc)
	assert.Equal(t, domain.ResultFail, final.ResultState)
	ex, err := testStore.ListExceptions(ctx, w.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, ex, 3)
	assert.Equal(t, "MISSING", ex[0].Category)
	assert.Equal(t, "EVENT_JOURNAL_BREAK", ex[0].ReasonCode)
	assert.Equal(t, "2026-09-30", w.src.lastQuery(catalogue.SysGL).Get("created_before"))
}
