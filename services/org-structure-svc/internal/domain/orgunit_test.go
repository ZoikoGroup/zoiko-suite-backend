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
