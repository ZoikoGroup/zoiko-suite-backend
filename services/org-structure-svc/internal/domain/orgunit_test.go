package domain

import "testing"

func TestIsValidUnitType(t *testing.T) {
	for _, ok := range []string{UnitDepartment, UnitBusinessUnit, UnitFunction, UnitDivision, UnitTeam} {
		if !IsValidUnitType(ok) {
			t.Errorf("%q should be a valid unit type", ok)
		}
	}
	for _, bad := range []string{"", "department", "COST_CENTER", "TEAM "} {
		if IsValidUnitType(bad) {
			t.Errorf("%q should not be a valid unit type", bad)
		}
	}
}

func TestOrgUnitTransition(t *testing.T) {
	cases := []struct {
		from, cmd, want string
		ok              bool
	}{
		{UnitStatusDraft, UnitCommandActivate, UnitStatusActive, true},
		{UnitStatusActive, UnitCommandDeactivate, UnitStatusInactive, true},
		{UnitStatusDraft, UnitCommandDeactivate, "", false},
		{UnitStatusActive, UnitCommandActivate, "", false},
		{UnitStatusInactive, UnitCommandActivate, "", false},
		{UnitStatusInactive, UnitCommandDeactivate, "", false},
		{UnitStatusDraft, "RESURRECT", "", false},
	}
	for _, c := range cases {
		got, err := OrgUnitTransition(c.from, c.cmd)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("%s+%s: want %s, got %q err=%v", c.from, c.cmd, c.want, got, err)
		}
		if !c.ok && err != ErrInvalidTransition {
			t.Errorf("%s+%s: want ErrInvalidTransition, got %v", c.from, c.cmd, err)
		}
	}
}

func TestReachesUnit(t *testing.T) {
	// root <- a <- b <- c, and an unrelated x
	parents := map[string]string{"a": "root", "b": "a", "c": "b"}
	parentOf := func(id string) *string {
		if p, ok := parents[id]; ok {
			return &p
		}
		return nil
	}
	cases := []struct {
		name          string
		start, target string
		want          bool
	}{
		{"unit under its own descendant is a loop", "c", "a", true},
		{"unit under its direct child is a loop", "b", "a", true},
		{"unit under itself is a loop", "a", "a", true},
		{"unit under an ancestor is fine", "root", "c", false},
		{"unrelated branch is fine", "x", "a", false},
	}
	for _, c := range cases {
		if got := ReachesUnit(c.start, c.target, parentOf); got != c.want {
			t.Errorf("%s: ReachesUnit(%s,%s)=%v, want %v", c.name, c.start, c.target, got, c.want)
		}
	}

	// Bad data that already contains a cycle must terminate.
	cyc := map[string]string{"p": "q", "q": "p"}
	if ReachesUnit("p", "zzz", func(id string) *string {
		if v, ok := cyc[id]; ok {
			return &v
		}
		return nil
	}) {
		t.Error("an unrelated target must not be reached in a cycle")
	}
}

func unitNode(id, code string, parent *string) OrgUnit {
	return OrgUnit{UnitID: id, Code: code, ParentUnitID: parent}
}

func TestBuildOrgUnitTree(t *testing.T) {
	root, mid := "root", "mid"
	units := []OrgUnit{
		unitNode("leaf-b", "B", &mid),
		unitNode("mid", "M", &root),
		unitNode("leaf-a", "A", &mid),
		unitNode("root", "R", nil),
		unitNode("other", "O", nil),
	}
	forest, err := BuildOrgUnitTree(units)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(forest) != 2 || forest[0].Code != "O" || forest[1].Code != "R" {
		t.Fatalf("roots must be ordered by code, got %d: %v", len(forest), forest)
	}
	r := forest[1]
	if len(r.Children) != 1 || r.Children[0].Code != "M" {
		t.Fatalf("root has one child M, got %+v", r.Children)
	}
	m := r.Children[0]
	if len(m.Children) != 2 || m.Children[0].Code != "A" || m.Children[1].Code != "B" {
		t.Fatalf("siblings must be ordered by code, got %+v", m.Children)
	}
	if forest[0].Children == nil || m.Children[0].Children == nil {
		t.Fatal("leaves must carry an empty children list, not null")
	}
}

// A unit whose parent is not in the snapshot (for example a retired parent
// that was left out) is returned as a root instead of vanishing.
func TestBuildOrgUnitTree_MissingParentBecomesRoot(t *testing.T) {
	gone := "retired"
	forest, err := BuildOrgUnitTree([]OrgUnit{unitNode("kid", "K", &gone)})
	if err != nil || len(forest) != 1 || forest[0].UnitID != "kid" {
		t.Fatalf("want kid as a root, got %v err=%v", forest, err)
	}
}

func TestBuildOrgUnitTree_EmptyAndCycle(t *testing.T) {
	forest, err := BuildOrgUnitTree(nil)
	if err != nil || forest == nil || len(forest) != 0 {
		t.Fatalf("empty input must give an empty, non-nil forest, got %v err=%v", forest, err)
	}

	a, b := "a", "b"
	if _, err := BuildOrgUnitTree([]OrgUnit{unitNode("a", "A", &b), unitNode("b", "B", &a)}); err != ErrHierarchyLoop {
		t.Fatalf("a cycle must be reported, got %v", err)
	}
}
