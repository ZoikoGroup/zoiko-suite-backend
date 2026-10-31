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
