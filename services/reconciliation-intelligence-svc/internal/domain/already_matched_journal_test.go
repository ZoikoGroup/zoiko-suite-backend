package domain

// SPEC_DEVIATIONS A2b part 3: a journal bank-reconciliation-svc already
// reports as matched or pending-confirmation elsewhere must not be proposed
// again, alone or as half of an A2b part 2 pair.

import "testing"

func TestCandidate_AlreadyMatchedJournal_ExcludedFromSingleJournalSearch(t *testing.T) {
	search := searchWith(cashEntry("j-1", "2026-07-03", "125.50"))
	search.AlreadyMatchedJournalIDs = map[string]bool{"j-1": true}

	items := analyzeCandidates(t, search, cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CandidateSearchOutcome != CandidateNone || got.CounterpartJournalID != "" {
		t.Fatalf("expected NO_CANDIDATE once the only qualifying journal is already claimed, got %+v", got)
	}
}

// Excluding one claimed journal must not disqualify a genuinely unclaimed
// one that also happens to qualify — exclusion is per journal, not per line.
func TestCandidate_AlreadyMatchedJournal_LeavesUnclaimedCandidateUsable(t *testing.T) {
	search := searchWith(
		cashEntry("j-claimed", "2026-07-03", "125.50"), // would also qualify alone
	)
	search.EntriesByAccount[candCash] = append(search.EntriesByAccount[candCash], cashEntry("j-free", "2026-07-02", "125.50"))
	search.AlreadyMatchedJournalIDs = map[string]bool{"j-claimed": true}

	items := analyzeCandidates(t, search, cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CounterpartJournalID != "j-free" {
		t.Fatalf("expected the unclaimed journal j-free proposed, got %+v", got)
	}
}

// An already-claimed journal is removed from the pair-search pool entirely:
// it can never be proposed as half of a two-journal match either.
func TestCandidate_AlreadyMatchedJournal_ExcludedFromPairSearch(t *testing.T) {
	search := searchWith(
		cashEntry("j-1", "2026-07-02", "100.00"),
		cashEntry("j-2", "2026-07-03", "25.50"), // j-1+j-2 would sum to 125.50
	)
	search.AlreadyMatchedJournalIDs = map[string]bool{"j-2": true}

	items := analyzeCandidates(t, search, cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CandidateSearchOutcome != CandidateNone || len(got.CounterpartJournalIDs) != 0 {
		t.Fatalf("expected NO_CANDIDATE once one half of the only qualifying pair is already claimed, got %+v", got)
	}
}

// A nil AlreadyMatchedJournalIDs (the zero value, what every caller that
// predates A2b part 3 still passes) excludes nothing.
func TestCandidate_NilAlreadyMatchedJournalIDs_ExcludesNothing(t *testing.T) {
	search := searchWith(cashEntry("j-1", "2026-07-03", "125.50"))
	if search.AlreadyMatchedJournalIDs != nil {
		t.Fatal("test setup: expected a nil map")
	}
	items := analyzeCandidates(t, search, cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CounterpartJournalID != "j-1" {
		t.Fatalf("expected j-1 still proposed with no exclusion set, got %+v", got)
	}
}
