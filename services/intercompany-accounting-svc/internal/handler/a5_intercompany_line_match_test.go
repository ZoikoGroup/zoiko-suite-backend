package handler_test

// SPEC_DEVIATIONS A5: "The match compares the whole target journal, not its
// intercompany line... There is no intercompany-account resolution
// ('Server-resolved: intercompany accounts')." These are end-to-end
// (HTTP-level) proofs that MatchEntry now resolves and compares only the
// intercompany-designated line(s) of the target journal, using
// general-ledger-svc's new IsIntercompanyAccount classification carried on
// each JournalLine (see internal/ledger/client.go).

import (
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
)

// The real scenario A5 names: a target journal with both an intercompany
// line and an unrelated tax line. Under the old whole-journal comparison
// (debits 5000 + 50 = 5050 vs the pair's 5000) this would have mismatched
// even though the actual intercompany position is exactly right — see
// TestOldAndNewComparisonBases_DivergeOnTheSameJournal for the direct
// numeric proof. End to end, MatchEntry must now ignore the tax line and
// match.
func TestMatchEntry_IgnoresUnrelatedTaxLine_MatchesOnIntercompanyLineOnly(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	leg := &stubLedger{journal: &ledger.JournalDetail{
		JournalID: "j-tgt", LegalEntityID: "le-uk", Status: "FINALIZED", CurrencyCode: "USD",
		Lines: []ledger.JournalLine{
			{AccountCode: "1890-Intercompany", DebitAmount: "5000", IsIntercompanyAccount: true},
			{AccountCode: "2200-TAX", DebitAmount: "50", IsIntercompanyAccount: false},
		},
	}}
	resp := match(t, s, leg)
	if resp.MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected MATCHED (the whole-journal total of 5050 would have mismatched a 5000 pair), got %+v", resp)
	}
}

// The other half of A5's defect: "a journal of the right total but the
// wrong accounts will match." A journal whose ordinary line happens to total
// the pair's amount, plus a small intercompany-designated line for a
// different amount, must compare against the intercompany line — not the
// coincidental whole-journal total — and correctly MISMATCH.
func TestMatchEntry_RightWholeJournalTotal_WrongIntercompanyLine_Mismatches(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	leg := &stubLedger{journal: &ledger.JournalDetail{
		JournalID: "j-tgt", LegalEntityID: "le-uk", Status: "FINALIZED", CurrencyCode: "USD",
		Lines: []ledger.JournalLine{
			// The intercompany-designated line is only 4000 — the real
			// position — but an unrelated 1000 debit line brings the whole
			// journal's total to exactly 5000, what the old comparison
			// would have accepted.
			{AccountCode: "1890-Intercompany", DebitAmount: "4000", IsIntercompanyAccount: true},
			{AccountCode: "6000-Other", DebitAmount: "1000", IsIntercompanyAccount: false},
		},
	}}
	resp := match(t, s, leg)
	if resp.MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected MISMATCH (the intercompany line is 4000, not the pair's 5000, even though the whole journal totals 5000), got %+v", resp)
	}
	if resp.FailedRule == nil || *resp.FailedRule != domain.MatchRuleAmountMismatch {
		t.Fatalf("expected failed_rule AMOUNT_MISMATCH, got %+v", resp.FailedRule)
	}
}

// Zero intercompany-designated lines: the journal is real, FINALIZED, the
// right entity and currency — it simply isn't an intercompany entry. There
// is nothing to compare the pair's amount against, so this is a new,
// distinct MISMATCH rule, not a 503 the caller would retry identically.
func TestMatchEntry_NoIntercompanyLines_RecordedAsMismatch(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	leg := &stubLedger{journal: &ledger.JournalDetail{
		JournalID: "j-tgt", LegalEntityID: "le-uk", Status: "FINALIZED", CurrencyCode: "USD",
		Lines: []ledger.JournalLine{
			{AccountCode: "6000-Other", DebitAmount: "5000", IsIntercompanyAccount: false},
		},
	}}
	resp := match(t, s, leg)
	if resp.MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected MISMATCH, got %+v", resp)
	}
	if resp.FailedRule == nil || *resp.FailedRule != domain.MatchRuleNoIntercompanyLines {
		t.Fatalf("expected failed_rule NO_INTERCOMPANY_LINES, got %+v", resp.FailedRule)
	}
	if resp.MismatchReason == nil {
		t.Fatal("expected a mismatch_reason explaining the refusal")
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected the pair's stored match_status to be MISMATCH, got %q", s.entries["e1"].MatchStatus)
	}
}

// Multiple intercompany-designated lines are summed end to end, matching a
// pair whose amount is their total rather than either line alone.
func TestMatchEntry_MultipleIntercompanyLines_SummedToMatch(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	leg := &stubLedger{journal: &ledger.JournalDetail{
		JournalID: "j-tgt", LegalEntityID: "le-uk", Status: "FINALIZED", CurrencyCode: "USD",
		Lines: []ledger.JournalLine{
			{AccountCode: "1890-Intercompany", DebitAmount: "3000", IsIntercompanyAccount: true},
			{AccountCode: "1891-Intercompany", DebitAmount: "2000", IsIntercompanyAccount: true},
			{AccountCode: "2200-TAX", DebitAmount: "50", IsIntercompanyAccount: false},
		},
	}}
	resp := match(t, s, leg)
	if resp.MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected MATCHED (3000 + 2000 = 5000 across the two intercompany lines), got %+v", resp)
	}
}
