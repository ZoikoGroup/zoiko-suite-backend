package resolve

import (
	"testing"
	"time"

	"zoiko.io/policy-svc/internal/domain"
)

// ResolveRanked ranks governing versions of different chains by a caller-given
// rank. The default Resolve is the same function ranked by scope specificity,
// which the existing tests prove; these prove the ranking can be replaced and
// how ties and losers are named.

func rv(id, policy string, activated time.Time) *domain.ApplicablePolicyVersion {
	tenant := "t"
	a := activated
	v := &domain.ApplicablePolicyVersion{}
	v.PolicyVersionID, v.PolicyID, v.TenantID = id, policy, &tenant
	v.ScopeType = domain.ScopeTypeTenant
	v.VersionStatus, v.VersionNumber, v.ActivatedAt = domain.StatusActive, 1, &a
	v.EffectiveFrom = activated.Add(-time.Hour)
	return v
}

func rankBy(m map[string][]int) Rank {
	return func(v *domain.ApplicablePolicyVersion) []int { return m[v.PolicyVersionID] }
}

func TestResolveRanked_TheHigherRankGoverns(t *testing.T) {
	now := time.Now().UTC()
	a, b := rv("a", "pa", now.Add(-time.Hour)), rv("b", "pb", now.Add(-time.Hour))
	res := ResolveRanked([]*domain.ApplicablePolicyVersion{a, b}, now, now, rankBy(map[string][]int{"a": {2, 0}, "b": {1, 999}}))
	if res.Status != Resolved || res.Selected != a {
		t.Fatalf("status %s selected %v, want a: the first element decides before the second is looked at", res.Status, res.Selected)
	}
	if got := reasonOf(res, "b"); got != ReasonLessSpecific {
		t.Errorf("b's reason = %q, want %q", got, ReasonLessSpecific)
	}
}

func TestResolveRanked_PrecedenceBreaksATieOnTheFirstElement(t *testing.T) {
	now := time.Now().UTC()
	a, b := rv("a", "pa", now.Add(-time.Hour)), rv("b", "pb", now.Add(-time.Hour))
	res := ResolveRanked([]*domain.ApplicablePolicyVersion{a, b}, now, now, rankBy(map[string][]int{"a": {2, 1}, "b": {2, 5}}))
	if res.Status != Resolved || res.Selected != b {
		t.Fatalf("status %s selected %v, want b", res.Status, res.Selected)
	}
	if got := reasonOf(res, "a"); got != ReasonLowerPrecedence {
		t.Errorf("a's reason = %q, want %q", got, ReasonLowerPrecedence)
	}
	if got := reasonOf(res, "b"); got != ReasonSelected {
		t.Errorf("b's reason = %q", got)
	}
}

func TestResolveRanked_EqualInEveryElementIsAConflictNeverAGuess(t *testing.T) {
	now := time.Now().UTC()
	a, b, c := rv("a", "pa", now.Add(-time.Hour)), rv("b", "pb", now.Add(-2*time.Hour)), rv("c", "pc", now.Add(-time.Hour))
	res := ResolveRanked([]*domain.ApplicablePolicyVersion{a, b, c}, now, now, rankBy(map[string][]int{"a": {2, 5}, "b": {2, 5}, "c": {1, 9}}))
	if res.Status != Conflicted || res.Selected != nil || len(res.Conflicting) != 2 {
		t.Fatalf("status %s selected %v conflicting %d, want a conflict between a and b", res.Status, res.Selected, len(res.Conflicting))
	}
	// However they were recorded: the later activation does not win a tie.
	if reasonOf(res, "a") != ReasonConflicting || reasonOf(res, "b") != ReasonConflicting || reasonOf(res, "c") != ReasonLessSpecific {
		t.Errorf("reasons: a %q b %q c %q", reasonOf(res, "a"), reasonOf(res, "b"), reasonOf(res, "c"))
	}
	// The conflicting set is stable, whatever order the candidates came in.
	res2 := ResolveRanked([]*domain.ApplicablePolicyVersion{c, b, a}, now, now, rankBy(map[string][]int{"a": {2, 5}, "b": {2, 5}, "c": {1, 9}}))
	if res2.Conflicting[0].PolicyVersionID != res.Conflicting[0].PolicyVersionID {
		t.Error("the order of the conflicting versions depends on input order")
	}
}

// Versions of one chain supersede each other before any ranking happens: the
// rank only orders chains.
func TestResolveRanked_ChainsStillSupersedeBeforeRanking(t *testing.T) {
	now := time.Now().UTC()
	old, neu := rv("old", "pa", now.Add(-48*time.Hour)), rv("new", "pa", now.Add(-24*time.Hour))
	neu.VersionNumber = 2
	other := rv("other", "pb", now.Add(-time.Hour))
	res := ResolveRanked([]*domain.ApplicablePolicyVersion{old, neu, other}, now, now,
		rankBy(map[string][]int{"old": {9, 9}, "new": {1, 0}, "other": {1, 0}}))
	if res.Status != Conflicted {
		t.Fatalf("status %s: old's high rank must not survive its supersession, leaving new and other tied", res.Status)
	}
	if reasonOf(res, "old") != ReasonSupersededInChain {
		t.Errorf("old's reason = %q", reasonOf(res, "old"))
	}
}

func TestResolveRanked_NoCandidatesIsNotApplicable(t *testing.T) {
	now := time.Now().UTC()
	if res := ResolveRanked(nil, now, now, rankBy(nil)); res.Status != NotApplicable || res.Selected != nil {
		t.Errorf("status %s", res.Status)
	}
}
