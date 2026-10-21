package domain

import (
	"strings"
	"testing"
)

func refLine(id, amount, ref string) BankStatementLine {
	l := line(id, amount)
	l.BankReference = ref
	return l
}

// subjects maps each item's statement_line_id to the item. It fails the
// test if any line is the subject of more than one suggestion — the
// suppress rule: a copy gets DUPLICATE_ENTRY only, never also a write-off
// or review recommendation.
func subjects(t *testing.T, items []ReconciliationSuggestion) map[string]ReconciliationSuggestion {
	t.Helper()
	out := map[string]ReconciliationSuggestion{}
	for _, it := range items {
		if _, dup := out[it.StatementLineID]; dup {
			t.Fatalf("line %s is the subject of more than one suggestion", it.StatementLineID)
		}
		out[it.StatementLineID] = it
	}
	return out
}

// Same amount, same reference ignoring case and whitespace: the copy gets
// one DUPLICATE_ENTRY naming both lines; the entry it duplicates keeps its
// own missing-reference suggestion, which points at the copy.
func TestDuplicates_ExactReference(t *testing.T) {
	items := analyze(t, refLine("sl-1", "5.00", "REF 123"), refLine("sl-2", "5", "ref123"), refLine("sl-3", "7", "OTHER"))
	if len(items) != 3 {
		t.Fatalf("expected one suggestion per line (3), got %d", len(items))
	}
	by := subjects(t, items)

	dup := by["sl-2"]
	if dup.DiscrepancyType != DiscrepancyDuplicateEntry || dup.Recommendation != RecommendationConfirmDuplicate {
		t.Fatalf("copy: got %s / %s, want DUPLICATE_ENTRY / CONFIRM_DUPLICATE", dup.DiscrepancyType, dup.Recommendation)
	}
	if dup.CounterpartStatementLineID != "sl-1" || dup.SourceRecordB != "REF 123" || dup.DiscrepancyAmount != "0" {
		t.Fatalf("copy: counterpart %q, ref_b %q, discrepancy %s", dup.CounterpartStatementLineID, dup.SourceRecordB, dup.DiscrepancyAmount)
	}
	if dup.ConfidenceScore != duplicateExactReferenceConfidence || dup.SuggestionStatus != StatusRecommended {
		t.Fatalf("copy: confidence %.1f status %s", dup.ConfidenceScore, dup.SuggestionStatus)
	}
	if dup.HeuristicRule != HeuristicDuplicateExact || dup.ToleranceVersion != 7 {
		t.Fatalf("copy: rule %q version %d, want %q / 7", dup.HeuristicRule, dup.ToleranceVersion, HeuristicDuplicateExact)
	}
	for _, want := range []string{"heuristic rule", "sl-2 probably duplicates statement line sl-1", "identical ignoring case and whitespace"} {
		if !strings.Contains(dup.ResolutionNotes, want) {
			t.Fatalf("copy rationale missing %q: %q", want, dup.ResolutionNotes)
		}
	}

	anchor := by["sl-1"]
	if anchor.DiscrepancyType != DiscrepancyMissingReference || anchor.Recommendation != RecommendationWriteOff || anchor.CounterpartStatementLineID != "" {
		t.Fatalf("anchor: got %s / %s / counterpart %q, want its own missing-reference suggestion",
			anchor.DiscrepancyType, anchor.Recommendation, anchor.CounterpartStatementLineID)
	}
	if !strings.Contains(anchor.ResolutionNotes, "1 probable duplicate(s) of it in this run (sl-2)") {
		t.Fatalf("anchor rationale does not point at its copy: %q", anchor.ResolutionNotes)
	}
	if by["sl-3"].DiscrepancyType != DiscrepancyMissingReference {
		t.Fatalf("unrelated line: %s", by["sl-3"].DiscrepancyType)
	}
}

// One reference is the other plus a trailing sequence suffix. The
// unsuffixed original is the anchor even when it has the highest id.
func TestDuplicates_SuffixedReference_AnchorIsTheOriginal(t *testing.T) {
	items := analyze(t,
		refLine("sl-1", "250", "TRX-9001-1"),
		refLine("sl-2", "250", "TRX-9001 (2)"),
		refLine("sl-9", "250", "TRX-9001"),
	)
	by := subjects(t, items)
	for _, id := range []string{"sl-1", "sl-2"} {
		it := by[id]
		if it.DiscrepancyType != DiscrepancyDuplicateEntry || it.CounterpartStatementLineID != "sl-9" {
			t.Fatalf("%s: got %s, counterpart %q; want DUPLICATE_ENTRY of sl-9", id, it.DiscrepancyType, it.CounterpartStatementLineID)
		}
		if it.ConfidenceScore != duplicateSuffixedReferenceConfidence || !strings.Contains(it.ResolutionNotes, "plus a trailing sequence suffix") {
			t.Fatalf("%s: confidence %.1f, rationale %q", id, it.ConfidenceScore, it.ResolutionNotes)
		}
		if it.HeuristicRule != HeuristicDuplicateSuffixed {
			t.Fatalf("%s: rule %q, want %q", id, it.HeuristicRule, HeuristicDuplicateSuffixed)
		}
	}
	if by["sl-9"].DiscrepancyType != DiscrepancyMissingReference || !strings.Contains(by["sl-9"].ResolutionNotes, "(sl-1, sl-2)") {
		t.Fatalf("anchor sl-9: %s / %q", by["sl-9"].DiscrepancyType, by["sl-9"].ResolutionNotes)
	}
}

// Three identical lines: one entry stays, two copies point at it.
func TestDuplicates_Triplicate(t *testing.T) {
	items := analyze(t, refLine("c", "40", "X1"), refLine("a", "40", "X1"), refLine("b", "40.000", "x1"))
	by := subjects(t, items)
	if by["a"].DiscrepancyType != DiscrepancyMissingReference {
		t.Fatalf("lowest id should be the anchor, got %s", by["a"].DiscrepancyType)
	}
	for _, id := range []string{"b", "c"} {
		if by[id].CounterpartStatementLineID != "a" {
			t.Fatalf("%s: counterpart %q, want a", id, by[id].CounterpartStatementLineID)
		}
	}
	// Items stay in population order.
	if items[0].StatementLineID != "c" || items[1].StatementLineID != "a" || items[2].StatementLineID != "b" {
		t.Fatalf("order changed: %s %s %s", items[0].StatementLineID, items[1].StatementLineID, items[2].StatementLineID)
	}
}

// Near-misses that must NOT be reported as duplicates.
func TestDuplicates_NotDuplicates(t *testing.T) {
	cases := map[string][2]BankStatementLine{
		"different amount":       {refLine("1", "100", "R1"), refLine("2", "100.01", "R1")},
		"payment and reversal":   {refLine("1", "100", "R1"), refLine("2", "-100", "R1")},
		"two different suffixes": {refLine("1", "100", "PAY-1"), refLine("2", "100", "PAY-2")},
		"longer number":          {refLine("1", "100", "INV-1001"), refLine("2", "100", "INV-1002")},
		"prefix is not suffix":   {refLine("1", "100", "REF123"), refLine("2", "100", "REF1234")},
		"three-digit suffix":     {refLine("1", "100", "REF"), refLine("2", "100", "REF-123")},
		"empty references":       {refLine("1", "100", ""), refLine("2", "100", "  ")},
	}
	for name, pair := range cases {
		items := analyze(t, pair[0], pair[1])
		for _, it := range items {
			if it.DiscrepancyType == DiscrepancyDuplicateEntry {
				t.Errorf("%s: reported as duplicate (%s ~ %s)", name, pair[0].BankReference, pair[1].BankReference)
			}
		}
	}

	a, b := refLine("1", "100", "R1"), refLine("2", "100", "R1")
	b.CurrencyCode = "EUR"
	for _, it := range analyze(t, a, b) {
		if it.DiscrepancyType == DiscrepancyDuplicateEntry {
			t.Error("different currency: reported as duplicate")
		}
	}
}

func TestNormalizeReferenceAndSuffix(t *testing.T) {
	for in, want := range map[string]string{" ref 12\t3 ": "REF123", "a-b": "A-B", "": ""} {
		if got := normalizeReference(in); got != want {
			t.Errorf("normalizeReference(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"REF-1": "REF", "REF_12": "REF", "REF/3": "REF", "REF.4": "REF", "REF#5": "REF", "REF(2)": "REF",
		"REF-123": "REF-123", "REF1": "REF1", "REF": "REF",
	} {
		if got := duplicateSuffix.ReplaceAllString(in, ""); got != want {
			t.Errorf("strip suffix %q = %q, want %q", in, got, want)
		}
	}
}
