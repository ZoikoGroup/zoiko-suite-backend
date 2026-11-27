package domain_test

import (
	"strings"
	"testing"

	"zoiko.io/data-lineage-svc/internal/domain"
)

func sp(s string) *string { return &s }

func completeSnap() *domain.LineageGraphSnapshot {
	return &domain.LineageGraphSnapshot{
		Activities: []domain.LineageActivity{{ActivityID: "act_1", TransformationVersion: sp("t:v1"), RunID: sp("run-1"), BuildVersion: sp("b1"), ConfigDigest: sp("d")}},
		Edges:      []domain.LineageEdge{{EdgeID: "edge_1", ActivityID: "act_1", SourceVersion: sp("s@1"), DerivedVersion: sp("o@1")}},
	}
}

func kinds(gaps []domain.LineageGap) string {
	var k []string
	for _, g := range gaps {
		k = append(k, g.Kind+":"+g.ActivityID+g.EdgeID)
	}
	return strings.Join(k, ",")
}

func TestLineageGaps_CompleteGraphHasNone(t *testing.T) {
	if g := domain.LineageGaps(completeSnap()); len(g) != 0 {
		t.Fatalf("a complete graph reported gaps: %s", kinds(g))
	}
}

func TestLineageGaps_EmptyGraphIsAGap(t *testing.T) {
	if got := kinds(domain.LineageGaps(&domain.LineageGraphSnapshot{})); got != domain.GapNoInputLineage+":" {
		t.Fatalf("gaps = %q", got)
	}
}

func TestLineageGaps_EachMissingPieceIsNamedAgainstItsActivityOrEdge(t *testing.T) {
	cases := map[string]struct {
		mutate func(*domain.LineageGraphSnapshot)
		want   string
	}{
		"transformation version": {func(s *domain.LineageGraphSnapshot) { s.Activities[0].TransformationVersion = nil }, domain.GapMissingTransformationVer + ":act_1"},
		"run id":                 {func(s *domain.LineageGraphSnapshot) { s.Activities[0].RunID = nil }, domain.GapMissingRunID + ":act_1"},
		"build version":          {func(s *domain.LineageGraphSnapshot) { s.Activities[0].BuildVersion = sp("  ") }, domain.GapMissingBuildVersion + ":act_1"},
		"config digest":          {func(s *domain.LineageGraphSnapshot) { s.Activities[0].ConfigDigest = nil }, domain.GapMissingConfigDigest + ":act_1"},
		"source version":         {func(s *domain.LineageGraphSnapshot) { s.Edges[0].SourceVersion = nil }, domain.GapMissingSourceVersion + ":edge_1"},
		"derived version":        {func(s *domain.LineageGraphSnapshot) { s.Edges[0].DerivedVersion = sp("") }, domain.GapMissingDerivedVersion + ":edge_1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := completeSnap()
			tc.mutate(s)
			if got := kinds(domain.LineageGaps(s)); got != tc.want {
				t.Fatalf("gaps = %q, want %q", got, tc.want)
			}
		})
	}
}

// Unlike sealing, which stops at the first gap, the list is complete.
func TestLineageGaps_ListsEveryGapNotJustTheFirst(t *testing.T) {
	s := &domain.LineageGraphSnapshot{
		Activities: []domain.LineageActivity{{ActivityID: "act_1"}, {ActivityID: "act_2", RunID: sp("r"), BuildVersion: sp("b"), ConfigDigest: sp("d"), TransformationVersion: sp("t")}},
		Edges:      []domain.LineageEdge{{EdgeID: "edge_1"}, {EdgeID: "edge_2", SourceVersion: sp("s"), DerivedVersion: sp("d")}},
	}
	if n := len(domain.LineageGaps(s)); n != 6 {
		t.Fatalf("%d gaps, want act_1's four and edge_1's two: %s", n, kinds(domain.LineageGaps(s)))
	}
}
