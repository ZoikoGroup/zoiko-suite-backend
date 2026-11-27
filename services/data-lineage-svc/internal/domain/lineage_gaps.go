package domain

import "strings"

// Lineage gaps (ZS-SVC-Z-001 RB-03 "Lineage gap: material output has no complete provenance - block
// certification; identify missing run/edge; do not infer lineage").
//
// SealManifest refuses at the FIRST gap it meets. A reader of a lineage graph needs ALL of them, so
// they can see exactly what is missing rather than fix one and meet the next. LineageGaps lists every
// gap using the same rules SealManifest enforces, and a test holds the two together: a graph is
// sealable if and only if it has no gaps.

const (
	GapNoInputLineage           = "NO_INPUT_LINEAGE"
	GapMissingTransformationVer = "MISSING_TRANSFORMATION_VERSION"
	GapMissingRunID             = "MISSING_RUN_ID"
	GapMissingBuildVersion      = "MISSING_BUILD_VERSION"
	GapMissingConfigDigest      = "MISSING_CONFIG_DIGEST"
	GapMissingSourceVersion     = "MISSING_SOURCE_VERSION"
	GapMissingDerivedVersion    = "MISSING_DERIVED_VERSION"
)

// LineageGap is one missing piece of evidence, naming the activity or edge it is missing from.
type LineageGap struct {
	Kind       string `json:"kind"`
	ActivityID string `json:"activity_id,omitempty"`
	EdgeID     string `json:"edge_id,omitempty"`
}

func blankStr(p *string) bool { return p == nil || strings.TrimSpace(*p) == "" }

// LineageGaps returns every gap in snap, in a stable order. An empty graph is itself a gap: it
// certifies nothing.
func LineageGaps(snap *LineageGraphSnapshot) []LineageGap {
	gaps := []LineageGap{}
	if len(snap.Edges) == 0 {
		gaps = append(gaps, LineageGap{Kind: GapNoInputLineage})
	}
	for _, a := range snap.Activities {
		if blankStr(a.TransformationVersion) {
			gaps = append(gaps, LineageGap{Kind: GapMissingTransformationVer, ActivityID: a.ActivityID})
		}
		if blankStr(a.RunID) {
			gaps = append(gaps, LineageGap{Kind: GapMissingRunID, ActivityID: a.ActivityID})
		}
		if blankStr(a.BuildVersion) {
			gaps = append(gaps, LineageGap{Kind: GapMissingBuildVersion, ActivityID: a.ActivityID})
		}
		if blankStr(a.ConfigDigest) {
			gaps = append(gaps, LineageGap{Kind: GapMissingConfigDigest, ActivityID: a.ActivityID})
		}
	}
	for _, e := range snap.Edges {
		if blankStr(e.SourceVersion) {
			gaps = append(gaps, LineageGap{Kind: GapMissingSourceVersion, EdgeID: e.EdgeID})
		}
		if blankStr(e.DerivedVersion) {
			gaps = append(gaps, LineageGap{Kind: GapMissingDerivedVersion, EdgeID: e.EdgeID})
		}
	}
	return gaps
}
