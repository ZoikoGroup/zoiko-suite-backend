package domain

import (
	"math"
	"testing"
)

// edge is an ownership relationship in the given status.
func edge(id, parent, child string, pct float64, from int, to *int, status OwnershipStatus) OwnershipRelationship {
	p := pct
	r := OwnershipRelationship{
		RelationshipID: id, ParentLegalEntityID: parent, ChildLegalEntityID: child,
		RelationshipKind: RelationshipOwnership, OwnershipPct: &p, ValidFrom: day(from),
		Status: status, EvidenceRef: "ev-" + id,
	}
	if to != nil {
		r.ValidTo = dayp(*to)
	}
	return r
}

func intp(n int) *int { return &n }

func TestInForceAt(t *testing.T) {
	cases := []struct {
		name string
		r    OwnershipRelationship
		at   int
		want bool
	}{
		{"active, inside", edge("a", "A", "B", 50, 0, nil, OwnershipActive), 10, true},
		{"before it starts", edge("a", "A", "B", 50, 10, nil, OwnershipActive), 5, false},
		{"valid_to is exclusive", edge("a", "A", "B", 50, 0, intp(10), OwnershipActive), 10, false},
		{"the day before valid_to", edge("a", "A", "B", 50, 0, intp(10), OwnershipActive), 9, true},
		{"superseded still shows in its own past", edge("a", "A", "B", 50, 0, intp(100), OwnershipSuperseded), 50, true},
		{"superseded is gone after its end", edge("a", "A", "B", 50, 0, intp(100), OwnershipSuperseded), 100, false},
		{"ended still shows in its own past", edge("a", "A", "B", 50, 0, intp(30), OwnershipEnded), 10, true},
		{"proposed never took effect", edge("a", "A", "B", 50, 0, nil, OwnershipProposed), 10, false},
		{"rejected never took effect", edge("a", "A", "B", 50, 0, nil, OwnershipRejected), 10, false},
	}
	for _, c := range cases {
		if got := InForceAt(c.r, day(c.at)); got != c.want {
			t.Errorf("%s: InForceAt = %v, want %v", c.name, got, c.want)
		}
	}
}

// The same rows answer differently on different dates, which is what makes the
// graph reconstructable.
func TestGraphAsOf_ReconstructsThePast(t *testing.T) {
	all := []OwnershipRelationship{
		edge("old", "A", "B", 60, 0, intp(100), OwnershipSuperseded),
		edge("new", "A", "B", 80, 100, nil, OwnershipActive),
		edge("c", "B", "C", 100, 50, intp(150), OwnershipEnded),
		edge("pending", "A", "D", 10, 0, nil, OwnershipProposed),
	}

	at := func(d int) map[string]float64 {
		got := map[string]float64{}
		for _, r := range GraphAsOf(all, day(d), "").Relationships {
			got[r.RelationshipID] = *r.OwnershipPct
		}
		return got
	}

	if g := at(10); len(g) != 1 || g["old"] != 60 {
		t.Errorf("day 10: want only the original 60%%, got %v", g)
	}
	if g := at(60); len(g) != 2 || g["old"] != 60 || g["c"] != 100 {
		t.Errorf("day 60: want old and c, got %v", g)
	}
	if g := at(120); len(g) != 2 || g["new"] != 80 || g["c"] != 100 {
		t.Errorf("day 120: want the replacement 80%% and c, got %v", g)
	}
	if g := at(200); len(g) != 1 || g["new"] != 80 {
		t.Errorf("day 200: want only new once c ended, got %v", g)
	}
	if g := at(-5); len(g) != 0 {
		t.Errorf("before anything: want nothing, got %v", g)
	}
	// An empty answer is an empty list, never null.
	if v := GraphAsOf(all, day(-5), ""); v.Relationships == nil || v.Entities == nil {
		t.Error("an empty graph must render [] not null")
	}
}

func TestGraphAsOf_RootLimitsToWhatItOwns(t *testing.T) {
	all := []OwnershipRelationship{
		edge("ab", "A", "B", 60, 0, nil, OwnershipActive),
		edge("bc", "B", "C", 60, 0, nil, OwnershipActive),
		edge("xy", "X", "Y", 60, 0, nil, OwnershipActive),
	}
	v := GraphAsOf(all, day(1), "B")
	if len(v.Relationships) != 1 || v.Relationships[0].RelationshipID != "bc" {
		t.Fatalf("below B: want only bc, got %+v", v.Relationships)
	}
	if got := v.Entities; len(got) != 2 || got[0] != "B" || got[1] != "C" {
		t.Errorf("entities = %v", got)
	}
	// A root with nothing below it is a graph of that one entity.
	if v := GraphAsOf(all, day(1), "C"); len(v.Relationships) != 0 || len(v.Entities) != 1 {
		t.Errorf("leaf root: %+v", v)
	}
}

func TestGraphAsOf_IsDeterministicWhateverTheInputOrder(t *testing.T) {
	a := []OwnershipRelationship{
		edge("1", "A", "B", 10, 0, nil, OwnershipActive),
		edge("2", "A", "C", 10, 0, nil, OwnershipActive),
		edge("3", "B", "D", 10, 0, nil, OwnershipActive),
	}
	b := []OwnershipRelationship{a[2], a[0], a[1]}
	va, vb := GraphAsOf(a, day(1), ""), GraphAsOf(b, day(1), "")
	for i := range va.Relationships {
		if va.Relationships[i].RelationshipID != vb.Relationships[i].RelationshipID {
			t.Fatalf("order differs at %d: %v vs %v", i, va.Relationships, vb.Relationships)
		}
	}
}

func TestParentsAsOf(t *testing.T) {
	all := []OwnershipRelationship{
		edge("p1", "P1", "T", 30, 0, nil, OwnershipActive),
		edge("p2", "P2", "T", 70, 0, intp(50), OwnershipEnded),
		edge("other", "P1", "U", 30, 0, nil, OwnershipActive),
	}
	if got := ParentsAsOf(all, day(10), "T"); len(got) != 2 {
		t.Errorf("day 10: want 2 parents, got %d", len(got))
	}
	if got := ParentsAsOf(all, day(60), "T"); len(got) != 1 || got[0].ParentLegalEntityID != "P1" {
		t.Errorf("day 60: want only P1, got %+v", got)
	}
	if got := ParentsAsOf(all, day(10), "P1"); got == nil || len(got) != 0 {
		t.Errorf("a top entity has an empty (non-nil) parent list, got %#v", got)
	}
}

func TestGroupAsOf(t *testing.T) {
	all := []OwnershipRelationship{
		edge("ab", "A", "B", 60, 0, nil, OwnershipActive),
		edge("ac", "A", "C", 60, 0, nil, OwnershipActive),
		edge("db", "D", "B", 30, 0, nil, OwnershipActive), // a second owner of B
		edge("xy", "X", "Y", 60, 0, nil, OwnershipActive), // a different group
	}
	g := GroupAsOf(all, day(1), "C")
	if want := []string{"A", "B", "C", "D"}; !same(g.Members, want) {
		t.Errorf("members = %v, want %v (reached from C through A, then B and D)", g.Members, want)
	}
	if want := []string{"A", "D"}; !same(g.UltimateParents, want) {
		t.Errorf("ultimate parents = %v, want %v", g.UltimateParents, want)
	}
	if len(g.Relationships) != 3 {
		t.Errorf("relationships = %d, want 3 (xy is another group)", len(g.Relationships))
	}

	// A different date, a different group: AB has not started.
	later := append([]OwnershipRelationship{}, all...)
	later[0] = edge("ab", "A", "B", 60, 40, nil, OwnershipActive)
	if g := GroupAsOf(later, day(1), "B"); !same(g.Members, []string{"B", "D"}) {
		t.Errorf("before A->B starts, B's group = %v", g.Members)
	}

	// Nothing in force: a group of one, its own ultimate parent.
	solo := GroupAsOf(all, day(-1), "A")
	if !same(solo.Members, []string{"A"}) || !same(solo.UltimateParents, []string{"A"}) {
		t.Errorf("solo = %+v", solo)
	}
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestExplainControlChain_MultipliesEquityAndSumsPaths(t *testing.T) {
	// P owns 80% of M1 and 50% of M2; M1 and M2 each own 50% of T.
	// P's indirect holding in T: 0.8*0.5 + 0.5*0.5 = 65%.
	all := []OwnershipRelationship{
		edge("pm1", "P", "M1", 80, 0, nil, OwnershipActive),
		edge("pm2", "P", "M2", 50, 0, nil, OwnershipActive),
		edge("m1t", "M1", "T", 50, 0, nil, OwnershipActive),
		edge("m2t", "M2", "T", 50, 0, nil, OwnershipActive),
	}
	v := ExplainControlChain(all, day(1), "T")
	if len(v.Chains) != 1 || v.Chains[0].UltimateParentLegalEntityID != "P" {
		t.Fatalf("chains = %+v", v.Chains)
	}
	c := v.Chains[0]
	if len(c.Paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(c.Paths))
	}
	if !near(c.EffectiveOwnershipPct, 65) {
		t.Errorf("effective ownership = %v, want 65", c.EffectiveOwnershipPct)
	}
	// Each path is listed top-down, from the ultimate parent to the entity.
	for _, p := range c.Paths {
		if p.Links[0].ParentLegalEntityID != "P" || p.Links[len(p.Links)-1].ChildLegalEntityID != "T" {
			t.Errorf("path not top-down: %+v", p.Links)
		}
	}
	// 50% is not a majority, so no path controls, though P holds 65% overall.
	if c.Controls {
		t.Error("P holds 65% through two 50% links; no single chain is a majority all the way down")
	}
}

func TestExplainControlChain_MajorityChainControls(t *testing.T) {
	all := []OwnershipRelationship{
		edge("ab", "A", "B", 51, 0, nil, OwnershipActive),
		edge("bc", "B", "C", 70, 0, nil, OwnershipActive),
	}
	c := ExplainControlChain(all, day(1), "C").Chains[0]
	if !c.Controls {
		t.Error("51% then 70%: majority at every link, A controls C")
	}
	if !near(c.EffectiveOwnershipPct, 35.7) {
		t.Errorf("effective = %v, want 35.7", c.EffectiveOwnershipPct)
	}
}

func TestExplainControlChain_VotingPctDecidesControlNotEquity(t *testing.T) {
	voting := 60.0
	l := edge("ab", "A", "B", 20, 0, nil, OwnershipActive) // 20% of the equity ...
	l.VotingPct = &voting                                  // ... but 60% of the votes
	c := ExplainControlChain([]OwnershipRelationship{l}, day(1), "B").Chains[0]
	if !c.Controls || !near(c.EffectiveOwnershipPct, 20) {
		t.Errorf("controls=%v effective=%v; want control with 20%% equity", c.Controls, c.EffectiveOwnershipPct)
	}
}

func TestExplainControlChain_ControlLinkCarriesNoEquity(t *testing.T) {
	board := ControlBoard
	ctl := OwnershipRelationship{
		RelationshipID: "ctl", ParentLegalEntityID: "A", ChildLegalEntityID: "B",
		RelationshipKind: RelationshipControl, ControlType: &board, ValidFrom: day(0),
		Status: OwnershipActive, EvidenceRef: "board-minutes",
	}
	c := ExplainControlChain([]OwnershipRelationship{ctl}, day(1), "B").Chains[0]
	if !c.Controls {
		t.Error("a control relationship controls")
	}
	if c.Paths[0].EffectiveOwnershipPct != nil || c.EffectiveOwnershipPct != 0 {
		t.Errorf("a control link has no equity, got %v / %v", c.Paths[0].EffectiveOwnershipPct, c.EffectiveOwnershipPct)
	}
	if c.Paths[0].Links[0].EvidenceRef != "board-minutes" {
		t.Error("the chain must carry each link's evidence")
	}
}

func TestExplainControlChain_AsOfAndNoParents(t *testing.T) {
	all := []OwnershipRelationship{
		edge("old", "A", "B", 60, 0, intp(100), OwnershipSuperseded),
		edge("new", "Z", "B", 90, 100, nil, OwnershipActive),
	}
	before := ExplainControlChain(all, day(50), "B")
	after := ExplainControlChain(all, day(150), "B")
	if before.Chains[0].UltimateParentLegalEntityID != "A" || after.Chains[0].UltimateParentLegalEntityID != "Z" {
		t.Errorf("control changed hands: before=%v after=%v", before.Chains, after.Chains)
	}
	if top := ExplainControlChain(all, day(50), "A"); top.Chains == nil || len(top.Chains) != 0 {
		t.Errorf("an entity nobody owns has an empty (non-nil) chain list, got %#v", top.Chains)
	}
}
