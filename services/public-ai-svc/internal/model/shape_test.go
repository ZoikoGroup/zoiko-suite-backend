package model

import (
	"strings"
	"testing"
)

func TestShapeLeavesOrdinarySentencesAlone(t *testing.T) {
	for _, s := range []string{
		"Legal and Contracts governs commercial agreements, corporate actions, board resolutions and legal obligations.",
		"Delegated Authority is a register of who may act for whom: time-bound, entity-scoped grants of authority.",
		"Pick one: Finance (A, B) or Workforce (C).", // fewer than three groups
	} {
		if got, focused := shape(s, queryWords("anything about finance")); got != s || focused {
			t.Errorf("changed %q to %q", s, got)
		}
	}
}

func TestShapeListAndFocus(t *testing.T) {
	s := "Core modules are grouped into domains: Finance (Ledger, Payables), Workforce (Payroll and Human Resources), Tax, Legal and Compliance (Tax Management), and Commercial (Procurement)."
	list, focused := shape(s, queryWords("What modules are there?"))
	if focused || !strings.HasPrefix(list, "Core modules are grouped into domains:\n- **Finance:** Ledger, Payables\n") ||
		!strings.Contains(list, "\n- **Tax, Legal and Compliance:** Tax Management") || !strings.HasSuffix(list, "\n- **Commercial:** Procurement") {
		t.Fatalf("list: %q", list)
	}
	got, focused := shape(s, queryWords("Do you have HR?"))
	if !focused || got != "Core modules include Workforce (Payroll and Human Resources)." {
		t.Fatalf("focus: %q", got)
	}
}
