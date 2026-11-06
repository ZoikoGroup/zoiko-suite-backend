package knowledge

import (
	"testing"
	"time"
)

func chunkFor(id, text string, highImpact bool, due *time.Time, aud string) Chunk {
	return Chunk{ChunkID: id + "#001", SourceID: id, SourceVersionID: id + "-v1", URLKey: id, PlainText: text, DisplayTitle: "Pricing",
		Classification: Classification, Audience: aud, MarketCodes: []string{MarketAll}, Locale: "en-US", ReleaseID: "kb-r",
		ValidFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ValidUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		HighImpact: highImpact, ReviewDueAt: due}
}

// filler gives BM25 a realistic corpus (terms in every document score ~0).
func filler() []Chunk {
	var out []Chunk
	for i, t := range []string{"governance approvals delegated authority", "multi entity consolidation intercompany", "payroll workforce leave",
		"contracts clauses obligations", "tax filings indirect", "security encryption access", "integrations api connectors", "evidence audit trail"} {
		out = append(out, chunkFor("f"+string(rune('a'+i)), t, false, nil, Audience))
	}
	return out
}

// Doc 03 §12/§26: a high-impact chunk past its review deadline is not
// served; ordinary content keeps its validity window.
func TestOverdueHighImpactReviewStopsServing(t *testing.T) {
	due := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	idx := NewIndex(Manifest{ReleaseID: "kb-r", Chunks: append(filler(),
		chunkFor("price", "Growth plan pricing per user per month", true, &due, Audience),
		chunkFor("guide", "How pricing pages explain plans per user per month", false, &due, Audience),
	)})
	q := func(now time.Time) map[string]bool {
		got := map[string]bool{}
		for _, h := range idx.Search(Query{Text: "pricing per user per month", Locale: "en-US", Now: now}, nil).Hits {
			got[h.Chunk.SourceID] = true
		}
		return got
	}
	if got := q(due.Add(-time.Hour)); !got["price"] || !got["guide"] {
		t.Fatalf("before the deadline: %v", got)
	}
	// After it the overdue chunk is never served; as the best match is out of
	// date, the query reports stale evidence instead of a weaker answer.
	after := idx.Search(Query{Text: "pricing per user per month", Locale: "en-US", Now: due.Add(time.Hour)}, nil)
	for _, h := range after.Hits {
		if h.Chunk.SourceID == "price" {
			t.Fatal("an overdue high-impact chunk was served")
		}
	}
	if after.Answerability == Answerable && len(after.Hits) == 0 {
		t.Fatalf("inconsistent result: %+v", after)
	}
}

// Doc 05 §18: chunks for another audience are filtered before ranking and
// never counted as candidates.
func TestDeniedChunksAreNotRanked(t *testing.T) {
	idx := NewIndex(Manifest{ReleaseID: "kb-r", Chunks: append(filler(),
		chunkFor("staff", "Internal pricing playbook per user per month", false, nil, "STAFF"),
	)})
	res := idx.Search(Query{Text: "pricing per user per month", Locale: "en-US", Now: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}, nil)
	if len(res.Hits) != 0 || res.CandidatesBeforeACL != 0 {
		t.Fatalf("denied chunk was ranked: %+v", res)
	}
}
