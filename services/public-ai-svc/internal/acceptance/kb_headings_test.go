package acceptance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
)

var headingRe = regexp.MustCompile(`(?m)^## (.+)$`)

// headingExceptions are KB headings that are deliberately not answered when
// asked as "what is <heading>", with the reason.
var headingExceptions = map[string]string{
	"How it works":                     "too generic to name one article",
	"What you can do":                  "every word is a stopword, so it names nothing",
	"Capabilities, not certifications": "certification questions go to the claims path (I12), which needs an approved claim",
	"Sign-in security":                 "sign-in questions are routed to account support (I22)",
	"Create a data residency policy":   "data residency is answered only from approved claims (I14)",
}

// Every section of every public KB article answers a question that names
// it, so a visitor who asks about any page or section of the platform is
// not given the generic off-topic reply.
func TestEveryKBHeadingIsAnswered(t *testing.T) {
	x := publicKBServer(t)
	files, err := filepath.Glob("../../data/public-kb/sources/*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("no KB sources: %v", err)
	}
	asked := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range headingRe.FindAllStringSubmatch(string(b), -1) {
			h := strings.TrimSpace(m[1])
			if _, skip := headingExceptions[h]; skip {
				continue
			}
			for _, q := range []string{"what is " + strings.TrimSuffix(h, "?"), "tell me about " + strings.TrimSuffix(h, "?")} {
				asked++
				r := x.env.Pipeline.Answer(ctx, inputFor(q))
				if r.Status != contract.Answered || strings.Contains(r.Text, "I'm here to help with ZoikoSuite capabilities") {
					t.Errorf("%s: %q: %s %s %s", filepath.Base(f), q, r.Status, r.Reason, r.IntentID)
				}
			}
		}
	}
	if asked < 300 {
		t.Fatalf("only %d heading questions asked", asked)
	}
}

// Opening the knowledge base to every section must not open it to
// everything: questions about other subjects are still out of scope.
func TestOffTopicStillRefused(t *testing.T) {
	x := publicKBServer(t)
	for _, q := range []string{
		"what is the weather today", "write me a poem about money", "who won the football match",
		"tell me a joke", "how do I cook pasta", "what is your favourite movie", "recommend a good book",
		"how tall is mount everest", "approve my invoice", "pay this invoice for me",
	} {
		r := x.env.Pipeline.Answer(ctx, inputFor(q))
		if r.Status == contract.Answered {
			t.Errorf("%q answered from %v: %q", q, r.Citations, r.Text)
		}
	}
}
