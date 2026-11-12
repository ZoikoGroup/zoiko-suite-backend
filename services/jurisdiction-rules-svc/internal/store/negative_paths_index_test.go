package store_test

// REF-01 and PDC-02 negative paths proved against the store (rule R10; real
// Postgres, TEST_DATABASE_URL). The full index is in progress.md section 4.

import "testing"

// REF-01 #22: an unknown country or subdivision code is refused, never guessed.
func TestREF01NP22_UnknownCodeIsRefusedNotGuessed(t *testing.T) {
	TestAlias_NeverGuesses(t)
	TestAlias_NotMappedAtThatTimeIsDifferentFromUnknown(t)
}

// REF-01 #23: a retired code keeps its history; the current alias maps without rewriting it.
func TestREF01NP23_RetiredCodeKeepsItsHistory(t *testing.T) {
	TestAlias_ARetiredJurisdictionIsRetiredNotUnknown_AndStillResolvesForItsPast(t)
	TestRetire_RecordsEvidenceAndStaysReadableAsOfTheirPast(t)
}

// PDC-02 NP-14: a historical period resolves the hierarchy as it was, not today's.
func TestPDC02NP14_HistoricalPeriodUsesTheAsOfHierarchy(t *testing.T) {
	TestHierarchy_IsExactAsOfADateAcrossAMoveAndARetirement(t)
}

// PDC-02 NP-20: a late, retroactive change is answered as known at the time; earlier answers are reproducible.
func TestPDC02NP20_KnownAtReplaysWhatWasKnown(t *testing.T) {
	TestPgStore_ResolveRulePack_KnownAtReplaysWhatWasKnown(t)
}
