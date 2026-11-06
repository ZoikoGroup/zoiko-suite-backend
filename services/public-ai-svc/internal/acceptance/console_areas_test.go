package acceptance

import (
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
)

// Console areas (My Work; Identity, Scope & Foundation) are answered from
// their public KB articles, not with the generic off-topic reply.
func TestConsoleAreasAreAnswered(t *testing.T) {
	x := publicKBServer(t)
	for q, want := range map[string]string{
		"use of Task":                             "kb-my-work",
		"what is Tasks & Approvals":               "kb-my-work",
		"what are Exceptions":                     "kb-my-work",
		"use of the Identity Context":             "kb-identity-context",
		"what is the Authentication & federation": "kb-identity-context",
		"what is Secret Vault":                    "kb-identity-foundation",
		"what is Gateway Authentication":          "kb-identity-foundation",
		"what is Roles & Access":                  "kb-identity-foundation",
		"what is Tenants & Entities":              "kb-identity-foundation",
	} {
		r := x.env.Pipeline.Answer(ctx, inputFor(q))
		if r.Status != contract.Answered {
			t.Errorf("%q: %s %s: %q", q, r.Status, r.Reason, r.Text)
			continue
		}
		cited := false
		for _, c := range r.Citations {
			cited = cited || strings.HasPrefix(c.SourceVersionID, want+"-")
		}
		if !cited || strings.Contains(r.Text, "I'm here to help with ZoikoSuite capabilities") {
			t.Errorf("%q: cites %v, want %s: %q", q, r.Citations, want, r.Text)
		}
	}
}

func inputFor(q string) orchestrator.Input {
	return orchestrator.Input{RequestID: "req_console", Message: q, Locale: "en-US"}
}
