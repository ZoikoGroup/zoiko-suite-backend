package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func allMet() map[string]bool {
	m := map[string]bool{}
	for _, it := range ReadyChecklist {
		m[it.Code] = true
	}
	for _, it := range DoneChecklist {
		m[it.Code] = true
	}
	return m
}

func TestChecklists_AreTheSpecifiedSizes(t *testing.T) {
	assert.Len(t, ReadyChecklist, 10)
	assert.Len(t, DoneChecklist, 11)
	assert.True(t, ChecklistHasItem("READY", "DOR_10"))
	assert.False(t, ChecklistHasItem("READY", "DOD_01"))
	assert.Len(t, InitialPortfolio, 10)
	for _, e := range InitialPortfolio {
		assert.True(t, ValidFamilyRef(e.FamilyRef), e.FamilyRef)
	}
}

func TestTransitionBlockers_LaunchNeedsEverythingAndIndependence(t *testing.T) {
	ready := RolloutSnapshot{Status: RolloutReady, Owner: "owner", Met: allMet(), ApprovedExperts: 1, Experts: []string{"expert"}, LinkedPacks: 1, ReleasedLinked: 1}
	assert.Empty(t, TransitionBlockers(ready, RolloutLaunched, "launcher", ""))
	assert.NotEmpty(t, TransitionBlockers(ready, RolloutLaunched, "owner", ""))
	assert.NotEmpty(t, TransitionBlockers(ready, RolloutLaunched, "expert", ""))

	noExpert := ready
	noExpert.ApprovedExperts = 0
	assert.Contains(t, TransitionBlockers(noExpert, RolloutLaunched, "x", "")[0], "expert")

	noRelease := ready
	noRelease.ReleasedLinked = 0
	assert.Contains(t, TransitionBlockers(noRelease, RolloutLaunched, "x", "")[0], "RELEASED")

	retracted := ready
	retracted.Met = allMet()
	retracted.Met["DOD_05"] = false
	assert.Contains(t, TransitionBlockers(retracted, RolloutLaunched, "x", "")[0], "DOD_05")

	// Edges: nothing skips, and RETIRED is final.
	assert.Contains(t, TransitionBlockers(RolloutSnapshot{Status: RolloutPlanned}, RolloutLaunched, "x", "")[0], "cannot move")
	assert.Contains(t, TransitionBlockers(RolloutSnapshot{Status: RolloutRetired}, RolloutAuthoring, "x", "")[0], "cannot move")
	// Reasons and owner.
	assert.NotEmpty(t, TransitionBlockers(ready, RolloutRetired, "x", " "))
	assert.NotEmpty(t, TransitionBlockers(RolloutSnapshot{Status: RolloutPlanned, Owner: UnassignedOwner}, RolloutAuthoring, "x", ""))
	assert.Empty(t, TransitionBlockers(RolloutSnapshot{Status: RolloutPlanned, Owner: "someone"}, RolloutAuthoring, "x", ""))
}
