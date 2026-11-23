package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func row(id, code string, depth int, parent string) HierarchyRow {
	r := HierarchyRow{HierarchyNode: HierarchyNode{JurisdictionID: id, JurisdictionCode: code, Depth: depth}}
	if parent != "" {
		r.ParentJurisdictionID = &parent
	}
	return r
}

func shape(n *HierarchyNode) string {
	if len(n.Children) == 0 {
		return n.JurisdictionCode
	}
	parts := make([]string, 0, len(n.Children))
	for _, c := range n.Children {
		parts = append(parts, shape(c))
	}
	return n.JurisdictionCode + "(" + strings.Join(parts, ",") + ")"
}

func TestBuildHierarchy_NestsChildrenUnderTheirParents(t *testing.T) {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	h := BuildHierarchy(at, []HierarchyRow{
		row("1", "GB", 0, ""),
		row("2", "GB-ENG", 1, "1"), row("3", "GB-SCT", 1, "1"),
		row("4", "GB-LND", 2, "2"),
	})
	if got := shape(h.Root); got != "GB(GB-ENG(GB-LND),GB-SCT)" {
		t.Errorf("shape = %s", got)
	}
	if h.Count != 4 || !h.AsOf.Equal(at) {
		t.Errorf("count = %d, as_of = %v", h.Count, h.AsOf)
	}
}

// Children are never null: a client iterating a leaf's children must not have
// to special-case a missing array.
func TestBuildHierarchy_ALeafHasAnEmptyChildList(t *testing.T) {
	h := BuildHierarchy(time.Now(), []HierarchyRow{row("1", "GB", 0, "")})
	if h.Root.Children == nil || len(h.Root.Children) != 0 {
		t.Errorf("children = %#v, want an empty non-nil list", h.Root.Children)
	}
}

// A row whose parent the walk did not keep cannot be placed, so it is left out
// rather than attached somewhere it does not belong.
func TestBuildHierarchy_DropsARowWhoseParentIsNotInTheSet(t *testing.T) {
	h := BuildHierarchy(time.Now(), []HierarchyRow{row("1", "GB", 0, ""), row("9", "ORPHAN", 1, "missing")})
	if got := shape(h.Root); got != "GB" {
		t.Errorf("shape = %s, want the orphan left out", got)
	}
}

func TestNotEffectiveError_SaysWhichSideOfTheIntervalItFellOn(t *testing.T) {
	from := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	early := &NotEffectiveError{JurisdictionID: "j", AsOf: from.Add(-time.Hour), EffectiveFrom: from}
	if !strings.Contains(early.Error(), "not yet in force") || !strings.Contains(early.Error(), "2030-01-01") {
		t.Errorf("early = %q", early.Error())
	}
	late := &NotEffectiveError{JurisdictionID: "j", AsOf: to.Add(time.Hour), EffectiveFrom: from.AddDate(-20, 0, 0), EffectiveTo: &to}
	if !strings.Contains(late.Error(), "no longer in force") || !strings.Contains(late.Error(), "2025-06-01") {
		t.Errorf("late = %q", late.Error())
	}
	if !errors.Is(early, ErrJurisdictionNotEffective) || !errors.Is(late, ErrJurisdictionNotEffective) {
		t.Error("a NotEffectiveError must match ErrJurisdictionNotEffective")
	}
	if errors.Is(ErrJurisdictionNotFound, ErrJurisdictionNotEffective) {
		t.Error("not-found and not-effective must stay different answers")
	}
}
