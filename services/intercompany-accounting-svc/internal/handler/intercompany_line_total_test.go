package handler

// SPEC_DEVIATIONS A5: "The match compares the whole target journal, not its
// intercompany line... A journal that also carries non-intercompany lines
// (tax, a third-party leg) will mismatch, and a journal of the right total
// but the wrong accounts will match." This is a white-box test (same
// package, not handler_test) so it can call journalTotal (the old,
// whole-journal comparison, kept only for A6's create-time source
// observation) and intercompanyLineTotal (MatchEntry's new comparison
// basis) side by side on the exact same journal and show they diverge —
// the concrete proof that the fix changes a real outcome, not just internal
// bookkeeping.

import (
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/ledger"
)

// A target journal for a 5,000 USD intercompany pair that also carries an
// unrelated 50 USD tax line (general-ledger-svc's chart never classifies
// account 2200-TAX as intercompany).
func journalWithTaxLine() *ledger.JournalDetail {
	return &ledger.JournalDetail{
		JournalID: "j-tgt", LegalEntityID: "le-uk", Status: "FINALIZED", CurrencyCode: "USD",
		Lines: []ledger.JournalLine{
			{AccountCode: "1890-Intercompany", DebitAmount: "5000", IsIntercompanyAccount: true},
			{AccountCode: "2200-TAX", DebitAmount: "50", IsIntercompanyAccount: false},
		},
	}
}

// The old whole-journal comparison (A5's documented defect): the tax line
// inflates the total to 5050, which a 5,000 pair would mismatch on even
// though its actual intercompany position is exactly right.
func TestJournalTotal_OldWholeJournalComparison_InflatedByUnrelatedLine(t *testing.T) {
	j := journalWithTaxLine()
	total, err := journalTotal(j)
	if err != nil {
		t.Fatalf("journalTotal: %v", err)
	}
	if got := total.RatString(); got != "5050" {
		t.Fatalf("expected the old whole-journal total to be inflated to 5050 by the tax line, got %s", got)
	}
}

// The new line-level comparison (SPEC_DEVIATIONS A5's fix): only the
// intercompany-designated line counts, so the tax line never enters the
// comparison and the position resolves to exactly 5000 — the amount the
// pair actually expects.
func TestIntercompanyLineTotal_IgnoresUnrelatedLine(t *testing.T) {
	j := journalWithTaxLine()
	total, found, err := intercompanyLineTotal(j)
	if err != nil {
		t.Fatalf("intercompanyLineTotal: %v", err)
	}
	if !found {
		t.Fatal("expected the intercompany-designated line to be found")
	}
	if got := total.RatString(); got != "5000" {
		t.Fatalf("expected the intercompany-line total to be exactly 5000, got %s", got)
	}
}

// The two functions genuinely diverge on the same journal (5050 vs 5000) —
// this is exactly the gap SPEC_DEVIATIONS A5 names, and MatchEntry switching
// from journalTotal to intercompanyLineTotal is what closes it: a 5,000 pair
// now matches a journal the old comparison would have mismatched solely
// because of an unrelated tax line.
func TestOldAndNewComparisonBases_DivergeOnTheSameJournal(t *testing.T) {
	j := journalWithTaxLine()
	oldTotal, err := journalTotal(j)
	if err != nil {
		t.Fatalf("journalTotal: %v", err)
	}
	newTotal, found, err := intercompanyLineTotal(j)
	if err != nil {
		t.Fatalf("intercompanyLineTotal: %v", err)
	}
	if !found {
		t.Fatal("expected the intercompany line to be found")
	}
	if oldTotal.Cmp(newTotal) == 0 {
		t.Fatalf("expected the old and new comparison bases to diverge on a journal with an unrelated line, both resolved to %s", oldTotal.RatString())
	}
	expectedPairAmount := "5000"
	if newTotal.RatString() != expectedPairAmount {
		t.Fatalf("expected the new (correct) total to equal the pair's amount %s, got %s", expectedPairAmount, newTotal.RatString())
	}
	if oldTotal.RatString() == expectedPairAmount {
		t.Fatalf("expected the old (wrong) total to differ from the pair's amount %s, got %s too", expectedPairAmount, oldTotal.RatString())
	}
}

// Zero intercompany-designated lines: a journal that is entirely ordinary
// (no line the chart classifies intercompany) has nothing for the new
// comparison to use — intercompanyLineTotal reports found=false rather than
// falling back to the whole-journal total MatchEntry no longer trusts.
func TestIntercompanyLineTotal_NoIntercompanyLines_ReportsNotFound(t *testing.T) {
	j := &ledger.JournalDetail{
		JournalID: "j-tgt", LegalEntityID: "le-uk", Status: "FINALIZED", CurrencyCode: "USD",
		Lines: []ledger.JournalLine{
			{AccountCode: "6000", DebitAmount: "5000", IsIntercompanyAccount: false},
		},
	}
	_, found, err := intercompanyLineTotal(j)
	if err != nil {
		t.Fatalf("intercompanyLineTotal: %v", err)
	}
	if found {
		t.Fatal("expected found=false when no line is classified intercompany")
	}
}

// Multiple intercompany-designated lines are summed, not matched against
// only the first — the direct generalisation of the old "sum every debit,
// sum every credit, take the larger" logic, restricted to the lines the
// chart actually designates intercompany. A position split across two lines
// (e.g. by dimension) still nets to the position's real total.
func TestIntercompanyLineTotal_MultipleIntercompanyLines_AreSummed(t *testing.T) {
	j := &ledger.JournalDetail{
		JournalID: "j-tgt", LegalEntityID: "le-uk", Status: "FINALIZED", CurrencyCode: "USD",
		Lines: []ledger.JournalLine{
			{AccountCode: "1890-Intercompany", DebitAmount: "3000", IsIntercompanyAccount: true},
			{AccountCode: "1891-Intercompany", DebitAmount: "2000", IsIntercompanyAccount: true},
			{AccountCode: "2200-TAX", DebitAmount: "50", IsIntercompanyAccount: false},
		},
	}
	total, found, err := intercompanyLineTotal(j)
	if err != nil {
		t.Fatalf("intercompanyLineTotal: %v", err)
	}
	if !found {
		t.Fatal("expected the intercompany lines to be found")
	}
	if got := total.RatString(); got != "5000" {
		t.Fatalf("expected the two intercompany lines summed to 5000 (excluding the tax line), got %s", got)
	}
}
