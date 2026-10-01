package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func withAttr(r PopulationRecord, kv ...string) PopulationRecord {
	r.Attributes = map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Attributes[kv[i]] = kv[i+1]
	}
	return r
}

func groupParams() MatchParams {
	p := params("0", 0)
	p.AllowGroups, p.SkipContentDuplicates = true, true
	return p
}

// ── group (many-to-one) allocation ───────────────────────────────────────────

// AR balance vs GL: invoice + credit note + cash application net to the open balance.
func TestGroup_SubledgerBalanceNetsGLPostingsExplicitly(t *testing.T) {
	a := []PopulationRecord{rec("inv1", "INV-1", "40", "USD", "2026-09-01")} // outstanding after partial payment
	b := []PopulationRecord{rec("l1", "INV-1", "100", "USD", "2026-09-01"), rec("l2", "INV-1", "-60", "USD", "2026-09-10")}
	o, err := MatchPopulations(a, b, groupParams())
	if err != nil {
		t.Fatal(err)
	}
	if o.Result() != ResultPass || o.MatchedCount != 1 {
		t.Fatalf("got %+v", o.Exceptions)
	}
	m := o.Matches[0]
	if m.Kind != MatchGroup || m.Outcome != OutcomeGroupExact || m.Difference != "0" || m.Reference != "INV-1" {
		t.Errorf("unexpected match %+v", m)
	}
	if len(m.SideARecords) != 1 || len(m.SideBRecords) != 2 || m.SideBRecords[0] != "l1" || m.SideBRecords[1] != "l2" {
		t.Errorf("the allocation must name every record on each side, got %+v / %+v", m.SideARecords, m.SideBRecords)
	}
}

func TestGroup_SplitPaymentsManyToOne(t *testing.T) {
	a := []PopulationRecord{rec("a1", "PAY-1", "60", "USD", "2026-09-01"), rec("a2", "PAY-1", "40", "USD", "2026-09-02")}
	b := []PopulationRecord{rec("b1", "PAY-1", "100", "USD", "2026-09-02")}
	o, _ := MatchPopulations(a, b, groupParams())
	if o.Result() != ResultPass || o.Matches[0].Kind != MatchGroup || len(o.Matches[0].SideARecords) != 2 {
		t.Fatalf("one-to-many split settlement must reconcile, got %+v %+v", o.Matches, o.Exceptions)
	}
}

// Scenario 14: a double GL posting is not concealed by netting; the group difference exposes it.
func TestGroup_DoublePostedInvoiceIsNotConcealed(t *testing.T) {
	a := []PopulationRecord{rec("bill", "BILL-9", "500", "USD", "2026-09-05")}
	b := []PopulationRecord{rec("g1", "BILL-9", "500", "USD", "2026-09-05"), rec("g2", "BILL-9", "500", "USD", "2026-09-05")}
	o, _ := MatchPopulations(a, b, groupParams())
	if o.Result() != ResultFail || len(o.Exceptions) != 1 || o.Exceptions[0].Reason != ReasonGroupMismatch ||
		o.Exceptions[0].Exposure != "500" {
		t.Fatalf("got %+v", o.Exceptions)
	}
	ids := o.Exceptions[0].RecordIDs
	if len(ids) != 3 {
		t.Errorf("the exception must name every record involved, got %v", ids)
	}
}

// Scenario 13: AR equals GL only if unapplied cash is excluded improperly — the
// record-level comparison exposes the omission even though the totals agree.
func TestGroup_ImproperExclusionSurfacesAsMissingRecord(t *testing.T) {
	a := []PopulationRecord{rec("i1", "INV-1", "100", "USD", "2026-09-01"), rec("i2", "INV-2", "50", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("l1", "INV-1", "150", "USD", "2026-09-01")} // GL lumps both, INV-2 absent
	o, _ := MatchPopulations(a, b, groupParams())
	if o.Result() != ResultFail {
		t.Fatal("must fail")
	}
	got := reasons(o)
	if got[ReasonAmountMismatch] != 1 || got[ReasonMissingDownstream] != 1 {
		t.Fatalf("got %v", got)
	}
	if !o.TotalsAgree {
		t.Error("precondition: the totals do agree (150 vs 150)")
	}
}

func TestGroup_ToleranceAppliesToGroupSumOnly(t *testing.T) {
	a := []PopulationRecord{rec("i1", "INV-1", "100.00", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("l1", "INV-1", "99.99", "USD", "2026-09-01"), rec("l2", "INV-1", "0", "USD", "2026-09-02")}
	p := groupParams()
	p.AbsoluteTolerance = params("0.01", 0).AbsoluteTolerance
	p.DateToleranceDays = 5
	o, _ := MatchPopulations(a, b, p)
	if o.Result() != ResultPass || o.Matches[0].Outcome != OutcomeGroupTolerance || o.Matches[0].Difference != "0.01" {
		t.Fatalf("got %+v %+v", o.Matches, o.Exceptions)
	}
	p.AbsoluteTolerance = params("0.005", 0).AbsoluteTolerance
	if o, _ := MatchPopulations(a, b, p); o.Result() != ResultFail {
		t.Fatal("0.01 exceeds 0.005")
	}
}

func TestGroup_MixedCurrencyIsNeverAllocated(t *testing.T) {
	a := []PopulationRecord{rec("i1", "X", "100", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("l1", "X", "60", "USD", "2026-09-01"), rec("l2", "X", "40", "EUR", "2026-09-01")}
	o, _ := MatchPopulations(a, b, groupParams())
	if o.Result() != ResultFail || reasons(o)[ReasonCurrencyMismatch] != 1 || o.MatchedCount != 0 {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

func TestGroup_RecordsMayLegitimatelySpanDates(t *testing.T) {
	a := []PopulationRecord{rec("i1", "X", "100", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("l1", "X", "60", "USD", "2026-09-01"), rec("l2", "X", "40", "USD", "2026-09-20")}
	if o, _ := MatchPopulations(a, b, groupParams()); o.Result() != ResultPass {
		t.Fatalf("an allocation across dates is normal, got %+v", o.Exceptions)
	}
}

func TestGroup_DisabledMeansAmbiguousNotGuessed(t *testing.T) {
	a := []PopulationRecord{rec("i1", "X", "100", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("l1", "X", "60", "USD", "2026-09-01"), rec("l2", "X", "40", "USD", "2026-09-01")}
	o, _ := MatchPopulations(a, b, params("0", 0))
	if reasons(o)[ReasonAmbiguousKey] != 3 {
		t.Fatalf("without allow_groups the engine must not guess, got %v", reasons(o))
	}
}

func TestGroup_RepeatedRecordIDIsStillADuplicate(t *testing.T) {
	a := []PopulationRecord{rec("i1", "X", "100", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("l1", "X", "100", "USD", "2026-09-01"), rec("l1", "X", "100", "USD", "2026-09-01")}
	o, _ := MatchPopulations(a, b, groupParams())
	if reasons(o)[ReasonDuplicateID] != 1 {
		t.Fatalf("a re-delivered record_id is always a duplicate, got %v", reasons(o))
	}
}

func TestGroup_Deterministic(t *testing.T) {
	a := []PopulationRecord{rec("i2", "B", "5", "USD", "2026-09-01"), rec("i1", "A", "1", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("l3", "B", "2", "USD", "2026-09-01"), rec("l2", "B", "3", "USD", "2026-09-01"), rec("l1", "A", "1", "USD", "2026-09-01")}
	x, _ := MatchPopulations(a, b, groupParams())
	y, _ := MatchPopulations([]PopulationRecord{a[1], a[0]}, []PopulationRecord{b[2], b[1], b[0]}, groupParams())
	j1, _ := json.Marshal(x)
	j2, _ := json.Marshal(y)
	if string(j1) != string(j2) {
		t.Fatalf("group output must not depend on input order:\n%s\n%s", j1, j2)
	}
}

// ── outstanding / reconciling items (§10 timing differences, §14) ────────────

func TestOutstanding_RecentUnmatchedIsAReconcilingItemWithClearingDate(t *testing.T) {
	a := []PopulationRecord{rec("bank1", "DEP-1", "500", "USD", "2026-09-29")} // bank line, not yet in books
	p := params("0", 0)
	p.OutstandingDays, p.PeriodEnd = 5, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	o, _ := MatchPopulations(a, nil, p)
	if len(o.Exceptions) != 1 {
		t.Fatalf("got %+v", o.Exceptions)
	}
	e := o.Exceptions[0]
	if e.Category != CatTiming || e.Reason != ReasonOutstandingItem || e.ExpectedClearing == nil ||
		e.ExpectedClearing.Format("2006-01-02") != "2026-10-04" {
		t.Errorf("unexpected reconciling item %+v", e)
	}
	// Still an exception: a timing item is disclosed and aged, never silently dropped.
	if o.Result() != ResultFail {
		t.Error("outstanding items remain visible exceptions")
	}
}

func TestOutstanding_OldUnmatchedItemStaysMissing(t *testing.T) {
	a := []PopulationRecord{rec("bank1", "DEP-1", "500", "USD", "2026-09-01")}
	p := params("0", 0)
	p.OutstandingDays, p.PeriodEnd = 5, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	o, _ := MatchPopulations(a, nil, p)
	if o.Exceptions[0].Category != CatMissing || o.Exceptions[0].ExpectedClearing != nil {
		t.Fatalf("an item older than the window is a real omission, got %+v", o.Exceptions[0])
	}
}

func TestOutstanding_DisabledByDefault(t *testing.T) {
	o, _ := MatchPopulations([]PopulationRecord{rec("a", "R", "1", "USD", "2026-09-29")}, nil, params("0", 0))
	if o.Exceptions[0].Category != CatMissing {
		t.Fatal("no outstanding handling unless the pinned rule enables it")
	}
}

// ── DUPLICATE_SCAN (FIN-CTRL-028) ────────────────────────────────────────────

func dupLogic(fields ...string) RuleLogic {
	l, err := ParseRuleLogic(json.RawMessage(`{"kind":"DUPLICATE_SCAN","key_fields":["` + join(fields) + `"]}`))
	if err != nil {
		panic(err)
	}
	return l
}

func join(f []string) string {
	s := ""
	for i, x := range f {
		if i > 0 {
			s += `","`
		}
		s += x
	}
	return s
}

func TestDuplicateScan_SameVendorAmountDateDifferentInvoiceNumber(t *testing.T) {
	recs := []PopulationRecord{
		withAttr(rec("i1", "V1-1001", "250", "USD", "2026-09-03"), "vendor_id", "V1"),
		withAttr(rec("i2", "V1-1002", "250", "USD", "2026-09-03"), "vendor_id", "V1"), // same vendor/amount/date, new number
		withAttr(rec("i3", "V2-7", "250", "USD", "2026-09-03"), "vendor_id", "V2"),    // different vendor: fine
	}
	o, err := ScanDuplicates(recs, dupLogic("attr:vendor_id", "amount", "currency", "date"))
	if err != nil {
		t.Fatal(err)
	}
	if o.Result() != ResultFail || len(o.Exceptions) != 1 || o.Exceptions[0].Reason != ReasonDuplicateBusinessKey ||
		o.Exceptions[0].RecordIDs[0] != "i2" || o.Exceptions[0].Exposure != "250" {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

func TestDuplicateScan_CleanPopulationPasses_AndAmountsCompareNumerically(t *testing.T) {
	recs := []PopulationRecord{
		withAttr(rec("i1", "A", "100.0", "USD", "2026-09-03"), "vendor_id", "V1"),
		withAttr(rec("i2", "B", "100.5", "USD", "2026-09-03"), "vendor_id", "V1"),
	}
	if o, _ := ScanDuplicates(recs, dupLogic("attr:vendor_id", "amount")); o.Result() != ResultPass {
		t.Fatalf("got %+v", o.Exceptions)
	}
	recs[1].Amount = "100"
	if o, _ := ScanDuplicates(recs, dupLogic("attr:vendor_id", "amount")); o.Result() != ResultFail {
		t.Fatal("100.0 and 100 are the same amount")
	}
}

func TestDuplicateScan_MissingKeyFieldIsAnExceptionNotASkip(t *testing.T) {
	recs := []PopulationRecord{withAttr(rec("i1", "A", "100", "USD", "2026-09-03"))} // no vendor_id
	o, _ := ScanDuplicates(recs, dupLogic("attr:vendor_id", "amount"))
	if o.Result() != ResultFail || o.Exceptions[0].Reason != ReasonKeyFieldMissing || o.Exceptions[0].Category != CatDataQuality {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

func TestDuplicateScan_RepeatedRecordID(t *testing.T) {
	recs := []PopulationRecord{
		withAttr(rec("i1", "A", "1", "USD", "2026-09-03"), "vendor_id", "V"),
		withAttr(rec("i1", "A", "1", "USD", "2026-09-03"), "vendor_id", "V"),
	}
	o, _ := ScanDuplicates(recs, dupLogic("attr:vendor_id"))
	if len(o.Exceptions) != 1 || o.Exceptions[0].Reason != ReasonDuplicateID {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

// ── SEQUENCE_GAP (FIN-CTRL-027) ──────────────────────────────────────────────

func seqLogic(extra string) RuleLogic {
	l, err := ParseRuleLogic(json.RawMessage(`{"kind":"SEQUENCE_GAP"` + extra + `}`))
	if err != nil {
		panic(err)
	}
	return l
}

func TestSequenceGap_MissingRangesAreReportedPerContiguousRange(t *testing.T) {
	var recs []PopulationRecord
	for _, n := range []string{"000001", "000002", "000005", "000006", "000010"} {
		recs = append(recs, rec("id"+n, "INV-"+n, "10", "USD", "2026-09-01"))
	}
	o, err := ScanSequenceGaps(recs, seqLogic(""))
	if err != nil {
		t.Fatal(err)
	}
	if o.Result() != ResultFail || len(o.Exceptions) != 2 {
		t.Fatalf("got %+v", o.Exceptions)
	}
	if o.Exceptions[0].RecordIDs[0] != "gap:INV-000003-INV-000004" || o.Exceptions[1].RecordIDs[0] != "gap:INV-000007-INV-000009" {
		t.Errorf("unexpected ranges: %v %v", o.Exceptions[0].RecordIDs, o.Exceptions[1].RecordIDs)
	}
	if o.Exceptions[0].Category != CatMissing || o.Exceptions[0].Assertion != "COMPLETENESS" || o.Exceptions[0].Exposure != "0" {
		t.Errorf("gap exceptions are completeness findings, got %+v", o.Exceptions[0])
	}
}

func TestSequenceGap_ContiguousPassesAndSeriesAreIndependent(t *testing.T) {
	recs := []PopulationRecord{
		rec("a1", "INV-1", "1", "USD", "2026-09-01"), rec("a2", "INV-2", "1", "USD", "2026-09-01"), rec("a3", "INV-3", "1", "USD", "2026-09-01"),
		rec("c1", "CN-7", "1", "USD", "2026-09-01"), rec("c2", "CN-8", "1", "USD", "2026-09-01"), // separate series
	}
	if o, _ := ScanSequenceGaps(recs, seqLogic("")); o.Result() != ResultPass {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

func TestSequenceGap_DuplicateNumberAndUnparseableReference(t *testing.T) {
	recs := []PopulationRecord{
		rec("a1", "INV-1", "1", "USD", "2026-09-01"), rec("a2", "INV-2", "1", "USD", "2026-09-01"),
		rec("a2b", "INV-2", "9", "USD", "2026-09-02"), rec("x", "MANUAL", "3", "USD", "2026-09-01"),
	}
	o, _ := ScanSequenceGaps(recs, seqLogic(""))
	got := reasons(o)
	if got[ReasonDuplicateSequence] != 1 || got[ReasonUnparseableSequence] != 1 || o.Result() != ResultFail {
		t.Fatalf("got %v", got)
	}
}

func TestSequenceGap_FindingLimitIsDisclosedNotSilent(t *testing.T) {
	var recs []PopulationRecord
	for i := 1; i <= 20; i += 2 { // 1,3,5,... => 9 gaps
		recs = append(recs, rec(string(rune('a'+i)), "INV-"+itoa(i), "1", "USD", "2026-09-01"))
	}
	o, _ := ScanSequenceGaps(recs, seqLogic(`,"max_findings":3`))
	got := reasons(o)
	if got[ReasonSequenceGap] != 3 || got[ReasonFindingLimit] != 1 {
		t.Fatalf("a suppressed finding count must itself be an exception, got %v", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

// ── DATE_COVERAGE (FIN-CTRL-032, scenario 16) ────────────────────────────────

func covLogic(extra string) RuleLogic {
	l, err := ParseRuleLogic(json.RawMessage(`{"kind":"DATE_COVERAGE","group_attr":"bank_account_id"` + extra + `}`))
	if err != nil {
		panic(err)
	}
	return l
}

func stmt(id, acct, date string) PopulationRecord {
	return withAttr(rec(id, acct+"|"+date, "0", "USD", date), "bank_account_id", acct)
}

var endOfSept = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func TestCoverage_MissingStatementDaysAreCompletenessExceptions(t *testing.T) {
	var recs []PopulationRecord
	for d := 1; d <= 30; d++ {
		if d == 10 || d == 11 || d == 20 {
			continue
		}
		recs = append(recs, stmt("s"+itoa(d), "ACC1", time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")))
	}
	o, err := CheckDateCoverage(recs, covLogic(""), "2026-09", endOfSept)
	if err != nil {
		t.Fatal(err)
	}
	if o.Result() != ResultFail || len(o.Exceptions) != 2 {
		t.Fatalf("got %+v", o.Exceptions)
	}
	if o.Exceptions[0].RecordIDs[0] != "gap:ACC1:2026-09-10..2026-09-11" || o.Exceptions[1].RecordIDs[0] != "gap:ACC1:2026-09-20..2026-09-20" {
		t.Errorf("got %v %v", o.Exceptions[0].RecordIDs, o.Exceptions[1].RecordIDs)
	}
	if o.Exceptions[0].Reason != ReasonStatementDayMissing || o.Exceptions[0].Assertion != "COMPLETENESS" {
		t.Errorf("got %+v", o.Exceptions[0])
	}
}

func TestCoverage_FullCoveragePasses_AndAccountsAreIndependent(t *testing.T) {
	var recs []PopulationRecord
	for d := 1; d <= 30; d++ {
		ds := time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		recs = append(recs, stmt("a"+itoa(d), "ACC1", ds), stmt("b"+itoa(d), "ACC2", ds))
	}
	if o, _ := CheckDateCoverage(recs, covLogic(""), "2026-09", endOfSept); o.Result() != ResultPass {
		t.Fatalf("got %+v", o.Exceptions)
	}
	// ACC2 loses one day: only ACC2 is reported.
	recs = append(recs[:1:1], recs[2:]...)
	o, _ := CheckDateCoverage(recs, covLogic(""), "2026-09", endOfSept)
	if len(o.Exceptions) != 1 || o.Exceptions[0].RecordIDs[0] != "gap:ACC2:2026-09-01..2026-09-01" {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

func TestCoverage_DeadFeedCannotPassBySendingNothing(t *testing.T) {
	o, _ := CheckDateCoverage(nil, covLogic(`,"expected_groups":["ACC9"]`), "2026-09", endOfSept)
	if o.Result() != ResultFail || len(o.Exceptions) != 1 || o.Exceptions[0].RecordIDs[0] != "gap:ACC9:2026-09-01..2026-09-30" {
		t.Fatalf("an expected account with no statements at all must fail, got %+v", o.Exceptions)
	}
}

func TestCoverage_WeekendsAndInProgressPeriod(t *testing.T) {
	// 2026-09-05 is a Saturday, 06 a Sunday.
	var recs []PopulationRecord
	for d := 1; d <= 30; d++ {
		day := time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		recs = append(recs, stmt("s"+itoa(d), "ACC1", day.Format("2006-01-02")))
	}
	if o, _ := CheckDateCoverage(recs, covLogic(`,"skip_weekends":true`), "2026-09", endOfSept); o.Result() != ResultPass {
		t.Fatalf("weekend days are not expected when skip_weekends: %+v", o.Exceptions)
	}
	if o, _ := CheckDateCoverage(recs, covLogic(""), "2026-09", endOfSept); o.Result() != ResultFail {
		t.Fatal("weekends are expected unless skipped")
	}
	// Period still in progress: only days up to "today" are required.
	partial := []PopulationRecord{stmt("s1", "ACC1", "2026-09-01"), stmt("s2", "ACC1", "2026-09-02")}
	if o, _ := CheckDateCoverage(partial, covLogic(""), "2026-09", time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)); o.Result() != ResultPass {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

func TestCoverage_RequiresAPeriod_AndAttributedRecords(t *testing.T) {
	if _, err := CheckDateCoverage(nil, covLogic(""), "", endOfSept); err == nil {
		t.Fatal("a period-less coverage run cannot know which days to expect")
	}
	unattributed := []PopulationRecord{rec("x", "r", "0", "USD", "2026-09-01")}
	o, _ := CheckDateCoverage(unattributed, covLogic(""), "2026-09", endOfSept)
	found := false
	for _, e := range o.Exceptions {
		found = found || e.Reason == ReasonKeyFieldMissing
	}
	if !found {
		t.Fatal("a record with no account attribute must be an exception, not silently ignored")
	}
}

// ── rule logic parsing ───────────────────────────────────────────────────────

func TestParseRuleLogic(t *testing.T) {
	l, err := ParseRuleLogic(nil)
	if err != nil || l.Kind != KindMatch || !l.TwoSided() || l.MaxFindings != 1000 {
		t.Fatalf("empty logic is a plain MATCH: %+v %v", l, err)
	}
	l, err = ParseRuleLogic(json.RawMessage(`{"kind":"match","allow_groups":true,"outstanding_days":5}`))
	if err != nil || !l.AllowGroups || l.OutstandingDays != 5 || l.Kind != KindMatch {
		t.Fatalf("got %+v %v", l, err)
	}
	for name, raw := range map[string]string{
		"unknown kind":      `{"kind":"MAGIC"}`,
		"dup no keys":       `{"kind":"DUPLICATE_SCAN"}`,
		"dup bad key":       `{"kind":"DUPLICATE_SCAN","key_fields":["attr:Bad Name"]}`,
		"outstanding range": `{"kind":"MATCH","outstanding_days":9999}`,
		"negative findings": `{"kind":"SEQUENCE_GAP","max_findings":-1}`,
		"groups no attr":    `{"kind":"DATE_COVERAGE","expected_groups":["A"]}`,
		"not json":          `{`,
	} {
		if _, err := ParseRuleLogic(json.RawMessage(raw)); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
	if l, _ := ParseRuleLogic(json.RawMessage(`{"kind":"SEQUENCE_GAP"}`)); l.TwoSided() {
		t.Error("SEQUENCE_GAP is single-population")
	}
}
