package acceptance

import (
	"os"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
)

// TestPlatformQuestionBank asks the assistant the visitor questions in
// testdata/platform-questions.tsv, area by area (Tax, Governance, Finance…),
// and requires each to be answered from one of the articles listed for it.
func TestPlatformQuestionBank(t *testing.T) {
	b, err := os.ReadFile("../../testdata/platform-questions.tsv")
	if err != nil {
		t.Fatal(err)
	}
	x := publicKBServer(t)
	asked := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("bad line %q", line)
		}
		area, want, q := f[0], f[1], f[2]
		asked++
		r := x.env.Pipeline.Answer(ctx, inputFor(q))
		if strings.Contains(r.Text, "§") {
			t.Errorf("[%s] %q: heading marker in answer: %q", area, q, r.Text)
		}
		if r.Status != contract.Answered || strings.Contains(r.Text, "I'm here to help with ZoikoSuite capabilities") {
			t.Errorf("[%s] %q: %s %s %s: %.120q", area, q, r.Status, r.Reason, r.IntentID, r.Text)
			continue
		}
		ok := false
		var got []string
		for _, c := range r.Citations {
			got = append(got, c.SourceVersionID)
			for _, w := range strings.Split(want, "|") {
				ok = ok || strings.HasPrefix(c.SourceVersionID, w+"-v")
			}
		}
		if !ok {
			t.Errorf("[%s] %q: cites %v, want %s: %.120q", area, q, got, want, r.Text)
		}
	}
	if asked < 100 {
		t.Fatalf("only %d questions", asked)
	}
}
