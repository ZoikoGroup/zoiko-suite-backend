package domain

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func day(n int) time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n) }
func dayp(n int) *time.Time {
	t := day(n)
	return &t
}

var relSeq int

// rel is an ACTIVE ownership edge parent -> child holding pct, from day from to
// day to (nil to = open-ended).
func rel(parent, child string, pct float64, from int, to *time.Time) OwnershipRelationship {
	relSeq++
	p := pct
	return OwnershipRelationship{
		RelationshipID: fmt.Sprintf("r%d", relSeq), ParentLegalEntityID: parent, ChildLegalEntityID: child,
		RelationshipKind: RelationshipOwnership, OwnershipPct: &p, ValidFrom: day(from), ValidTo: to,
		Status: OwnershipActive,
	}
}

func TestCheckOwnershipGraph_Cycles(t *testing.T) {
	ab := rel("A", "B", 50, 0, nil)
	bc := rel("B", "C", 50, 0, nil)

	t.Run("direct reversal", func(t *testing.T) {
		err := CheckOwnershipGraph([]OwnershipRelationship{ab}, rel("B", "A", 10, 0, nil), "")
		if !errors.Is(err, ErrOwnershipCycle) {
			t.Fatalf("want cycle, got %v", err)
		}
	})
	t.Run("loop through two hops", func(t *testing.T) {
		err := CheckOwnershipGraph([]OwnershipRelationship{ab, bc}, rel("C", "A", 10, 0, nil), "")
		if !errors.Is(err, ErrOwnershipCycle) {
			t.Fatalf("want cycle, got %v", err)
		}
	})
	t.Run("a diamond is not a cycle", func(t *testing.T) {
		// A->B, A->C, B->D already; C->D joins the diamond but loops nowhere.
		active := []OwnershipRelationship{ab, rel("A", "C", 50, 0, nil), rel("B", "D", 50, 0, nil)}
		if err := CheckOwnershipGraph(active, rel("C", "D", 50, 0, nil), ""); err != nil {
			t.Fatalf("diamond refused: %v", err)
		}
	})
	t.Run("the chain extended downwards is fine", func(t *testing.T) {
		if err := CheckOwnershipGraph([]OwnershipRelationship{ab, bc}, rel("C", "D", 10, 0, nil), ""); err != nil {
			t.Fatalf("refused: %v", err)
		}
	})
	t.Run("a loop that never coexists is not a cycle", func(t *testing.T) {
		// A owned B until day 100; B may own A only from day 200.
		past := rel("A", "B", 50, 0, dayp(100))
		if err := CheckOwnershipGraph([]OwnershipRelationship{past}, rel("B", "A", 50, 200, nil), ""); err != nil {
			t.Fatalf("refused a loop whose edges never coexist: %v", err)
		}
		// ...but the same loop overlapping by a day is one.
		if err := CheckOwnershipGraph([]OwnershipRelationship{past}, rel("B", "A", 50, 99, nil), ""); !errors.Is(err, ErrOwnershipCycle) {
			t.Fatalf("want cycle for the overlapping loop, got %v", err)
		}
	})
	t.Run("all edges of a long loop must coexist", func(t *testing.T) {
		// A->B [0,50) and B->C [60,..): A->B and B->C never coexist, so C->A closes nothing.
		active := []OwnershipRelationship{rel("A", "B", 50, 0, dayp(50)), rel("B", "C", 50, 60, nil)}
		if err := CheckOwnershipGraph(active, rel("C", "A", 50, 0, nil), ""); err != nil {
			t.Fatalf("refused: %v", err)
		}
	})
	t.Run("control counts as an edge too", func(t *testing.T) {
		ctl := ControlBoard
		ctlEdge := OwnershipRelationship{RelationshipID: "c1", ParentLegalEntityID: "B", ChildLegalEntityID: "A",
			RelationshipKind: RelationshipControl, ControlType: &ctl, ValidFrom: day(0), Status: OwnershipActive}
		if err := CheckOwnershipGraph([]OwnershipRelationship{ab}, ctlEdge, ""); !errors.Is(err, ErrOwnershipCycle) {
			t.Fatalf("want cycle through a control edge, got %v", err)
		}
	})
	t.Run("a replaced relationship is not part of the loop", func(t *testing.T) {
		// Superseding A->B with B->A would be a cycle only if the old edge still counted.
		if err := CheckOwnershipGraph([]OwnershipRelationship{ab}, rel("B", "A", 10, 0, nil), ab.RelationshipID); err != nil {
			t.Fatalf("the excluded edge was still counted: %v", err)
		}
	})
}

func TestCheckOwnershipGraph_Overlap(t *testing.T) {
	held := rel("A", "B", 40, 0, dayp(100))

	if err := CheckOwnershipGraph([]OwnershipRelationship{held}, rel("A", "B", 40, 50, nil), ""); !errors.Is(err, ErrOwnershipOverlap) {
		t.Fatalf("want overlap, got %v", err)
	}
	// Back to back is not an overlap: [0,100) then [100,..).
	if err := CheckOwnershipGraph([]OwnershipRelationship{held}, rel("A", "B", 40, 100, nil), ""); err != nil {
		t.Fatalf("adjacent intervals refused: %v", err)
	}
	// A different kind between the same entities is a different fact.
	ctl := ControlVoting
	control := OwnershipRelationship{RelationshipID: "x", ParentLegalEntityID: "A", ChildLegalEntityID: "B",
		RelationshipKind: RelationshipControl, ControlType: &ctl, ValidFrom: day(0)}
	if err := CheckOwnershipGraph([]OwnershipRelationship{held}, control, ""); err != nil {
		t.Fatalf("control beside ownership refused: %v", err)
	}
	// Superseding replaces it, so the old one does not overlap its successor.
	if err := CheckOwnershipGraph([]OwnershipRelationship{held}, rel("A", "B", 55, 50, nil), held.RelationshipID); err != nil {
		t.Fatalf("supersession refused as overlap: %v", err)
	}
}

func TestCheckOwnershipGraph_TotalCannotExceed100(t *testing.T) {
	a := rel("A", "T", 60, 0, nil)

	if err := CheckOwnershipGraph([]OwnershipRelationship{a}, rel("B", "T", 40, 0, nil), ""); err != nil {
		t.Fatalf("exactly 100%% refused: %v", err)
	}
	if err := CheckOwnershipGraph([]OwnershipRelationship{a}, rel("B", "T", 40.01, 0, nil), ""); !errors.Is(err, ErrOwnershipExceeds) {
		t.Fatalf("want exceeds, got %v", err)
	}
	// A holding that has ended does not count against a later one.
	ended := rel("A", "T", 90, 0, dayp(10))
	if err := CheckOwnershipGraph([]OwnershipRelationship{ended}, rel("B", "T", 90, 10, nil), ""); err != nil {
		t.Fatalf("an ended holding counted: %v", err)
	}
	// A holding that STARTS inside the candidate's interval does count.
	later := rel("A", "T", 60, 50, nil)
	if err := CheckOwnershipGraph([]OwnershipRelationship{later}, rel("B", "T", 60, 0, nil), ""); !errors.Is(err, ErrOwnershipExceeds) {
		t.Fatalf("want exceeds for a holding that starts mid-interval, got %v", err)
	}
	// Voting is summed on its own.
	v := 70.0
	w := 40.0
	votingA := rel("A", "T", 10, 0, nil)
	votingA.VotingPct = &v
	votingB := rel("B", "T", 10, 0, nil)
	votingB.VotingPct = &w
	if err := CheckOwnershipGraph([]OwnershipRelationship{votingA}, votingB, ""); !errors.Is(err, ErrOwnershipExceeds) {
		t.Fatalf("want voting total exceeded, got %v", err)
	}
	// Control relationships carry no equity and are not summed.
	ctl := ControlContractual
	control := OwnershipRelationship{RelationshipID: "c", ParentLegalEntityID: "C", ChildLegalEntityID: "T",
		RelationshipKind: RelationshipControl, ControlType: &ctl, ValidFrom: day(0)}
	if err := CheckOwnershipGraph([]OwnershipRelationship{a}, control, ""); err != nil {
		t.Fatalf("control relationship summed as equity: %v", err)
	}
}
