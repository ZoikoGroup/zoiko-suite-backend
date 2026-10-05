package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
)

func logic(t *testing.T, raw string) domain.RuleLogic {
	t.Helper()
	l, err := domain.ParseRuleLogic(json.RawMessage(raw))
	require.NoError(t, err)
	return l
}

func withAttrs(r domain.PopulationRecord, kv ...string) domain.PopulationRecord {
	r.Attributes = map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Attributes[kv[i]] = kv[i+1]
	}
	return r
}

type countingFetcher struct {
	inner *fakeFetcher
	calls []string
}

func (c *countingFetcher) Fetch(ctx context.Context, s source.Spec, sc source.Scope) (*source.Fetched, error) {
	c.calls = append(c.calls, s.Ref())
	return c.inner.Fetch(ctx, s, sc)
}

func runAt(t *testing.T, st *fakeStore, f source.Fetcher, now time.Time) *domain.ControlRun {
	t.Helper()
	ex := New(st, f, zap.NewNop())
	ex.now = func() time.Time { return now }
	r, err := ex.Execute(context.Background(), Input{TenantID: "t1", RunID: "r1", Actor: "alice", CorrelationID: "c1"})
	require.NoError(t, err)
	return r
}

var sept30 = time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

func TestKinds_GroupMatchReconcilesSubledgerToLedgerPostings(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"MATCH","allow_groups":true,"skip_content_duplicates":true}`)
	st := &fakeStore{ec: ec}
	f := newFetcher(
		[]domain.PopulationRecord{rec("inv1", "INV-1", "40")}, // AR open balance after a partial payment
		[]domain.PopulationRecord{rec("l1", "INV-1", "100"), rec("l2", "INV-1", "-60")})
	r := runAt(t, st, f, sept30)

	assert.Equal(t, domain.ResultPass, r.ResultState)
	require.Len(t, st.recorded.Matches, 1)
	m := st.recorded.Matches[0]
	assert.Equal(t, domain.MatchGroup, m.Kind)
	assert.Equal(t, []string{"l1", "l2"}, m.SideBRecords, "the allocation names every ledger line")

	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(st.recorded.EvidenceContent, &c))
	assert.Equal(t, domain.KindMatch, c.Execution.Kind)
	assert.Equal(t, 1, c.Execution.GroupMatchCount)
}

// The behaviour comes from the PINNED rule version: without allow_groups the same data is ambiguous.
func TestKinds_SameDataDifferentPinnedRuleDifferentOutcome(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"MATCH"}`)
	st := &fakeStore{ec: ec}
	r := runAt(t, st, newFetcher(
		[]domain.PopulationRecord{rec("inv1", "INV-1", "40")},
		[]domain.PopulationRecord{rec("l1", "INV-1", "100"), rec("l2", "INV-1", "-60")}), sept30)
	assert.Equal(t, domain.ResultFail, r.ResultState)
	for _, x := range st.recorded.Exceptions {
		assert.Equal(t, domain.ReasonAmbiguousKey, x.ReasonCode)
	}
}

// Scenario 14 end to end: the double-posted supplier invoice is exposed, not netted away.
func TestKinds_DoublePostedInvoiceFailsGroupControl(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"MATCH","allow_groups":true,"skip_content_duplicates":true}`)
	st := &fakeStore{ec: ec}
	r := runAt(t, st, newFetcher(
		[]domain.PopulationRecord{rec("b1", "BILL-9", "500")},
		[]domain.PopulationRecord{rec("g1", "BILL-9", "500"), rec("g2", "BILL-9", "500")}), sept30)
	assert.Equal(t, domain.ResultFail, r.ResultState)
	require.Len(t, st.recorded.Exceptions, 1)
	assert.Equal(t, domain.ReasonGroupMismatch, st.recorded.Exceptions[0].ReasonCode)
	assert.Equal(t, "500", st.recorded.Exceptions[0].Exposure)
}

func TestKinds_ExclusionsAreEvidencedNotSilent(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"MATCH","exclusions":[{"side":"A","attr":"status","values":["SUPERSEDED"],
		"reason":"replaced by re-normalisation","authority":"TREASURER"}]}`)
	st := &fakeStore{ec: ec}
	a := []domain.PopulationRecord{
		withAttrs(rec("a1", "R-1", "100"), "status", "NORMALIZED"),
		withAttrs(rec("a1old", "R-1", "100"), "status", "SUPERSEDED"), // would otherwise be a duplicate/ambiguous
		withAttrs(rec("a2old", "R-2", "30"), "status", "SUPERSEDED"),
	}
	r := runAt(t, st, newFetcher(a, []domain.PopulationRecord{rec("b1", "R-1", "100")}), sept30)

	assert.Equal(t, domain.ResultPass, r.ResultState)
	require.Len(t, st.frozen, 2)
	assert.Equal(t, 1, st.frozen[0].Snapshot.RowCount, "excluded records are not in the frozen population")
	require.Len(t, st.frozen[0].Snapshot.Exclusions, 1)
	x := st.frozen[0].Snapshot.Exclusions[0]
	assert.Equal(t, 2, x.Count)
	assert.Equal(t, "130", x.Value)
	assert.Equal(t, "USD", x.Currency)
	assert.Equal(t, "TREASURER", x.Authority)
	assert.Contains(t, x.Reason, "replaced by re-normalisation")
	assert.True(t, domain.ValidDigest(x.Digest), "the excluded ids are fingerprinted")
	assert.Empty(t, st.frozen[1].Snapshot.Exclusions, "the exclusion was scoped to side A")

	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(st.recorded.EvidenceContent, &c))
	require.Len(t, c.Populations[0].Exclusions, 1, "the evidence package carries the exclusion impact")
	assert.Equal(t, 2, c.Populations[0].Exclusions[0].Count)
}

func TestKinds_OutstandingReconcilingItemHasExpectedClearingDueDate(t *testing.T) {
	ec := newEC()
	ec.Run.PeriodID = "2026-09"
	ec.RuleLogic = logic(t, `{"kind":"MATCH","outstanding_days":5}`)
	st := &fakeStore{ec: ec}
	late := rec("a1", "DEP-1", "500")
	late.Date = "2026-09-29"
	r := runAt(t, st, newFetcher([]domain.PopulationRecord{late}, nil), sept30)

	assert.Equal(t, domain.ResultFail, r.ResultState, "a reconciling item is still a disclosed exception")
	require.Len(t, st.recorded.Exceptions, 1)
	x := st.recorded.Exceptions[0]
	assert.Equal(t, domain.CatTiming, x.Category)
	assert.Equal(t, domain.ReasonOutstandingItem, x.ReasonCode)
	require.NotNil(t, x.ExpectedClearing)
	assert.Equal(t, "2026-10-04", *x.ExpectedClearing)
	assert.Equal(t, time.Date(2026, 10, 4, 23, 59, 59, 0, time.UTC), x.DueAt, "due when it is expected to clear, not the default SLA")
}

func TestKinds_SequenceGapIsSingleSidedAndFetchesOnce(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"SEQUENCE_GAP"}`)
	ec.Definition.TargetSpec = json.RawMessage(`{}`) // no target for a single-population control
	st := &fakeStore{ec: ec}
	cf := &countingFetcher{inner: newFetcher([]domain.PopulationRecord{
		rec("a1", "INV-1", "1"), rec("a2", "INV-2", "1"), rec("a5", "INV-5", "1")}, nil)}
	r := runAt(t, st, cf, sept30)

	assert.Equal(t, []string{"ar/invoices"}, cf.calls, "only side A is fetched")
	require.Len(t, st.frozen, 1)
	assert.Equal(t, domain.SideA, st.frozen[0].Snapshot.Side)
	assert.Equal(t, domain.ResultFail, r.ResultState)
	require.Len(t, st.recorded.Exceptions, 1)
	assert.Equal(t, domain.ReasonSequenceGap, st.recorded.Exceptions[0].ReasonCode)
	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(st.recorded.EvidenceContent, &c))
	assert.Len(t, c.Populations, 1)
	assert.Equal(t, domain.KindSequenceGap, c.Execution.Kind)
}

func TestKinds_DuplicateScanPassAndFail(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"DUPLICATE_SCAN","key_fields":["attr:vendor_id","amount","date"]}`)
	clean := []domain.PopulationRecord{
		withAttrs(rec("a1", "B-1", "10"), "vendor_id", "V1"), withAttrs(rec("a2", "B-2", "11"), "vendor_id", "V1")}
	st := &fakeStore{ec: ec}
	assert.Equal(t, domain.ResultPass, runAt(t, st, newFetcher(clean, nil), sept30).ResultState)

	dirty := append(clean, withAttrs(rec("a3", "B-3", "10"), "vendor_id", "V1")) // same vendor+amount+date
	st = &fakeStore{ec: ec}
	assert.Equal(t, domain.ResultFail, runAt(t, st, newFetcher(dirty, nil), sept30).ResultState)
	assert.Equal(t, domain.ReasonDuplicateBusinessKey, st.recorded.Exceptions[0].ReasonCode)
}

// Scenario 16: a missing statement day fails the feed-completeness control.
func TestKinds_DateCoverageMissingStatementDay(t *testing.T) {
	ec := newEC()
	ec.Run.PeriodID = "2026-09"
	ec.RuleLogic = logic(t, `{"kind":"DATE_COVERAGE","group_attr":"bank_account_id","expected_groups":["ACC1"]}`)
	var recs []domain.PopulationRecord
	for d := 1; d <= 30; d++ {
		if d == 14 {
			continue
		}
		day := time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		r := withAttrs(rec("s"+day, "ACC1|"+day, "0"), "bank_account_id", "ACC1")
		r.Date = day
		recs = append(recs, r)
	}
	st := &fakeStore{ec: ec}
	r := runAt(t, st, newFetcher(recs, nil), sept30)
	assert.Equal(t, domain.ResultFail, r.ResultState)
	require.Len(t, st.recorded.Exceptions, 1)
	assert.Equal(t, domain.ReasonStatementDayMissing, st.recorded.Exceptions[0].ReasonCode)
	assert.Equal(t, []string{"gap:ACC1:2026-09-14..2026-09-14"}, st.recorded.Exceptions[0].RecordIDs)
}

func TestKinds_CoverageWithoutAPeriodIsATechnicalFailure_NotAPass(t *testing.T) {
	ec := newEC()
	ec.Run.PeriodID = ""
	ec.RuleLogic = logic(t, `{"kind":"DATE_COVERAGE"}`)
	st := &fakeStore{ec: ec}
	r := runAt(t, st, newFetcher(nil, nil), sept30)
	assert.Equal(t, domain.LifecycleFailed, r.LifecycleState)
	assert.Equal(t, domain.ResultIndeterminate, r.ResultState)
	assert.Contains(t, st.failed, "period_id")
}

// A single-population control needs no target spec, but a two-sided one still fails closed without it.
func TestKinds_TwoSidedControlWithoutExecutableTargetFails(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"MATCH"}`)
	ec.Definition.TargetSpec = json.RawMessage(`{}`)
	st := &fakeStore{ec: ec}
	r := runAt(t, st, newFetcher(nil, nil), sept30)
	assert.Equal(t, domain.ResultIndeterminate, r.ResultState)
	assert.Contains(t, st.failed, "target_spec")
}

func TestKinds_ForwardsPopulationParamsToTheSource(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"MATCH","allow_groups":true}`)
	ec.Definition.TargetSpec = json.RawMessage(`{"system":"gl","population":"ar-postings","params":{"account_codes":"1200","normal_balance":"DEBIT"}}`)
	st := &fakeStore{ec: ec}
	f := &fakeFetcher{byRef: map[string]*source.Fetched{
		"ar/invoices": {Records: nil, Watermark: "w"},
		"gl/ar-postings?account_codes=1200&normal_balance=DEBIT": {Records: nil, Watermark: "w"},
	}, errs: map[string]error{}}
	r := runAt(t, st, f, sept30)
	assert.Equal(t, domain.ResultPass, r.ResultState, "the spec ref identifies the exact population incl. params")
	assert.Equal(t, "gl/ar-postings?account_codes=1200&normal_balance=DEBIT", st.frozen[1].Snapshot.SpecRef)
}

func slipRec(id, gross, tax, ben, net string) domain.PopulationRecord {
	r := rec(id, "RUN-1", net)
	r.Attributes = map[string]string{"gross_pay": gross, "tax_withheld": tax, "benefits_deductions": ben, "net_pay": net}
	return r
}

func TestKinds_ArithmeticGrossToNetThroughThePipeline(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"ARITHMETIC","equations":[{"name":"gross_to_net","plus":["attr:gross_pay"],
		"minus":["attr:tax_withheld","attr:benefits_deductions"],"equals":"attr:net_pay"}]}`)
	ec.Tolerance = &domain.TolerancePolicy{ToleranceID: "t", AbsoluteTolerance: "0.01"}

	st := &fakeStore{ec: ec}
	cf := &countingFetcher{inner: newFetcher([]domain.PopulationRecord{
		slipRec("s1", "5000", "1200", "300", "3500"), slipRec("s2", "4000", "900", "0", "3100.01")}, nil)}
	r := runAt(t, st, cf, sept30)
	assert.Equal(t, domain.ResultPass, r.ResultState, "0.01 is within the PINNED tolerance")
	assert.Len(t, cf.calls, 1, "single population")

	st = &fakeStore{ec: ec}
	r = runAt(t, st, newFetcher([]domain.PopulationRecord{slipRec("s1", "5000", "1200", "300", "3400")}, nil), sept30)
	assert.Equal(t, domain.ResultFail, r.ResultState)
	require.Len(t, st.recorded.Exceptions, 1)
	assert.Equal(t, domain.ReasonEquationViolation, st.recorded.Exceptions[0].ReasonCode)
	assert.Equal(t, "100", st.recorded.Exceptions[0].Exposure)
}

// The per-run value is bound from the run's scope; evidence names the resolved population.
func TestKinds_RunScopeBindsPopulationParams(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"MATCH"}`)
	ec.Run.Scope = json.RawMessage(`{"count_id":"count-77"}`)
	ec.Definition.SourceSpec = json.RawMessage(`{"system":"inv","population":"stock-count-lines","params":{"count_id":"${scope.count_id}","side":"book"}}`)
	ec.Definition.TargetSpec = json.RawMessage(`{"system":"inv","population":"stock-count-lines","params":{"count_id":"${scope.count_id}","side":"physical"}}`)
	f := &fakeFetcher{byRef: map[string]*source.Fetched{
		"inv/stock-count-lines?count_id=count-77&side=book":     {Records: []domain.PopulationRecord{rec("l1", "count-77|i|l", "10")}, Watermark: "w"},
		"inv/stock-count-lines?count_id=count-77&side=physical": {Records: []domain.PopulationRecord{rec("l1", "count-77|i|l", "10")}, Watermark: "w"},
	}, errs: map[string]error{}}
	st := &fakeStore{ec: ec}
	r := runAt(t, st, f, sept30)
	assert.Equal(t, domain.ResultPass, r.ResultState)
	assert.Equal(t, "inv/stock-count-lines?count_id=count-77&side=book", st.frozen[0].Snapshot.SpecRef)
}

func TestKinds_RunWithoutTheRequiredScopeFailsIndeterminate(t *testing.T) {
	ec := newEC()
	ec.RuleLogic = logic(t, `{"kind":"MATCH"}`)
	ec.Definition.SourceSpec = json.RawMessage(`{"system":"inv","population":"stock-count-lines","params":{"count_id":"${scope.count_id}","side":"book"}}`)
	st := &fakeStore{ec: ec}
	cf := &countingFetcher{inner: newFetcher(nil, nil)}
	r := runAt(t, st, cf, sept30)
	assert.Equal(t, domain.ResultIndeterminate, r.ResultState)
	assert.Contains(t, st.failed, "scope binding")
	assert.Empty(t, cf.calls, "nothing is fetched for an unbound run")
}
