package resolver

// Specification negative paths proved by the runtime resolver (rule R10). The
// full index is in progress.md section 4.

import "testing"

// JUR-NEG-07: a past return recalculated after a rate update selects the historical rule version.
func TestJURNEG07_HistoricalRecalculationUsesTheHistoricalVersion(t *testing.T) {
	TestResolve_PicksTheHighestVersionInForce_AndPinnedReplayReproducesHistory(t)
}

// JUR-NEG-24 (and PDC-02 NP-17): two regimes or packs activating for one transaction are
// resolved by declared precedence or blocked as ambiguous; never last-write-wins.
func TestJURNEG24_CrossPackAmbiguityIsBlocked(t *testing.T) {
	TestResolve_AmbiguityIsBlockedNotGuessed(t)
	TestCalculateDue_TwoPacksDefiningTheSameObligationIsBlocked(t)
}

func TestPDC02NP17_SameRankIncompatibleMandatesAreConflicted(t *testing.T) {
	TestResolve_AmbiguityIsBlockedNotGuessed(t)
}

// JUR-NEG-03/04 at runtime: a covering pack that is not trustworthy refuses the resolution.
func TestJURNEG04_RuntimeRefusesAnUntrustworthyPack(t *testing.T) {
	TestResolve_FailsClosedWhenACoveringPackIsNotTrustworthy(t)
}
