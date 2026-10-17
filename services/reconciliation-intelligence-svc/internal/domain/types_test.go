package domain

import (
	"math/big"
	"strings"
	"testing"
)

// Regression tests for the "fabricated intelligence" fix (row from the
// 2026-08-27 master-register review): recommendations came from hardcoded
// threshold comparisons presented as a numeric "confidence score" with no
// visible reasoning. These tests pin the current, honestly-documented
// heuristic behavior — not because the heuristic is correct or governed, but
// so a future change to the threshold is visible in a diff and a test
// failure, not silent.
//
// The tolerance is passed explicitly here, mirroring what a real caller
// fetches from financial-control-svc's governed TolerancePolicy registry
// (internal/financialcontrol) — it is not a package constant (ZS-SVC-Z-001
// INV-09).
var testMissingReferenceTolerance = big.NewRat(10, 1)

const testRunID = "6f1c2a8e-3b7d-4c55-9a10-2d4e8f7b9c01"

func line(id, amount string) BankStatementLine {
	return BankStatementLine{
		StatementLineID: id, LegalEntityID: "le-1", BankAccountID: "ba-1",
		StatementDate: "2026-07-01", Amount: amount, CurrencyCode: "USD", BankReference: "REF-" + id,
	}
}

func analyze(t *testing.T, lines ...BankStatementLine) []UnmatchedItem {
	t.Helper()
	items, err := AnalyzeUnmatchedStatementLines(lines, testRunID, "tenant-1", testMissingReferenceTolerance)
	if err != nil {
		t.Fatalf("AnalyzeUnmatchedStatementLines: %v", err)
	}
	return items
}

func TestAnalyze_SmallAmount_WriteOffWithRationale(t *testing.T) {
	items := analyze(t, line("sl-1", "5.00"))
	item := items[0]

	if item.Recommendation != RecommendationWriteOff {
		t.Fatalf("expected WRITE_OFF below the 10 small-amount tolerance, got %s", item.Recommendation)
	}
	if item.ConfidenceScore != missingReferenceWriteOffConfidence {
		t.Fatalf("expected confidence %.1f, got %.1f", missingReferenceWriteOffConfidence, item.ConfidenceScore)
	}
	// The rationale must be visible and must name the actual rule, the record
	// and the threshold — a bare numeric score is the defect this removes.
	for _, want := range []string{"heuristic rule", "statement line sl-1", "run " + testRunID, "magnitude 5 is below the 10 governed"} {
		if !strings.Contains(item.ResolutionNotes, want) {
			t.Fatalf("rationale missing %q, got %q", want, item.ResolutionNotes)
		}
	}
}

func TestAnalyze_LargerAmount_ManualReviewWithRationale(t *testing.T) {
	item := analyze(t, line("sl-1", "500.00"))[0]
	if item.Recommendation != RecommendationManualReview || item.ConfidenceScore != missingReferenceManualReviewConfidence {
		t.Fatalf("expected MANUAL_REVIEW/%.1f above the tolerance, got %s/%.1f",
			missingReferenceManualReviewConfidence, item.Recommendation, item.ConfidenceScore)
	}
	if !strings.Contains(item.ResolutionNotes, "heuristic rule") {
		t.Fatalf("FABRICATION: no visible rationale, got %q", item.ResolutionNotes)
	}
}

// The tolerance boundary is compared exactly: an amount of exactly the
// tolerance is at it, not below it.
func TestAnalyze_ExactlyAtTolerance_IsManualReview(t *testing.T) {
	item := analyze(t, line("sl-1", "10.0000"))[0]
	if item.Recommendation != RecommendationManualReview {
		t.Fatalf("expected MANUAL_REVIEW at the tolerance boundary, got %s", item.Recommendation)
	}
}

// A credit is judged by its magnitude: a large negative amount is never a
// "small amount" write-off.
func TestAnalyze_NegativeAmountUsesMagnitude(t *testing.T) {
	items := analyze(t, line("credit", "-50000.00"), line("small", "-4.00"))
	if items[0].Recommendation != RecommendationManualReview || items[0].DiscrepancyAmount != "50000" || items[0].AmountA != "-50000" {
		t.Fatalf("-50000 credit: got %s, amount %s, discrepancy %s; want MANUAL_REVIEW, -50000, 50000",
			items[0].Recommendation, items[0].AmountA, items[0].DiscrepancyAmount)
	}
	if items[1].Recommendation != RecommendationWriteOff || items[1].DiscrepancyAmount != "4" {
		t.Fatalf("-4 credit: got %s with discrepancy %s, want WRITE_OFF / 4", items[1].Recommendation, items[1].DiscrepancyAmount)
	}
}

// A3: the population is single-sided. Every UNMATCHED line is examined once
// and yields exactly one suggestion, in order — none is skipped, none is
// counted as matched — and each one names the authoritative record (A1).
func TestAnalyze_EveryLineYieldsOneTraceableSuggestion(t *testing.T) {
	items := analyze(t, line("sl-1", "1"), line("sl-2", "20"), line("sl-3", "-300.5"))
	if len(items) != 3 {
		t.Fatalf("expected one suggestion per line (3), got %d", len(items))
	}
	for i, id := range []string{"sl-1", "sl-2", "sl-3"} {
		it := items[i]
		if it.StatementLineID != id || it.BankReconciliationRunID != testRunID {
			t.Fatalf("item %d: source = run %q line %q, want run %q line %q", i, it.BankReconciliationRunID, it.StatementLineID, testRunID, id)
		}
		if it.TransactionRefA != "REF-"+id || it.DiscrepancyType != DiscrepancyMissingReference || it.ResolutionStatus != StatusRecommended {
			t.Fatalf("item %d: got ref %q type %s status %s", i, it.TransactionRefA, it.DiscrepancyType, it.ResolutionStatus)
		}
	}
}

func TestAnalyze_EmptyPopulation_NoSuggestions(t *testing.T) {
	if items := analyze(t); len(items) != 0 {
		t.Fatalf("expected no suggestions for no lines, got %d", len(items))
	}
}

// ZS-DATA-001 D06: money is exact. float64 cannot hold 90071992547409.93
// (its spacing at that magnitude is ~0.016); the amount survives unchanged.
func TestAnalyze_LargeAmountIsExact(t *testing.T) {
	item := analyze(t, line("big", "90071992547409.93"))[0]
	if item.AmountA != "90071992547409.93" || item.DiscrepancyAmount != "90071992547409.93" {
		t.Fatalf("amount not exact: amount %s, discrepancy %s", item.AmountA, item.DiscrepancyAmount)
	}
}

func TestAnalyze_UnstorableAmountRefused(t *testing.T) {
	for _, amt := range []string{"", "abc", "1.00001", "100000000000000"} {
		if _, err := AnalyzeUnmatchedStatementLines([]BankStatementLine{line("x", amt)}, testRunID, "t", testMissingReferenceTolerance); err == nil {
			t.Errorf("%q: expected an error", amt)
		}
	}
}

func TestAnalyze_NilToleranceRefused(t *testing.T) {
	if _, err := AnalyzeUnmatchedStatementLines([]BankStatementLine{line("x", "1")}, testRunID, "t", nil); err == nil {
		t.Fatal("expected an error for a missing governed tolerance")
	}
}

func TestAnalyzeReconciliationRequest_Validate(t *testing.T) {
	ok := AnalyzeReconciliationRequest{LegalEntityID: "le-1", JobName: "j", BankReconciliationRunID: testRunID}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	for name, mutate := range map[string]func(*AnalyzeReconciliationRequest){
		"no legal entity": func(r *AnalyzeReconciliationRequest) { r.LegalEntityID = "" },
		"no job name":     func(r *AnalyzeReconciliationRequest) { r.JobName = "" },
		"no run id":       func(r *AnalyzeReconciliationRequest) { r.BankReconciliationRunID = "" },
		"run id not uuid": func(r *AnalyzeReconciliationRequest) { r.BankReconciliationRunID = "../../v1/statement-lines" },
	} {
		r := ok
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestValidateResolutionTransition(t *testing.T) {
	allowed := [][2]ResolutionStatus{
		{StatusRecommended, StatusApproved}, {StatusRecommended, StatusRejected}, {StatusApproved, StatusExecuted},
	}
	for _, e := range allowed {
		if err := ValidateResolutionTransition(e[0], e[1]); err != nil {
			t.Fatalf("%s -> %s should be allowed: %v", e[0], e[1], err)
		}
	}
	refused := [][2]ResolutionStatus{
		{StatusRecommended, StatusExecuted}, {StatusApproved, StatusRejected}, {StatusApproved, StatusApproved},
		{StatusRejected, StatusApproved}, {StatusRejected, StatusExecuted}, {StatusExecuted, StatusRejected},
		{StatusRecommended, StatusRecommended}, {StatusRecommended, "BOGUS"},
	}
	for _, e := range refused {
		if err := ValidateResolutionTransition(e[0], e[1]); err == nil {
			t.Fatalf("%s -> %s should be refused", e[0], e[1])
		}
	}
}
