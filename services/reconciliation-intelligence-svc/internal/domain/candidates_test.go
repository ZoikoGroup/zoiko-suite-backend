package domain

import (
	"math/big"
	"strings"
	"testing"
)

// SPEC_DEVIATIONS A2a: proposing the book side of an UNMATCHED statement
// line. The fixtures mirror bank-reconciliation-svc's own acceptance rule
// (verifyJournalMatches): a FINALIZED journal of the line's legal entity
// whose net movement on the line's gl_cash_account_code equals the line's
// signed amount.

const (
	candTenant = "tenant-1"
	candEntity = "le-1"
	candCash   = "1000-Cash"
)

func cashLine(id, amount, date string) BankStatementLine {
	return BankStatementLine{StatementLineID: id, LegalEntityID: candEntity, BankAccountID: "ba-1", StatementDate: date,
		Amount: amount, CurrencyCode: "USD", BankReference: "REF-" + id, GLCashAccountCode: candCash}
}

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

// debitEntry is a cash-account entry of journal on date: a positive amount is
// a debit (money in), a negative one a credit (money out).
func cashEntry(journal, date, amount string) LedgerEntry {
	e := LedgerEntry{JournalID: journal, TenantID: candTenant, LegalEntityID: candEntity, AccountCode: candCash,
		CurrencyCode: "USD", TransactionDate: date, Debit: new(big.Rat), Credit: new(big.Rat)}
	if strings.HasPrefix(amount, "-") {
		e.Credit = rat(amount[1:])
	} else {
		e.Debit = rat(amount)
	}
	return e
}

func searchWith(entries ...LedgerEntry) *CandidateSearch {
	return &CandidateSearch{Tolerance: DateTolerance{Days: 3, Version: 5}, HorizonDays: 31,
		EntriesByAccount: map[string][]LedgerEntry{candCash: entries}}
}

func analyzeCandidates(t *testing.T, search *CandidateSearch, lines ...BankStatementLine) []ReconciliationSuggestion {
	t.Helper()
	items, err := AnalyzeUnmatchedStatementLines(lines, testRunID, candTenant, testMissingReferenceTolerance, search)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(items) != len(lines) {
		t.Fatalf("every line must get exactly one suggestion: %d lines, %d suggestions", len(lines), len(items))
	}
	return items
}

func TestCandidate_SameAmountWithinTolerance_ProposesAutoMatch(t *testing.T) {
	items := analyzeCandidates(t, searchWith(cashEntry("j-1", "2026-07-03", "125.50")), cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.Recommendation != RecommendationAutoMatch || got.CandidateSearchOutcome != CandidateWithinDateTolerance ||
		got.CounterpartJournalID != "j-1" || got.HeuristicRule != HeuristicCandidateWithinDateTolerance {
		t.Fatalf("expected an AUTO_MATCH proposal of j-1, got %+v", got)
	}
	if got.SuggestionStatus != StatusRecommended {
		t.Fatalf("a proposal is advisory: status %s", got.SuggestionStatus)
	}
	if got.DiscrepancyAmount != "0" || got.AmountB != "125.5" || got.DateToleranceVersion != 5 || got.ToleranceVersion != testMissingReferenceTolerance.Version {
		t.Fatalf("unexpected amounts or provenance: %+v", got)
	}
	if !strings.Contains(got.ResolutionNotes, "2 day(s)") || !strings.Contains(got.ResolutionNotes, "bank-reconciliation-svc") {
		t.Fatalf("rationale must state the day count and where the match is made: %s", got.ResolutionNotes)
	}
}

func TestCandidate_SameAmountBeyondTolerance_ProposesDateSkewForReview(t *testing.T) {
	items := analyzeCandidates(t, searchWith(cashEntry("j-1", "2026-07-10", "125.50")), cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.DiscrepancyType != DiscrepancyDateSkew || got.CandidateSearchOutcome != CandidateDateSkew ||
		got.Recommendation != RecommendationManualReview || got.CounterpartJournalID != "j-1" {
		t.Fatalf("expected a DATE_SKEW proposal of j-1 for manual review, got %+v", got)
	}
	if !strings.Contains(got.ResolutionNotes, "9 day(s)") {
		t.Fatalf("rationale must state the 9-day skew: %s", got.ResolutionNotes)
	}
}

// No candidate is an explicit, recorded result — the line still gets its
// suggestion and says what the search looked for.
func TestCandidate_NoCandidate_IsAnExplicitResult(t *testing.T) {
	items := analyzeCandidates(t, searchWith(), cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CandidateSearchOutcome != CandidateNone || got.CounterpartJournalID != "" || got.DateToleranceVersion != 5 {
		t.Fatalf("expected NO_CANDIDATE recorded with its tolerance version, got %+v", got)
	}
	if got.Recommendation == RecommendationAutoMatch || !strings.Contains(got.ResolutionNotes, "found no FINALIZED journal") {
		t.Fatalf("expected the missing-reference suggestion with the search result stated, got %+v", got)
	}
}

// Same figure, but not this line's book side: another legal entity, another
// currency, another tenant. None may be proposed.
func TestCandidate_SameAmountOutsideScope_IsNeverProposed(t *testing.T) {
	other := func(mut func(*LedgerEntry)) LedgerEntry {
		e := cashEntry("j-x", "2026-07-02", "125.50")
		mut(&e)
		return e
	}
	for name, e := range map[string]LedgerEntry{
		"other legal entity": other(func(e *LedgerEntry) { e.LegalEntityID = "le-2" }),
		"other currency":     other(func(e *LedgerEntry) { e.CurrencyCode = "EUR" }),
		"other tenant":       other(func(e *LedgerEntry) { e.TenantID = "tenant-2" }),
	} {
		t.Run(name, func(t *testing.T) {
			got := analyzeCandidates(t, searchWith(e), cashLine("sl-1", "125.50", "2026-07-01"))[0]
			if got.CounterpartJournalID != "" || got.CandidateSearchOutcome != CandidateNone || got.Recommendation == RecommendationAutoMatch {
				t.Fatalf("an out-of-scope journal was proposed: %+v", got)
			}
		})
	}
	// One out-of-scope line of an otherwise matching journal disqualifies it.
	mixed := cashEntry("j-mixed", "2026-07-02", "125.50")
	foreignLine := cashEntry("j-mixed", "2026-07-02", "0")
	foreignLine.CurrencyCode = "EUR"
	if got := analyzeCandidates(t, searchWith(mixed, foreignLine), cashLine("sl-1", "125.50", "2026-07-01"))[0]; got.CounterpartJournalID != "" {
		t.Fatalf("a journal with an out-of-scope entry was proposed: %+v", got)
	}
}

// Direction and net movement are bank-reconciliation-svc's own test: money
// in is a debit to cash, and a journal's cash lines are netted.
func TestCandidate_DirectionAndNetMovement(t *testing.T) {
	if got := analyzeCandidates(t, searchWith(cashEntry("j-in", "2026-07-01", "125.50")), cashLine("sl-out", "-125.50", "2026-07-01"))[0]; got.CounterpartJournalID != "" {
		t.Fatalf("a debit (money in) was proposed for a payment out: %+v", got)
	}
	if got := analyzeCandidates(t, searchWith(cashEntry("j-out", "2026-07-01", "-125.50")), cashLine("sl-out", "-125.50", "2026-07-01"))[0]; got.CounterpartJournalID != "j-out" {
		t.Fatalf("a credit (money out) should be proposed for a payment out: %+v", got)
	}
	split := searchWith(cashEntry("j-split", "2026-07-01", "100"), cashEntry("j-split", "2026-07-01", "25.50"))
	if got := analyzeCandidates(t, split, cashLine("sl-1", "125.50", "2026-07-01"))[0]; got.CounterpartJournalID != "j-split" {
		t.Fatalf("a journal whose cash lines net to the amount should be proposed: %+v", got)
	}
	netted := searchWith(cashEntry("j-net", "2026-07-01", "125.50"), cashEntry("j-net", "2026-07-01", "-10"))
	if got := analyzeCandidates(t, netted, cashLine("sl-1", "125.50", "2026-07-01"))[0]; got.CounterpartJournalID != "" {
		t.Fatalf("a journal netting to 115.50 was proposed for 125.50: %+v", got)
	}
}

// Several qualifying journals: nothing is proposed, a human picks — and a
// small amount with book-side candidates is never recommended for write-off.
func TestCandidate_Ambiguous_ProposesNothingAndNeverWritesOff(t *testing.T) {
	got := analyzeCandidates(t, searchWith(cashEntry("j-a", "2026-07-01", "5"), cashEntry("j-b", "2026-07-02", "5")),
		cashLine("sl-1", "5", "2026-07-01"))[0]
	if got.CandidateSearchOutcome != CandidateAmbiguous || got.CounterpartJournalID != "" ||
		got.Recommendation != RecommendationManualReview || got.HeuristicRule != HeuristicCandidateAmbiguous {
		t.Fatalf("expected AMBIGUOUS manual review, got %+v", got)
	}
	if !strings.Contains(got.ResolutionNotes, "j-a") || !strings.Contains(got.ResolutionNotes, "j-b") {
		t.Fatalf("rationale must name the candidates: %s", got.ResolutionNotes)
	}
}

// One journal is one book entry; proposing it for two lines would invite a
// human to confirm both.
func TestCandidate_OneJournalQualifyingForTwoLines_IsAmbiguousForBoth(t *testing.T) {
	a, b := cashLine("sl-a", "125.50", "2026-07-01"), cashLine("sl-b", "125.50", "2026-07-01")
	b.BankReference = "OTHER-PAYMENT"
	items := analyzeCandidates(t, searchWith(cashEntry("j-1", "2026-07-01", "125.50")), a, b)
	for _, got := range items {
		if got.CounterpartJournalID != "" || got.CandidateSearchOutcome != CandidateAmbiguous {
			t.Fatalf("a journal claimed by two lines was proposed: %+v", got)
		}
	}
}

func TestCandidate_WithinToleranceWinsOverDateSkew(t *testing.T) {
	got := analyzeCandidates(t, searchWith(cashEntry("j-near", "2026-07-02", "125.50"), cashEntry("j-far", "2026-07-20", "125.50")),
		cashLine("sl-1", "125.50", "2026-07-01"))[0]
	if got.Recommendation != RecommendationAutoMatch || got.CounterpartJournalID != "j-near" {
		t.Fatalf("expected the in-tolerance journal to be proposed, got %+v", got)
	}
}

func TestCandidate_NoCashAccountAndDuplicateCopies(t *testing.T) {
	noCash := cashLine("sl-1", "125.50", "2026-07-01")
	noCash.GLCashAccountCode = ""
	if got := analyzeCandidates(t, searchWith(cashEntry("j-1", "2026-07-01", "125.50")), noCash)[0]; got.CandidateSearchOutcome != CandidateNoCashAccount || got.CounterpartJournalID != "" {
		t.Fatalf("expected NO_CASH_ACCOUNT, got %+v", got)
	}

	// A copy is not searched; the entry it duplicates is, and gets the
	// proposal.
	anchor, copyLine := cashLine("sl-a", "125.50", "2026-07-01"), cashLine("sl-b", "125.50", "2026-07-01")
	copyLine.BankReference = anchor.BankReference
	items := analyzeCandidates(t, searchWith(cashEntry("j-1", "2026-07-01", "125.50")), anchor, copyLine)
	var proposed, dup int
	for _, it := range items {
		switch it.DiscrepancyType {
		case DiscrepancyDuplicateEntry:
			dup++
			if it.CandidateSearchOutcome != "" || it.DateToleranceVersion != 0 {
				t.Fatalf("a duplicate copy must not be searched: %+v", it)
			}
		default:
			if it.CounterpartJournalID == "j-1" {
				proposed++
			}
		}
	}
	if dup != 1 || proposed != 1 {
		t.Fatalf("expected one DUPLICATE_ENTRY and one proposal of j-1, got dup=%d proposed=%d", dup, proposed)
	}
}

func TestAnalyze_CandidateSearchRequired(t *testing.T) {
	for _, s := range []*CandidateSearch{nil,
		{Tolerance: DateTolerance{Days: 3}, HorizonDays: 31},              // no version
		{Tolerance: DateTolerance{Days: 40, Version: 1}, HorizonDays: 31}, // horizon narrower than tolerance
	} {
		if _, err := AnalyzeUnmatchedStatementLines([]BankStatementLine{line("x", "1")}, testRunID, "t", testMissingReferenceTolerance, s); err == nil {
			t.Fatalf("expected refusal for %+v", s)
		}
	}
}
