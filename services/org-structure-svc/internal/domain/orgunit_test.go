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
