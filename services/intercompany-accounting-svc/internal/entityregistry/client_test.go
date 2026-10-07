package entityregistry

import (
	"testing"
	"time"
)

func TestSharesOpenGroupRelationship_DirectParentChild_True(t *testing.T) {
	now := time.Now().UTC()
	a := []Hierarchy{{ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-a", EffectiveFrom: now.AddDate(-1, 0, 0)}}
	var b []Hierarchy
	if !SharesOpenGroupRelationship(a, b, "entity-a", "parent-1", now) {
		t.Fatal("expected a direct open parent/child relationship to be recognized")
	}
}

func TestSharesOpenGroupRelationship_SharedImmediateParent_True(t *testing.T) {
	now := time.Now().UTC()
	a := []Hierarchy{{ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-a", EffectiveFrom: now.AddDate(-1, 0, 0)}}
	b := []Hierarchy{{ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-b", EffectiveFrom: now.AddDate(-1, 0, 0)}}
	if !SharesOpenGroupRelationship(a, b, "entity-a", "entity-b", now) {
		t.Fatal("expected two siblings under the same open parent to be recognized as sharing a group relationship")
	}
}

func TestSharesOpenGroupRelationship_NoRelationship_False(t *testing.T) {
	now := time.Now().UTC()
	a := []Hierarchy{{ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-a", EffectiveFrom: now.AddDate(-1, 0, 0)}}
	b := []Hierarchy{{ParentLegalEntityID: "parent-2", ChildLegalEntityID: "entity-b", EffectiveFrom: now.AddDate(-1, 0, 0)}}
	if SharesOpenGroupRelationship(a, b, "entity-a", "entity-b", now) {
		t.Fatal("expected entities under different parents to NOT share a group relationship")
	}
}

// TestSharesOpenGroupRelationship_ClosedMidPeriod_False is the spec's own
// negative path, "Entity loses group relationship mid-period": a
// relationship that was open when the intercompany pair was created but
// has since been end-dated (EffectiveTo set, on or before asOf) must no
// longer count as open.
func TestSharesOpenGroupRelationship_ClosedMidPeriod_False(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.AddDate(0, 0, -1) // closed yesterday
	a := []Hierarchy{{
		ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-a",
		EffectiveFrom: now.AddDate(-1, 0, 0), EffectiveTo: &closedAt,
	}}
	b := []Hierarchy{{
		ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-b",
		EffectiveFrom: now.AddDate(-1, 0, 0),
	}}
	if SharesOpenGroupRelationship(a, b, "entity-a", "entity-b", now) {
		t.Fatal("expected a relationship closed before asOf to NOT count as open")
	}
}

func TestSharesOpenGroupRelationship_ClosedInFuture_StillOpenNow(t *testing.T) {
	now := time.Now().UTC()
	closesLater := now.AddDate(0, 1, 0) // scheduled to close next month
	a := []Hierarchy{{
		ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-a",
		EffectiveFrom: now.AddDate(-1, 0, 0), EffectiveTo: &closesLater,
	}}
	b := []Hierarchy{{
		ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-b",
		EffectiveFrom: now.AddDate(-1, 0, 0),
	}}
	if !SharesOpenGroupRelationship(a, b, "entity-a", "entity-b", now) {
		t.Fatal("expected a relationship that closes in the future to still count as open now")
	}
}

func TestSharesOpenGroupRelationship_NotYetEffective_False(t *testing.T) {
	now := time.Now().UTC()
	a := []Hierarchy{{ParentLegalEntityID: "parent-1", ChildLegalEntityID: "entity-a", EffectiveFrom: now.AddDate(0, 1, 0)}}
	var b []Hierarchy
	if SharesOpenGroupRelationship(a, b, "entity-a", "parent-1", now) {
		t.Fatal("expected a relationship not yet effective to NOT count as open")
	}
}
