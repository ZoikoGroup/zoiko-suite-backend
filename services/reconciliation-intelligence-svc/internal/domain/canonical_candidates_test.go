package domain

import (
	"strings"
	"testing"
)

// SPEC_DEVIATIONS A2b: proposing a banking-connector-svc canonical
// transaction as the counterpart of an UNMATCHED statement line no journal
// matched. The fixtures mirror bank-reconciliation-svc's own acceptance rule
// (verifyCanonicalMatch): NORMALIZED, same tenant, same currency, same
// signed amount — plus this service's stricter legal entity and bank account.

func canonicalTxn(id, date, amount string) CanonicalTransaction {
	return CanonicalTransaction{TransactionID: id, TenantID: candTenant, LegalEntityID: candEntity, BankAccountID: candBankAccount,
		Currency: "USD", Status: "NORMALIZED", TransactionDate: date, Amount: rat(amount)}
}

// searchWithTxns is a search with journal entries and canonical transactions
// read for bank account ba-1.
func searchWithTxns(entries []LedgerEntry, txns ...CanonicalTransaction) *CandidateSearch {
	s := searchWith(entries...)
	s.TransactionsByBankAccount[candBankAccount] = txns
	return s
}

func TestCanonicalCandidate_WithinTolerance_ProposesAutoMatchNamingTheTransaction(t *testing.T) {
	got := analyzeCandidates(t, searchWithTxns(nil, canonicalTxn("t-1", "2026-07-02", "125.50")), cashLine("sl-1", "125.50", "2026-07-01"))[0]
	if got.Recommendation != RecommendationAutoMatch || got.CandidateSearchOutcome != CandidateWithinDateTolerance ||
		got.CounterpartTransactionID != "t-1" || got.CounterpartJournalID != "" ||
		got.HeuristicRule != HeuristicCandidateTransactionWithinDateTolerance {
		t.Fatalf("expected an AUTO_MATCH proposal of transaction t-1, got %+v", got)
	}
	if got.SuggestionStatus != StatusRecommended || got.DiscrepancyAmount != "0" || got.AmountB != "125.5" || got.DateToleranceVersion != 5 {
		t.Fatalf("unexpected status, amounts or provenance: %+v", got)
	}
	// Provenance in the rationale: which service, which kind of record, and
	// that no journal matched first.
	for _, want := range []string{"banking-connector-svc canonical transaction t-1", "bank account ba-1", "no FINALIZED general-ledger-svc journal", "1 day(s)", "bank-reconciliation-svc"} {
		if !strings.Contains(got.ResolutionNotes, want) {
			t.Fatalf("rationale is missing %q: %s", want, got.ResolutionNotes)
		}
	}
}

func TestCanonicalCandidate_BeyondTolerance_ProposesDateSkewForReview(t *testing.T) {
	got := analyzeCandidates(t, searchWithTxns(nil, canonicalTxn("t-1", "2026-07-15", "125.50")), cashLine("sl-1", "125.50", "2026-07-01"))[0]
	if got.Recommendation != RecommendationManualReview || got.DiscrepancyType != DiscrepancyDateSkew ||
		got.CandidateSearchOutcome != CandidateDateSkew || got.CounterpartTransactionID != "t-1" ||
		got.HeuristicRule != HeuristicCandidateTransactionDateSkew {
		t.Fatalf("expected a DATE_SKEW proposal of t-1, got %+v", got)
	}
	if !strings.Contains(got.ResolutionNotes, "14 day(s)") || !strings.Contains(got.ResolutionNotes, "canonical transaction") {
		t.Fatalf("rationale must state the day count and the kind of record: %s", got.ResolutionNotes)
	}
	// Beyond the horizon is not a candidate at all.
	got = analyzeCandidates(t, searchWithTxns(nil, canonicalTxn("t-1", "2026-08-15", "125.50")), cashLine("sl-1", "125.50", "2026-07-01"))[0]
	if got.CandidateSearchOutcome != CandidateNone || got.CounterpartTransactionID != "" {
		t.Fatalf("a transaction beyond the horizon was proposed: %+v", got)
	}
}

// NO_CANDIDATE after both searches is a recorded result naming both.
func TestCanonicalCandidate_NothingInEitherService_IsNoCandidateNamingBoth(t *testing.T) {
	got := analyzeCandidates(t, searchWithTxns(nil, canonicalTxn("t-1", "2026-07-01", "99")), cashLine("sl-1", "125.50", "2026-07-01"))[0]
	if got.CandidateSearchOutcome != CandidateNone || got.CounterpartTransactionID != "" || got.CounterpartJournalID != "" {
		t.Fatalf("expected NO_CANDIDATE, got %+v", got)
	}
	if !strings.Contains(got.ResolutionNotes, "general-ledger-svc journal") || !strings.Contains(got.ResolutionNotes, "banking-connector-svc canonical transaction") {
		t.Fatalf("NO_CANDIDATE must say both services were searched: %s", got.ResolutionNotes)
	}
}

// The canonical search runs on NO_CANDIDATE lines only. A line with a
// journal proposal, an ambiguous journal result, or no cash account never
// also gets a canonical-transaction one — even when one would qualify.
func TestCanonicalCandidate_OnlyNoCandidateLinesAreSearched(t *testing.T) {
	qualifying := canonicalTxn("t-1", "2026-07-01", "125.50")

	t.Run("journal proposal stands", func(t *testing.T) {
		got := analyzeCandidates(t, searchWithTxns([]LedgerEntry{cashEntry("j-1", "2026-07-01", "125.50")}, qualifying),
			cashLine("sl-1", "125.50", "2026-07-01"))[0]
		if got.CounterpartJournalID != "j-1" || got.CounterpartTransactionID != "" || got.HeuristicRule != HeuristicCandidateWithinDateTolerance {
			t.Fatalf("a line with a journal candidate got a canonical one: %+v", got)
		}
	})
	t.Run("journal date skew stands", func(t *testing.T) {
		// The transaction is within tolerance, the journal only beyond it:
		// the journal result still wins, the canonical search never runs.
		got := analyzeCandidates(t, searchWithTxns([]LedgerEntry{cashEntry("j-far", "2026-07-20", "125.50")}, qualifying),
			cashLine("sl-1", "125.50", "2026-07-01"))[0]
		if got.CounterpartJournalID != "j-far" || got.CounterpartTransactionID != "" || got.CandidateSearchOutcome != CandidateDateSkew {
			t.Fatalf("a line with a DATE_SKEW journal got a canonical candidate: %+v", got)
		}
	})
	t.Run("journal ambiguity stands", func(t *testing.T) {
		got := analyzeCandidates(t, searchWithTxns([]LedgerEntry{cashEntry("j-a", "2026-07-01", "125.50"), cashEntry("j-b", "2026-07-02", "125.50")}, qualifying),
			cashLine("sl-1", "125.50", "2026-07-01"))[0]
		if got.CounterpartTransactionID != "" || got.HeuristicRule != HeuristicCandidateAmbiguous || strings.Contains(got.ResolutionNotes, "t-1") {
			t.Fatalf("a journal-AMBIGUOUS line got a canonical candidate: %+v", got)
		}
	})
	t.Run("no cash account", func(t *testing.T) {
		noCash := cashLine("sl-1", "125.50", "2026-07-01")
		noCash.GLCashAccountCode = ""
		got := analyzeCandidates(t, searchWithTxns(nil, qualifying), noCash)[0]
		if got.CandidateSearchOutcome != CandidateNoCashAccount || got.CounterpartTransactionID != "" {
			t.Fatalf("a NO_CASH_ACCOUNT line got a canonical candidate: %+v", got)
		}
	})
	t.Run("CanonicalSearchLines names exactly the NO_CANDIDATE lines", func(t *testing.T) {
		withJournal := cashLine("sl-j", "125.50", "2026-07-01")
		without := cashLine("sl-n", "300", "2026-07-01")
		noCash := cashLine("sl-c", "77", "2026-07-01")
		noCash.GLCashAccountCode = ""
		need, err := CanonicalSearchLines([]BankStatementLine{withJournal, without, noCash}, candTenant,
			searchWith(cashEntry("j-1", "2026-07-01", "125.50")))
		if err != nil || len(need) != 1 || need[0].StatementLineID != "sl-n" {
			t.Fatalf("expected only sl-n to need a canonical read, got %+v %v", need, err)
		}
	})
}

// "Could not look" must never become "no candidate": a NO_CANDIDATE line
// whose bank account was not read is refused.
func TestCanonicalCandidate_UnreadBankAccount_IsRefused(t *testing.T) {
	s := searchWith()
	s.TransactionsByBankAccount = nil
	if _, err := AnalyzeUnmatchedStatementLines([]BankStatementLine{cashLine("sl-1", "125.50", "2026-07-01")}, testRunID, candTenant, testMissingReferenceTolerance, s); err == nil {
		t.Fatal("expected a refusal when the canonical search was required but not read")
	}
	// A run where every line has a journal result needs no canonical read.
	s = searchWith(cashEntry("j-1", "2026-07-01", "125.50"))
	s.TransactionsByBankAccount = nil
	if _, err := AnalyzeUnmatchedStatementLines([]BankStatementLine{cashLine("sl-1", "125.50", "2026-07-01")}, testRunID, candTenant, testMissingReferenceTolerance, s); err != nil {
		t.Fatalf("no line needed the canonical read: %v", err)
	}
}

// Exact, signed amount. Both services sign positive as money in, so a
// transaction moving the same figure the other way is not a candidate; and
// the comparison is big.Rat, so two NUMERIC(18,4) amounts that collapse to
// the same float64 are still told apart.
func TestCanonicalCandidate_AmountIsExactAndSigned(t *testing.T) {
	cases := []struct {
		name, line, txn string
		match           bool
	}{
		{"same figure, opposite direction", "-125.50", "125.50", false},
		{"money out matches money out", "-125.50", "-125.5000", true},
		{"one ten-thousandth apart", "125.50", "125.5001", false},
		// 12345678901234.5678 and …5677 are the same float64 (its spacing at
		// that magnitude is about 0.002); as decimals they differ.
		{"equal as float64, not as decimals", "12345678901234.5678", "12345678901234.5677", false},
		{"largest storable, exactly equal", "12345678901234.5678", "12345678901234.5678", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := analyzeCandidates(t, searchWithTxns(nil, canonicalTxn("t-1", "2026-07-01", c.txn)), cashLine("sl-1", c.line, "2026-07-01"))[0]
			if (got.CounterpartTransactionID == "t-1") != c.match {
				t.Fatalf("line %s vs transaction %s: match=%v expected, got %+v", c.line, c.txn, c.match, got)
			}
		})
	}
}

func TestCanonicalCandidate_OutsideScope_IsNeverProposed(t *testing.T) {
	for name, mutate := range map[string]func(*CanonicalTransaction){
		"other tenant":       func(x *CanonicalTransaction) { x.TenantID = "tenant-2" },
		"other legal entity": func(x *CanonicalTransaction) { x.LegalEntityID = "le-2" },
		"other bank account": func(x *CanonicalTransaction) { x.BankAccountID = "ba-2" },
		"other currency":     func(x *CanonicalTransaction) { x.Currency = "EUR" },
		"not NORMALIZED":     func(x *CanonicalTransaction) { x.Status = "SUPERSEDED" },
	} {
		t.Run(name, func(t *testing.T) {
			x := canonicalTxn("t-1", "2026-07-01", "125.50")
			mutate(&x)
			got := analyzeCandidates(t, searchWithTxns(nil, x), cashLine("sl-1", "125.50", "2026-07-01"))[0]
			if got.CounterpartTransactionID != "" || got.CandidateSearchOutcome != CandidateNone {
				t.Fatalf("an out-of-scope transaction was proposed: %+v", got)
			}
		})
	}
}

// Several qualifying transactions: nothing proposed, a human picks, and a
// small amount with counterpart candidates is never recommended for write-off.
func TestCanonicalCandidate_Ambiguous_ProposesNothingAndNeverWritesOff(t *testing.T) {
	got := analyzeCandidates(t, searchWithTxns(nil, canonicalTxn("t-a", "2026-07-01", "5"), canonicalTxn("t-b", "2026-07-02", "5")),
		cashLine("sl-1", "5", "2026-07-01"))[0]
	if got.CandidateSearchOutcome != CandidateAmbiguous || got.CounterpartTransactionID != "" ||
		got.Recommendation != RecommendationManualReview || got.HeuristicRule != HeuristicCandidateTransactionAmbiguous {
		t.Fatalf("expected canonical AMBIGUOUS manual review, got %+v", got)
	}
	if !strings.Contains(got.ResolutionNotes, "t-a") || !strings.Contains(got.ResolutionNotes, "t-b") ||
		!strings.Contains(got.ResolutionNotes, "banking-connector-svc canonical transaction") {
		t.Fatalf("rationale must name the candidates and their source: %s", got.ResolutionNotes)
	}
}

// Cross-line exclusivity across the two sources. One canonical transaction
// claimed by two lines is AMBIGUOUS for both; a journal proposed for one line
// and a transaction proposed for another are different records and both
// stand — even when their ids are the same string.
func TestCanonicalCandidate_CrossLineExclusivity(t *testing.T) {
	const sharedID = "7a1b2c3d-0000-4000-8000-0000000a2b2b"
	journalLine := cashLine("sl-journal", "125.50", "2026-07-01")
	txnLine := cashLine("sl-txn", "300", "2026-07-01")
	claimA := cashLine("sl-claim-a", "42", "2026-07-01")
	claimB := cashLine("sl-claim-b", "42", "2026-07-01")
	claimB.BankReference = "OTHER-PAYMENT"

	search := searchWithTxns([]LedgerEntry{cashEntry(sharedID, "2026-07-01", "125.50")},
		canonicalTxn(sharedID, "2026-07-01", "300"),
		canonicalTxn("t-shared", "2026-07-02", "42"),
		// Also matches the journal line's amount; the journal line is never
		// searched, so this cannot make it ambiguous.
		canonicalTxn("t-decoy", "2026-07-01", "125.50"))
	items := analyzeCandidates(t, search, journalLine, txnLine, claimA, claimB)
	byLine := map[string]ReconciliationSuggestion{}
	for _, it := range items {
		byLine[it.StatementLineID] = it
	}

	if got := byLine["sl-journal"]; got.CounterpartJournalID != sharedID || got.CounterpartTransactionID != "" || got.Recommendation != RecommendationAutoMatch {
		t.Fatalf("journal proposal should stand: %+v", got)
	}
	if got := byLine["sl-txn"]; got.CounterpartTransactionID != sharedID || got.CounterpartJournalID != "" || got.Recommendation != RecommendationAutoMatch {
		t.Fatalf("transaction proposal should stand alongside a journal with the same id: %+v", got)
	}
	for _, id := range []string{"sl-claim-a", "sl-claim-b"} {
		got := byLine[id]
		if got.CounterpartTransactionID != "" || got.CandidateSearchOutcome != CandidateAmbiguous || got.HeuristicRule != HeuristicCandidateTransactionAmbiguous {
			t.Fatalf("a transaction claimed by two lines was proposed for %s: %+v", id, got)
		}
		if !strings.Contains(got.ResolutionNotes, "t-shared") {
			t.Fatalf("rationale must name the shared transaction: %s", got.ResolutionNotes)
		}
	}
}
