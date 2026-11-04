package knowledge

import (
	"strings"
	"testing"
	"time"
)

// Doc 03 §14 step 4: a chunk scoped to other plans is never evidence for the
// plan the visitor named; unscoped chunks serve every plan.
func TestPlanScopeFilter(t *testing.T) {
	chunk := func(id string, plans []string) Chunk {
		return Chunk{ChunkID: id + "#001", SourceID: id, SourceVersionID: id + "-v1", DisplayTitle: "Plan limits",
			PlainText: "Plan limits allow entities and users per workspace.", Classification: Classification, Audience: Audience,
			MarketCodes: []string{MarketAll}, Locale: "en-US", PlanCodes: plans,
			ValidFrom: t0.Add(-time.Hour), ValidUntil: t0.Add(time.Hour), ReleaseID: "kb-p"}
	}
	filler := func(id, text string) Chunk {
		c := chunk(id, nil)
		c.DisplayTitle, c.PlainText = "Other", text
		return c
	}
	ix := NewIndex(Manifest{ReleaseID: "kb-p", Chunks: []Chunk{
		chunk("growth", []string{"GROWTH"}), chunk("starter", []string{"STARTER"}), chunk("any", nil), chunk("all", []string{"ALL"}),
		filler("f1", "Payroll runs monthly."), filler("f2", "Invoices are approved by finance."), filler("f3", "Contracts hold clauses."),
		filler("f4", "Audit trails record actions."), filler("f5", "Tax rules vary by country."), filler("f6", "Support hours are published."),
		filler("f7", "Reports export to spreadsheets."), filler("f8", "Workflows route approvals."),
	}})
	cases := []struct {
		plan string
		want []string
	}{
		{"GROWTH", []string{"all", "any", "growth"}},
		{"STARTER", []string{"all", "any", "starter"}},
		{"", []string{"all", "any", "growth", "starter"}},
	}
	for _, c := range cases {
		res := ix.Search(Query{Text: "plan limits entities", Now: t0, MaxChunks: 10, Plan: c.plan}, nil)
		got := map[string]bool{}
		for _, h := range res.Hits {
			got[h.Chunk.SourceID] = true
		}
		if len(got) != len(c.want) {
			t.Errorf("plan %q: got %v want %v", c.plan, got, c.want)
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("plan %q: missing %s (got %v)", c.plan, w, got)
			}
		}
	}
}

// Doc 03 §6, §10: two-person control counts distinct approvers, and scope
// labels must be well-formed (ISO 3166-1 alpha-2 or ALL; BCP-47 locale).
func TestIngestCheckApproversAndScope(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(y int) *time.Time { v := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC); return &v }
	base := Source{SourceID: "s", SourceVersionID: "s-v1", SourceType: "WEB_PAGE", OriginURI: "https://example.invalid/x",
		URLKey: "x", Title: "Overview", ProductID: ProductID, Audience: Audience, Classification: Classification, PIIState: PIIClear,
		AuthorityTier: "P1", SubjectType: "OVERVIEW", OwnerID: "owner", ApproverIDs: []string{"a", "b"},
		MarketCodes: []string{"ALL"}, Locale: "en-US", ApprovalState: "APPROVED", ApprovalTS: at(2026),
		EffectiveFrom: at(2026), ExpiresAt: at(2099), ReviewDueAt: at(2027), License: "OWNED_PUBLIC"}
	cfg := IngestConfig{AllowedHosts: []string{"example.invalid"}}
	cases := []struct {
		name string
		mut  func(*Source)
		want string // "" = staged; otherwise a reason fragment
	}{
		{"valid", func(*Source) {}, ""},
		{"same approver twice", func(s *Source) { s.ApproverIDs = []string{"a", "a"} }, "two approvers"},
		{"same approver, case", func(s *Source) { s.ApproverIDs = []string{"a", " A "} }, "two approvers"},
		{"market lower case", func(s *Source) { s.MarketCodes = []string{"us"} }, "market_codes"},
		{"market alpha-3", func(s *Source) { s.MarketCodes = []string{"USA"} }, "market_codes"},
		{"market ok", func(s *Source) { s.MarketCodes = []string{"US", "GB"} }, ""},
		{"locale underscore", func(s *Source) { s.Locale = "en_US" }, "BCP-47"},
		{"locale junk", func(s *Source) { s.Locale = "english" }, "BCP-47"},
		{"locale language only", func(s *Source) { s.Locale = "en" }, ""},
		{"locale script region", func(s *Source) { s.Locale = "zh-Hant-TW" }, ""},
	}
	for _, c := range cases {
		s := base
		s.ApproverIDs = append([]string(nil), base.ApproverIDs...)
		c.mut(&s)
		state, reason := check(cfg, map[string]bool{}, s, "A plain public overview of the product.", now, IngestOptions{})
		switch {
		case c.want == "" && state != StateStaged:
			t.Errorf("%s: %s %s", c.name, state, reason)
		case c.want != "" && !strings.Contains(reason, c.want):
			t.Errorf("%s: got %s %q, want %q", c.name, state, reason, c.want)
		}
	}
}
