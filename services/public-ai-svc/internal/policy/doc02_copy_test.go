package policy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The assistant's fixed replies use Doc 02's approved visitor-facing copy
// (§12 templates T01–T30) word for word. The document is read at test time,
// so a reply that drifts from it fails here. Deliberate differences:
// T19 says "This assistant" for the uncleared working name; T24 leaves out
// the "[server-generated trace ID]" placeholder (the widget shows the
// reference); X01 live-platform data keeps the product owner's wording.
func TestRepliesUseDoc02Copy(t *testing.T) {
	matches, _ := filepath.Glob(filepath.Join("..", "..", "..", "..", "docs", "suivo", "source-text", "*Document_02*.md"))
	if len(matches) != 1 {
		t.Fatalf("Doc 02 source text not found: %v", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	copyRe := regexp.MustCompile(`(?m)^### (T\d\d) /[^\n]*\n\s*\nVISITOR-FACING COPY\s+(.+)$`)
	doc := map[string]string{}
	for _, m := range copyRe.FindAllStringSubmatch(strings.ReplaceAll(string(raw), "\r\n", "\n"), -1) {
		doc[m[1]] = norm(m[2])
	}
	if len(doc) != 30 {
		t.Fatalf("parsed %d templates from Doc 02, want 30", len(doc))
	}
	doc["T19"] = strings.Replace(doc["T19"], "Suivo covers", "This assistant covers", 1)
	doc["T24"] = strings.TrimSpace(strings.Replace(doc["T24"], " Reference: [server-generated trace ID].", "", 1))

	ours := map[string][]string{
		"T01": {TplWelcome.Text},
		"T05": {TplClarifyMarket.Text},
		"T07": {BoundaryTemplates["I09"].Text},
		"T11": {BoundaryTemplates["I14"].Text},
		"T16": {RouteTemplates["I23"].Text},
		"T17": {RouteTemplates["I22"].Text},
		"T18": {BoundaryTemplates["I25"].Text},
		"T19": {ExceptionTemplates[X07CrossProduct].Text},
		"T20": {ExceptionTemplates[X04RegulatedAdvice].Text},
		"T21": {ExceptionTemplates[X01PrivateAction].Text, ExceptionTemplates[X02ActionDenied].Text},
		"T22": {ExceptionTemplates[X03SensitiveInput].Text},
		"T23": {ExceptionTemplates[X05SecurityEscalate].Text},
		"T24": {TplUnavailable.Text},
		"T25": {TplOutOfScope.Text},
		"T26": {RouteTemplates["I24"].Text},
		"T30": {BoundaryTemplates["I27"].Text},
	}
	for id, texts := range ours {
		for _, txt := range texts {
			if norm(txt) != doc[id] {
				t.Errorf("%s drifted from Doc 02:\n doc: %s\n ours: %s", id, doc[id], norm(txt))
			}
		}
	}
}

func norm(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`, "—", "-", "–", "-").Replace(s)), " ")
}
