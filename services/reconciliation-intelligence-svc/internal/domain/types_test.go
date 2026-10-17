package domain

import (
	"math/big"
	"strings"
	"testing"
)

// Regression tests for the "fabricated intelligence" fix (row from the
// 2026-08-27 master-register review): PerformIntelligentReconciliation's
// resolution recommendations came from two hardcoded threshold comparisons
// presented as a numeric "confidence score" with no visible reasoning. These
// tests pin the current, honestly-documented heuristic behavior — not
// because the heuristic is correct or governed, but so a future change to
// these thresholds is visible in a diff and a test failure, not silent.
//
// The two tolerances are passed explicitly here, mirroring what a real
// caller fetches from financial-control-svc's governed TolerancePolicy
// registry (internal/financialcontrol) — these are no longer package
// constants (ZS-SVC-Z-001 INV-09).
var (
	testAmountMismatchTolerance   = big.NewRat(50, 1)
	testMissingReferenceTolerance = big.NewRat(10, 1)
)

func run(t *testing.T, req *AnalyzeReconciliationRequest) []UnmatchedItem {
	t.Helper()
	_, _, _, items, err := PerformIntelligentReconciliation(req, "job-1", "tenant-1", testAmountMismatchTolerance, testMissingReferenceTolerance)
	if err != nil {
		t.Fatalf("PerformIntelligentReconciliation: %v", err)
	}
	return items
}

func reqWith(a, b []TransactionItem) *AnalyzeReconciliationRequest {
	return &AnalyzeReconciliationRequest{
		LegalEntityID: "le-1", JobName: "job", SourceSystemA: SourceGeneralLedger,
		SourceSystemB: SourceBankStatement, TransactionsA: a, TransactionsB: b,
	}
}

func TestPerformIntelligentReconciliation_AmountMismatch_RationaleNamesTheThreshold(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "r1", Amount: "100.00"}},
		[]TransactionItem{{RefID: "r1", Amount: "132.00"}}, // 32.00 discrepancy — below the $50 tolerance
	)
	items := run(t, req)
	if len(items) != 1 {
		t.Fatalf("expected 1 unmatched item, got %d", len(items))
	}
	item := items[0]

	if item.Recommendation != RecommendationWriteOff {
		t.Fatalf("expected WRITE_OFF below the %s tolerance, got %s", testAmountMismatchTolerance.FloatString(2), item.Recommendation)
	}
	if item.ConfidenceScore != amountMismatchConfidence {
		t.Fatalf("expected confidence %.1f, got %.1f", amountMismatchConfidence, item.ConfidenceScore)
	}
	// The rationale must be visible and must name the actual rule — a bare
	// numeric score with no explanation is the defect this fix removes.
	if !strings.Contains(item.ResolutionNotes, "heuristic rule") {
		t.Fatalf("FABRICATION: no visible rationale for the recommendation, got %q", item.ResolutionNotes)
	}
	if !strings.Contains(item.ResolutionNotes, "discrepancy 32 is below the 50 governed") {
		t.Fatalf("rationale does not name the threshold actually used, got %q", item.ResolutionNotes)
	}
}

func TestPerformIntelligentReconciliation_AmountMismatch_AtOrAboveTolerance_IsTimingAdjustment(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "r1", Amount: "100.00"}},
		[]TransactionItem{{RefID: "r1", Amount: "150.00"}}, // exactly at the tolerance boundary
	)
	items := run(t, req)
	if items[0].Recommendation != RecommendationTimingAdjustment {
		t.Fatalf("expected TIMING_ADJUSTMENT at the tolerance boundary, got %s", items[0].Recommendation)
	}
}

func TestPerformIntelligentReconciliation_MissingReference_SmallAmount_WriteOffWithRationale(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "orphan", Amount: "5.00"}}, // below the 10 small-amount tolerance, no match in B
		nil,
	)
	items := run(t, req)
	item := items[0]

	if item.Recommendation != RecommendationWriteOff {
		t.Fatalf("expected WRITE_OFF below the %s small-amount tolerance, got %s", testMissingReferenceTolerance.FloatString(2), item.Recommendation)
	}
	if !strings.Contains(item.ResolutionNotes, "heuristic rule") {
		t.Fatalf("FABRICATION: no visible rationale, got %q", item.ResolutionNotes)
	}
}

func TestPerformIntelligentReconciliation_MissingReference_LargerAmount_ManualReviewWithRationale(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "orphan", Amount: "500.00"}},
		nil,
	)
	items := run(t, req)
	item := items[0]

	if item.Recommendation != RecommendationManualReview {
		t.Fatalf("expected MANUAL_REVIEW above the small-amount tolerance, got %s", item.Recommendation)
	}
	if !strings.Contains(item.ResolutionNotes, "heuristic rule") {
		t.Fatalf("FABRICATION: no visible rationale, got %q", item.ResolutionNotes)
	}
}

// appendResolutionNote lives in package store, not domain — the equivalent
// behavioral test (the machine rationale must survive an empty-note human
// approval) is in internal/handler, exercised end-to-end through
// ApplyResolution rather than as a unit test on the helper directly.

// A missing-reference credit is judged by its magnitude: a large negative
// amount is never a "small amount" write-off.
func TestPerformIntelligentReconciliation_MissingReference_NegativeAmountUsesMagnitude(t *testing.T) {
	req := reqWith([]TransactionItem{{RefID: "credit", Amount: "-50000.00"}, {RefID: "small", Amount: "-4.00"}}, nil)
	items := run(t, req)
	if len(items) != 2 {
		t.Fatalf("expected 2 unmatched items, got %d", len(items))
	}
	if items[0].Recommendation != RecommendationManualReview || items[0].DiscrepancyAmount != "50000" {
		t.Fatalf("-50000 credit: got %s with discrepancy %s, want MANUAL_REVIEW / 50000",
			items[0].Recommendation, items[0].DiscrepancyAmount)
	}
	if items[1].Recommendation != RecommendationWriteOff || items[1].DiscrepancyAmount != "4" {
		t.Fatalf("-4 credit: got %s with discrepancy %s, want WRITE_OFF / 4",
			items[1].Recommendation, items[1].DiscrepancyAmount)
	}
}

// ZS-DATA-001 D06: money is exact. float64 cannot resolve a one-cent
// difference at this magnitude (its spacing there is ~0.016), so the old
// float path reported a wrong discrepancy; exact decimals report 0.01.
func TestPerformIntelligentReconciliation_LargeAmounts_DiscrepancyIsExact(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "big", Amount: "90071992547409.93"}},
		[]TransactionItem{{RefID: "big", Amount: "90071992547409.92"}},
	)
	items := run(t, req)
	if len(items) != 1 {
		t.Fatalf("expected 1 unmatched item, got %d", len(items))
	}
	if got := items[0]; got.DiscrepancyAmount != "0.01" || got.AmountA != "90071992547409.93" || got.AmountB != "90071992547409.92" {
		t.Fatalf("amounts not exact: a=%s b=%s disc=%s", got.AmountA, got.AmountB, got.DiscrepancyAmount)
	}
}

// Equal values in different spellings ("500" / "500.0000") are a match, and
// the governed tolerance boundary is compared exactly: a discrepancy of
// exactly 0.3 against a 0.3 tolerance is at the boundary, not below it.
func TestPerformIntelligentReconciliation_ExactComparisons(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "same", Amount: "500"}, {RefID: "edge", Amount: "0.1"}},
		[]TransactionItem{{RefID: "same", Amount: "500.0000"}, {RefID: "edge", Amount: "0.4"}},
	)
	matched, _, _, items, err := PerformIntelligentReconciliation(req, "job-1", "tenant-1", big.NewRat(3, 10), testMissingReferenceTolerance)
	if err != nil {
		t.Fatal(err)
	}
	if matched != 1 || len(items) != 1 {
		t.Fatalf("expected 1 match and 1 unmatched, got %d / %d", matched, len(items))
	}
	if items[0].DiscrepancyAmount != "0.3" || items[0].Recommendation != RecommendationTimingAdjustment {
		t.Fatalf("got discrepancy %s / %s, want 0.3 / TIMING_ADJUSTMENT", items[0].DiscrepancyAmount, items[0].Recommendation)
	}
}

func TestPerformIntelligentReconciliation_NilToleranceRefused(t *testing.T) {
	req := reqWith([]TransactionItem{{RefID: "r", Amount: "1"}}, nil)
	if _, _, _, _, err := PerformIntelligentReconciliation(req, "j", "t", nil, testMissingReferenceTolerance); err == nil {
		t.Fatal("expected an error for a missing governed tolerance")
	}
}

func TestAnalyzeReconciliationRequest_Validate_Amounts(t *testing.T) {
	bad := map[string]string{
		"empty":               "",
		"not a number":        "12abc",
		"five decimal places": "1.00001",
		"fifteen int digits":  "100000000000000",
	}
	for name, amt := range bad {
		req := reqWith([]TransactionItem{{RefID: "r", Amount: amt}}, nil)
		if err := req.Validate(); err == nil {
			t.Errorf("%s (%q): expected validation error", name, amt)
		}
		req = reqWith(nil, []TransactionItem{{RefID: "r", Amount: amt}})
		if err := req.Validate(); err == nil {
			t.Errorf("%s (%q) on side B: expected validation error", name, amt)
		}
	}
	for _, amt := range []string{"0", "-4.00", "1200.5", "99999999999999.9999", "-99999999999999.9999"} {
		req := reqWith([]TransactionItem{{RefID: "r", Amount: amt}}, nil)
		if err := req.Validate(); err != nil {
			t.Errorf("%q: unexpected validation error: %v", amt, err)
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
