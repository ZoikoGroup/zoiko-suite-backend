package domain

import (
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

func reqWith(a, b []TransactionItem) *AnalyzeReconciliationRequest {
	return &AnalyzeReconciliationRequest{
		LegalEntityID: "le-1", JobName: "job", SourceSystemA: SourceGeneralLedger,
		SourceSystemB: SourceBankStatement, TransactionsA: a, TransactionsB: b,
	}
}

func TestPerformIntelligentReconciliation_AmountMismatch_RationaleNamesTheThreshold(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "r1", Amount: 100.00}},
		[]TransactionItem{{RefID: "r1", Amount: 100 + 32.00}}, // $32.00 discrepancy — below the $50 tolerance
	)
	_, _, _, items := PerformIntelligentReconciliation(req, "job-1", "tenant-1")
	if len(items) != 1 {
		t.Fatalf("expected 1 unmatched item, got %d", len(items))
	}
	item := items[0]

	if item.Recommendation != RecommendationWriteOff {
		t.Fatalf("expected WRITE_OFF below the $%.2f tolerance, got %s", amountMismatchWriteOffTolerance, item.Recommendation)
	}
	if item.ConfidenceScore != amountMismatchConfidence {
		t.Fatalf("expected confidence %.1f, got %.1f", amountMismatchConfidence, item.ConfidenceScore)
	}
	// The rationale must be visible and must name the actual rule — a bare
	// numeric score with no explanation is the defect this fix removes.
	if !strings.Contains(item.ResolutionNotes, "heuristic rule") {
		t.Fatalf("FABRICATION: no visible rationale for the recommendation, got %q", item.ResolutionNotes)
	}
	if !strings.Contains(item.ResolutionNotes, "50.00") {
		t.Fatalf("rationale does not name the threshold actually used, got %q", item.ResolutionNotes)
	}
}

func TestPerformIntelligentReconciliation_AmountMismatch_AtOrAboveTolerance_IsTimingAdjustment(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "r1", Amount: 100.00}},
		[]TransactionItem{{RefID: "r1", Amount: 100 + 50.00}}, // exactly at the tolerance boundary
	)
	_, _, _, items := PerformIntelligentReconciliation(req, "job-1", "tenant-1")
	if items[0].Recommendation != RecommendationTimingAdjustment {
		t.Fatalf("expected TIMING_ADJUSTMENT at the tolerance boundary, got %s", items[0].Recommendation)
	}
}

func TestPerformIntelligentReconciliation_MissingReference_SmallAmount_WriteOffWithRationale(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "orphan", Amount: 5.00}}, // below the $10 small-amount tolerance, no match in B
		nil,
	)
	_, _, _, items := PerformIntelligentReconciliation(req, "job-1", "tenant-1")
	item := items[0]

	if item.Recommendation != RecommendationWriteOff {
		t.Fatalf("expected WRITE_OFF below the $%.2f small-amount tolerance, got %s", missingReferenceSmallAmountTolerance, item.Recommendation)
	}
	if !strings.Contains(item.ResolutionNotes, "heuristic rule") {
		t.Fatalf("FABRICATION: no visible rationale, got %q", item.ResolutionNotes)
	}
}

func TestPerformIntelligentReconciliation_MissingReference_LargerAmount_ManualReviewWithRationale(t *testing.T) {
	req := reqWith(
		[]TransactionItem{{RefID: "orphan", Amount: 500.00}},
		nil,
	)
	_, _, _, items := PerformIntelligentReconciliation(req, "job-1", "tenant-1")
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
