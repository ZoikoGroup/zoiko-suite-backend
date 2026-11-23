package acceptance

import (
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/knowledge"
)

// Doc 03 §06: a version that names the one it supersedes ends it when it
// starts, even if the old version was open-ended (no overlap conflict).
func TestSupersedesEndsTheOldVersion(t *testing.T) {
	dir := kbDir(t)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	writeSource(t, dir, "rules", map[string]any{"source_id": "src-rules"}, "## Rules\n\nOriginal wording about approval rules.")
	writeSource(t, dir, "rules2", map[string]any{"source_id": "src-rules", "source_version_id": "src-rules-v2", "supersedes": "src-rules-v1",
		"effective_from": start.Format(time.RFC3339)}, "## Rules\n\nRevised wording about approval rules.")
	m, states := ingestStates(t, dir)
	if states["src-rules"] != knowledge.StateStaged {
		t.Fatalf("states %v", states)
	}
	idx := knowledge.NewIndex(m)
	at := func(ts time.Time) string {
		for _, h := range idx.Search(knowledge.Query{Text: "wording approval rules", Locale: "en-US", Now: ts}, nil).Hits {
			if h.Chunk.SourceID == "src-rules" {
				return h.Chunk.SourceVersionID
			}
		}
		return ""
	}
	if v := at(start.Add(-time.Hour)); v != "src-rules-v1" {
		t.Fatalf("before: %q", v)
	}
	if v := at(start.Add(time.Hour)); v != "src-rules-v2" {
		t.Fatalf("after: %q", v)
	}
}

// Doc 03 §06: plan scope names registered plans; a revoked source states why.
func TestPlanRegistryAndRevocationReason(t *testing.T) {
	dir := kbDir(t)
	writeSource(t, dir, "unknownplan", map[string]any{"plan_codes": []string{"PLATINUM"}}, "## Plans\n\nA plan note.")
	writeSource(t, dir, "revoked", map[string]any{"approval_state": "REVOKED"}, "## Old\n\nWithdrawn wording.")
	_, states := ingestStates(t, dir)
	if states["src-unknownplan"] != knowledge.StateReviewUnmapped {
		t.Errorf("unknown plan code: %s", states["src-unknownplan"])
	}
	if states["src-revoked"] == knowledge.StateStaged {
		t.Errorf("a revoked source without a reason was staged")
	}
}
