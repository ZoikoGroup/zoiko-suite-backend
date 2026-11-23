package domain

// SPEC_DEVIATIONS A2b part 2: "one payment booked as two journals" — tried
// only when no single journal on the account matches the line at all. See
// candidates_test.go for the single-journal fixtures/helpers this reuses.

import (
	"strings"
	"testing"
)

func TestCandidate_TwoJournalsSumExactly_WithinTolerance_ProposesAutoMatch(t *testing.T) {
	// Neither journal alone equals 125.50; together they do.
	items := analyzeCandidates(t, searchWith(
		cashEntry("j-1", "2026-07-02", "100.00"),
		cashEntry("j-2", "2026-07-03", "25.50"),
	), cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.Recommendation != RecommendationAutoMatch || got.CandidateSearchOutcome != CandidateWithinDateTolerance ||
		got.HeuristicRule != HeuristicCandidateMultiJournalWithinDateTolerance {
		t.Fatalf("expected an AUTO_MATCH multi-journal proposal, got %+v", got)
	}
	if len(got.CounterpartJournalIDs) != 2 || got.CounterpartJournalID != "" {
		t.Fatalf("expected both journal ids in CounterpartJournalIDs and the scalar field empty, got %+v", got)
	}
	if got.AmountB != "125.5" {
		t.Fatalf("expected the combined amount 125.5, got %s", got.AmountB)
	}
	if !strings.Contains(got.ResolutionNotes, "j-1") || !strings.Contains(got.ResolutionNotes, "j-2") {
		t.Fatalf("rationale must name both journals: %s", got.ResolutionNotes)
	}
}

func TestCandidate_TwoJournalsSumExactly_BeyondTolerance_ProposesDateSkew(t *testing.T) {
	// The later journal (j-2, day 9) is beyond the 3-day tolerance but
	// within the 31-day horizon.
	items := analyzeCandidates(t, searchWith(
		cashEntry("j-1", "2026-07-02", "100.00"),
		cashEntry("j-2", "2026-07-10", "25.50"),
	), cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CandidateSearchOutcome != CandidateDateSkew || got.Recommendation != RecommendationManualReview ||
		got.HeuristicRule != HeuristicCandidateMultiJournalDateSkew {
		t.Fatalf("expected a DATE_SKEW multi-journal proposal, got %+v", got)
	}
	if !strings.Contains(got.ResolutionNotes, "9 day(s)") {
		t.Fatalf("rationale must state the worse day count (9), got: %s", got.ResolutionNotes)
	}
}

// A single matching journal is always preferred: the pair search never even
// runs when one journal alone equals the line.
func TestCandidate_SingleJournalPreferredOverPair(t *testing.T) {
	items := analyzeCandidates(t, searchWith(
		cashEntry("j-1", "2026-07-02", "125.50"), // alone equals the line
		cashEntry("j-2", "2026-07-02", "50.00"),
		cashEntry("j-3", "2026-07-02", "75.50"), // j-2+j-3 also sums to 125.50
	), cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CounterpartJournalID != "j-1" || len(got.CounterpartJournalIDs) != 0 {
		t.Fatalf("expected the single journal j-1 preferred over any pair, got %+v", got)
	}
	if got.HeuristicRule != HeuristicCandidateWithinDateTolerance {
		t.Fatalf("expected the single-journal rule, got %s", got.HeuristicRule)
	}
}

// More than one qualifying pair is AMBIGUOUS, same posture as more than one
// qualifying single journal.
func TestCandidate_MultipleQualifyingPairs_IsAmbiguous(t *testing.T) {
	items := analyzeCandidates(t, searchWith(
		cashEntry("j-1", "2026-07-02", "100.00"),
		cashEntry("j-2", "2026-07-02", "25.50"), // j-1+j-2 = 125.50
		cashEntry("j-3", "2026-07-02", "120.00"),
		cashEntry("j-4", "2026-07-02", "5.50"), // j-3+j-4 = 125.50
	), cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CandidateSearchOutcome != CandidateAmbiguous || got.Recommendation != RecommendationManualReview ||
		got.HeuristicRule != HeuristicCandidateMultiJournalAmbiguous {
		t.Fatalf("expected an ambiguous multi-journal result, got %+v", got)
	}
	if len(got.CounterpartJournalIDs) != 0 || got.CounterpartJournalID != "" {
		t.Fatalf("nothing is proposed when ambiguous, got %+v", got)
	}
}

// Cross-line exclusivity extends to pairs: a journal claimed by one line's
// pair cannot also be claimed by another line's single-journal match (or
// another line's pair) — both lines become AMBIGUOUS.
func TestCandidate_PairSharesJournalWithAnotherLinesSingleMatch_BothAmbiguous(t *testing.T) {
	items := analyzeCandidates(t, searchWith(
		cashEntry("j-shared", "2026-07-02", "25.50"),
		cashEntry("j-2", "2026-07-02", "100.00"), // with j-shared, sums to sl-1's 125.50
	),
		cashLine("sl-1", "125.50", "2026-07-01"), // needs the pair j-2+j-shared
		cashLine("sl-2", "25.50", "2026-07-01"),  // j-shared alone matches this one
	)
	for _, got := range items {
		if got.CandidateSearchOutcome != CandidateAmbiguous {
			t.Fatalf("expected both lines AMBIGUOUS over the shared journal, got %+v", got)
		}
	}
}

func TestCandidate_NoSingleOrPairMatches_IsNoCandidate(t *testing.T) {
	items := analyzeCandidates(t, searchWith(
		cashEntry("j-1", "2026-07-02", "10.00"),
		cashEntry("j-2", "2026-07-02", "20.00"),
	), cashLine("sl-1", "125.50", "2026-07-01"))
	got := items[0]
	if got.CandidateSearchOutcome != CandidateNone {
		t.Fatalf("expected NO_CANDIDATE after trying both single journals and pairs, got %+v", got)
	}
}
