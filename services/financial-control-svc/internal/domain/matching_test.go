package domain

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

func rec(id, ref, amt, ccy, date string) PopulationRecord {
	return PopulationRecord{RecordID: id, Reference: ref, Amount: amt, Currency: ccy, Date: date}
}

func params(abs string, days int) MatchParams {
	r, _ := new(big.Rat).SetString(abs)
	return MatchParams{AbsoluteTolerance: r, DateToleranceDays: days}
}

func reasons(o MatchOutput) map[string]int {
	m := map[string]int{}
	for _, e := range o.Exceptions {
		m[e.Reason]++
	}
	return m
}

func TestMatch_CleanPopulationsPass(t *testing.T) {
	a := []PopulationRecord{rec("a1", "INV-1", "100.00", "USD", "2026-09-01"), rec("a2", "INV-2", "50.50", "USD", "2026-09-02")}
	b := []PopulationRecord{rec("b1", "INV-1", "100", "USD", "2026-09-01"), rec("b2", "INV-2", "50.5", "USD", "2026-09-02")}
	o, err := MatchPopulations(a, b, params("0", 0))
	if err != nil {
		t.Fatal(err)
	}
	if o.Result() != ResultPass || o.MatchedCount != 2 || len(o.Exceptions) != 0 {
		t.Fatalf("expected clean pass, got %+v", o)
	}
	if !o.TotalsAgree {
		t.Error("totals should agree")
	}
	for _, m := range o.Matches {
		if m.Outcome != OutcomeExact || m.Difference != "0" {
			t.Errorf("unexpected match %+v", m)
		}
	}
}

// Scenario 01: a duplicate source transaction is identified and cannot produce
// a false completeness result.
func TestMatch_DuplicateSourceTransactionIsFlagged(t *testing.T) {
	a := []PopulationRecord{rec("a1", "INV-1", "100", "USD", "2026-09-01"), rec("a2", "INV-1", "100", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "INV-1", "100", "USD", "2026-09-01")}
	o, _ := MatchPopulations(a, b, params("0", 0))
	if o.Result() != ResultFail {
		t.Fatal("a duplicate must fail the run")
	}
	if reasons(o)[ReasonDuplicateContent] != 1 {
		t.Fatalf("expected one DUPLICATE_CONTENT, got %v", reasons(o))
	}
	if o.MatchedCount != 1 {
		t.Errorf("the surviving record still matches once, got %d", o.MatchedCount)
	}
}

// Scenario 14: an AP duplicate invoice posted twice downstream is not concealed by netting.
func TestMatch_DuplicateDownstreamNotConcealedByNetting(t *testing.T) {
	a := []PopulationRecord{rec("a1", "BILL-9", "500", "EUR", "2026-09-05")}
	b := []PopulationRecord{rec("g1", "BILL-9", "500", "EUR", "2026-09-05"), rec("g2", "BILL-9", "500", "EUR", "2026-09-05")}
	o, _ := MatchPopulations(a, b, params("0", 0))
	if o.Result() != ResultFail || reasons(o)[ReasonDuplicateContent] != 1 {
		t.Fatalf("double-posted GL entry must be reported, got %v", reasons(o))
	}
	if o.TotalsAgree {
		t.Error("totals differ (500 vs 1000) and must not be reported as agreeing")
	}
}

func TestMatch_SameRecordIDDeliveredTwice(t *testing.T) {
	a := []PopulationRecord{rec("a1", "INV-1", "10", "USD", "2026-09-01"), rec("a1", "INV-1", "10", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "INV-1", "10", "USD", "2026-09-01")}
	o, _ := MatchPopulations(a, b, params("0", 0))
	if reasons(o)[ReasonDuplicateID] != 1 || o.Result() != ResultFail {
		t.Fatalf("a re-delivered record_id must be flagged, got %v", reasons(o))
	}
}

// Scenario 02: a missing downstream record opens a completeness exception.
func TestMatch_MissingDownstreamIsCompletenessException(t *testing.T) {
	a := []PopulationRecord{rec("a1", "INV-1", "100", "USD", "2026-09-01"), rec("a2", "INV-2", "40", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "INV-1", "100", "USD", "2026-09-01")}
	o, _ := MatchPopulations(a, b, params("0", 0))
	if o.Result() != ResultFail || len(o.Exceptions) != 1 {
		t.Fatalf("got %+v", o.Exceptions)
	}
	e := o.Exceptions[0]
	if e.Category != CatMissing || e.Reason != ReasonMissingDownstream || e.Assertion != "COMPLETENESS" ||
		e.Exposure != "40" || e.Currency != "USD" || e.RecordIDs[0] != "a2" {
		t.Errorf("unexpected exception %+v", e)
	}
}

func TestMatch_UnsupportedDownstreamRecordIsExistenceException(t *testing.T) {
	o, _ := MatchPopulations(nil, []PopulationRecord{rec("b1", "GHOST", "75", "USD", "2026-09-01")}, params("0", 0))
	if len(o.Exceptions) != 1 || o.Exceptions[0].Reason != ReasonMissingSource || o.Exceptions[0].Assertion != "EXISTENCE" {
		t.Fatalf("got %+v", o.Exceptions)
	}
}

// Scenario 03: headline totals agree but composition differs — offsetting errors.
func TestMatch_OffsettingErrorsCannotPassOnTotals(t *testing.T) {
	a := []PopulationRecord{rec("a1", "INV-1", "100", "USD", "2026-09-01"), rec("a2", "INV-2", "200", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "INV-1", "150", "USD", "2026-09-01"), rec("b2", "INV-2", "150", "USD", "2026-09-01")}
	o, _ := MatchPopulations(a, b, params("0", 0))
	if !o.TotalsAgree {
		t.Fatal("precondition: the headline totals (300 vs 300) do agree")
	}
	if o.Result() != ResultFail || reasons(o)[ReasonAmountMismatch] != 2 {
		t.Fatalf("record-level comparison must expose both mismatches, got %v", reasons(o))
	}
}

// Invariant 5: tolerance applies to an existing pair's amount difference and
// nothing else.
func TestMatch_ToleranceAppliesOnlyToPairedAmounts(t *testing.T) {
	a := []PopulationRecord{rec("a1", "INV-1", "100.00", "USD", "2026-09-01"), rec("a2", "INV-2", "40", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "INV-1", "99.99", "USD", "2026-09-01")}
	o, _ := MatchPopulations(a, b, params("1000000", 30)) // huge tolerance
	if o.MatchedCount != 1 || o.Matches[0].Outcome != OutcomeWithinTolerance || o.Matches[0].Difference != "0.01" {
		t.Fatalf("the paired 0.01 difference is within tolerance: %+v", o.Matches)
	}
	if o.Result() != ResultFail || reasons(o)[ReasonMissingDownstream] != 1 {
		t.Fatal("a huge tolerance must not make a MISSING record valid")
	}
}

func TestMatch_DifferenceBeyondPinnedToleranceIsMismatch(t *testing.T) {
	a := []PopulationRecord{rec("a1", "INV-1", "100.00", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "INV-1", "99.98", "USD", "2026-09-01")}
	if o, _ := MatchPopulations(a, b, params("0.01", 0)); o.Result() != ResultFail || o.Exceptions[0].Exposure != "0.02" {
		t.Fatalf("0.02 > 0.01 must fail with exact exposure, got %+v", o.Exceptions)
	}
	if o, _ := MatchPopulations(a, b, params("0.02", 0)); o.Result() != ResultPass {
		t.Fatalf("0.02 <= 0.02 (boundary) must pass, got %+v", o.Exceptions)
	}
}

func TestMatch_ExactDecimalArithmetic(t *testing.T) {
	a := []PopulationRecord{rec("a1", "X", "0.30", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "X", "0.3000000000", "USD", "2026-09-01")}
	if o, _ := MatchPopulations(a, b, params("0", 0)); o.Result() != ResultPass || o.Matches[0].Outcome != OutcomeExact {
		t.Fatal("0.30 and 0.3000000000 are the same exact value")
	}
	big1 := []PopulationRecord{rec("a1", "X", "10000000000000001", "USD", "2026-09-01")}
	big2 := []PopulationRecord{rec("b1", "X", "10000000000000000", "USD", "2026-09-01")}
	if o, _ := MatchPopulations(big1, big2, params("0", 0)); o.Result() != ResultFail {
		t.Fatal("a difference of 1 at 1e16 must be seen (float64 would lose it)")
	}
}

func TestMatch_CurrencyMismatchIsNeverToleratedOrConverted(t *testing.T) {
	a := []PopulationRecord{rec("a1", "X", "100", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "X", "100", "EUR", "2026-09-01")}
	o, _ := MatchPopulations(a, b, params("1000", 30))
	if o.Result() != ResultFail || reasons(o)[ReasonCurrencyMismatch] != 1 {
		t.Fatalf("got %v", reasons(o))
	}
}

func TestMatch_DateToleranceIsSeparateAndExplicit(t *testing.T) {
	a := []PopulationRecord{rec("a1", "X", "100", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "X", "100", "USD", "2026-09-04")}
	if o, _ := MatchPopulations(a, b, params("0", 3)); o.Result() != ResultPass {
		t.Fatal("3 days within a 3-day tolerance")
	}
	o, _ := MatchPopulations(a, b, params("0", 2))
	if o.Result() != ResultFail || o.Exceptions[0].Category != CatTiming || o.Exceptions[0].Assertion != "CUTOFF" {
		t.Fatalf("3 days exceeds a 2-day tolerance and is a TIMING exception, got %+v", o.Exceptions)
	}
}

func TestMatch_AmbiguousReferenceIsNotGuessed(t *testing.T) {
	a := []PopulationRecord{rec("a1", "PAY-1", "60", "USD", "2026-09-01"), rec("a2", "PAY-1", "40", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b1", "PAY-1", "100", "USD", "2026-09-01")}
	o, _ := MatchPopulations(a, b, params("0", 0))
	if o.MatchedCount != 0 || reasons(o)[ReasonAmbiguousKey] != 3 || o.Result() != ResultFail {
		t.Fatalf("split settlement must not be silently paired, got %v", reasons(o))
	}
}

func TestMatch_EmptyPopulationsPass(t *testing.T) {
	o, err := MatchPopulations(nil, nil, params("0", 0))
	if err != nil || o.Result() != ResultPass {
		t.Fatalf("got %v %v", o, err)
	}
}

func TestMatch_InvalidAmountFailsClosed(t *testing.T) {
	_, err := MatchPopulations([]PopulationRecord{rec("a1", "X", "12,5", "USD", "2026-09-01")}, nil, params("0", 0))
	if err == nil {
		t.Fatal("a non-decimal amount must error, not be skipped")
	}
}

func TestMatch_Deterministic(t *testing.T) {
	a := []PopulationRecord{rec("a3", "C", "3", "USD", "2026-09-01"), rec("a1", "A", "1", "USD", "2026-09-01"), rec("a2", "B", "2", "USD", "2026-09-01")}
	b := []PopulationRecord{rec("b2", "B", "9", "USD", "2026-09-01"), rec("b1", "A", "1", "USD", "2026-09-01")}
	first, _ := MatchPopulations(a, b, params("0", 0))
	second, _ := MatchPopulations([]PopulationRecord{a[2], a[1], a[0]}, []PopulationRecord{b[1], b[0]}, params("0", 0))
	f1, _ := json.Marshal(first)
	f2, _ := json.Marshal(second)
	if string(f1) != string(f2) {
		t.Fatalf("output must not depend on input order:\n%s\n%s", f1, f2)
	}
}

func TestHashPopulation_OrderIndependentAndChangeSensitive(t *testing.T) {
	r1, r2 := rec("a", "R1", "1.0", "USD", "2026-09-01"), rec("b", "R2", "2", "USD", "2026-09-02")
	h1, _ := HashPopulation([]PopulationRecord{r1, r2})
	h2, _ := HashPopulation([]PopulationRecord{r2, r1})
	if h1 != h2 || !ValidDigest(h1) {
		t.Fatalf("hash must be order independent and well formed: %s %s", h1, h2)
	}
	same, _ := HashPopulation([]PopulationRecord{rec("a", "R1", "1", "USD", "2026-09-01"), r2})
	if same != h1 {
		t.Error("1.0 and 1 are the same amount")
	}
	for name, mut := range map[string][]PopulationRecord{
		"amount":    {rec("a", "R1", "1.01", "USD", "2026-09-01"), r2},
		"reference": {rec("a", "R9", "1", "USD", "2026-09-01"), r2},
		"currency":  {rec("a", "R1", "1", "EUR", "2026-09-01"), r2},
		"date":      {rec("a", "R1", "1", "USD", "2026-09-02"), r2},
		"removed":   {r2},
		"added":     {r1, r2, rec("c", "R3", "3", "USD", "2026-09-03")},
	} {
		if h, _ := HashPopulation(mut); h == h1 {
			t.Errorf("changing %s must change the hash", name)
		}
	}
}

func TestComputeTotals_PerCurrencyExact(t *testing.T) {
	tot, err := ComputeTotals([]PopulationRecord{
		rec("1", "a", "0.10", "USD", "2026-09-01"), rec("2", "b", "0.20", "USD", "2026-09-01"),
		rec("3", "c", "5", "EUR", "2026-09-01"), rec("4", "d", "-1.5", "EUR", "2026-09-01")})
	if err != nil {
		t.Fatal(err)
	}
	if len(tot) != 2 || tot[0].Currency != "EUR" || tot[0].Total != "3.5" || tot[0].Count != 2 ||
		tot[1].Currency != "USD" || tot[1].Total != "0.3" {
		t.Fatalf("got %+v", tot)
	}
}

func TestValidateRecord(t *testing.T) {
	if err := ValidateRecord(rec("a", "R", "1", "USD", "2026-09-01")); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]PopulationRecord{
		"no id": rec("", "R", "1", "USD", "2026-09-01"), "no ref": rec("a", "", "1", "USD", "2026-09-01"),
		"bad ccy": rec("a", "R", "1", "usd", "2026-09-01"), "bad date": rec("a", "R", "1", "USD", "01/09/2026"),
		"bad amount": rec("a", "R", "one", "USD", "2026-09-01"),
	} {
		if ValidateRecord(r) == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestDeriveSeverity(t *testing.T) {
	m := &MaterialityView{AmountThreshold: "1000", Currency: "USD"}
	cases := []struct {
		cat, exp, ccy string
		mat           *MaterialityView
		want          Severity
	}{
		{CatMismatch, "1000", "USD", m, SeverityHigh},
		{CatMismatch, "5000", "USD", m, SeverityHigh},
		{CatMismatch, "100", "USD", m, SeverityMedium},
		{CatMismatch, "99.99", "USD", m, SeverityLow},
		{CatMissing, "1", "USD", m, SeverityMedium},    // record-integrity floor
		{CatDuplicate, "1", "USD", m, SeverityMedium},  // record-integrity floor
		{CatMismatch, "1", "USD", nil, SeverityMedium}, // no policy: not treated as trivial
		{CatMismatch, "1", "EUR", m, SeverityMedium},   // needs an FX basis we do not have
	}
	for _, c := range cases {
		if got := DeriveSeverity(c.cat, c.exp, c.ccy, c.mat); got != c.want {
			t.Errorf("%s %s %s: got %s want %s", c.cat, c.exp, c.ccy, got, c.want)
		}
	}
}

func TestExceptionAttention_DerivedNotAuthoritative(t *testing.T) {
	past := ControlException{State: ExOpen, Severity: SeverityHigh}
	att := past.DeriveAttention(past.DueAt.AddDate(0, 0, 1))
	if len(att) != 3 {
		t.Fatalf("overdue open high exception: got %v", att)
	}
	closed := ControlException{State: ExClosed}
	if len(closed.DeriveAttention(closed.DueAt.AddDate(0, 0, 9))) != 0 {
		t.Error("a closed exception is never overdue")
	}
}

func TestEvidence_SealVerifyAndTamperDetection(t *testing.T) {
	c := EvidenceContent{Run: RunEvidence{RunID: "r1", TenantID: "t"},
		Execution: ExecutionEvidence{Algorithm: MatchAlgorithmVersion, MatchedCount: 5, Result: ResultPass}}
	raw, digest, err := SealEvidence(c)
	if err != nil || !ValidDigest(digest) {
		t.Fatal(err, digest)
	}
	if !VerifyEvidence(raw, digest) {
		t.Fatal("untouched content must verify")
	}
	// JSONB-style re-serialisation (reordered keys, whitespace) must still verify.
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	reser, _ := json.MarshalIndent(v, "", "   ")
	if !VerifyEvidence(reser, digest) {
		t.Fatal("re-serialisation must not break verification")
	}
	if !strings.Contains(string(raw), `"matched_count":5`) {
		t.Fatal("test precondition: matched_count present")
	}
	tampered := strings.Replace(string(raw), `"matched_count":5`, `"matched_count":6`, 1)
	if VerifyEvidence([]byte(tampered), digest) {
		t.Fatal("tampered evidence must NOT verify")
	}
}
