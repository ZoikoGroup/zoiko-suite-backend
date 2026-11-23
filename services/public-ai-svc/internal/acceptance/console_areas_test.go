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
		"how to Register a jurisdiction":          "kb-jurisdictions-rules",
		"can I delete a jurisdiction":             "kb-jurisdictions-rules",
		"how does support access work":            "kb-identity-context",
		"what is Notifications":                   "kb-identity-foundation",
		"what is Assigned Actions":                "kb-my-work",
		"what is Jurisdictions & Rules":           "kb-jurisdictions-rules",
		"how do I resolve a rule pack":            "kb-jurisdictions-rules",
		"what is the Governance Log":              "kb-governance-log-policies",
		"how to change an approval limit policy":  "kb-governance-log-policies|kb-delegated-authority",
		"how do policy versions work":             "kb-governance-log-policies",
		"what are Obligations":                    "kb-obligations",
		"how to raise an obligation":              "kb-obligations",
		"what is Authorization":                   "kb-authorization-workflows",
		"what is segregation of duties":           "kb-authorization-workflows|kb-security-trust",
		"what is Workflows & Approvals":           "kb-authorization-workflows|kb-governance",
		"what is the Executive Dashboard":         "kb-console-overview",
		"what is Domain Health":                   "kb-console-overview",
		// Section titles from the console pages (screenshots, 2026-11-06).
		"use of the Decision log":                "kb-governance-log-policies",
		"What is in force right now":             "kb-governance-log-policies",
		"use of the create rule":                 "kb-governance-log-policies",
		"Set the limit":                          "kb-governance-log-policies",
		"how do I bring a limit into force":      "kb-governance-log-policies",
		"how do I move an obligation's status":   "kb-obligations",
		"what is a filing requirement":           "kb-obligations",
		"how is a permission check decided":      "kb-authorization-workflows",
		"how do I start a workflow":              "kb-authorization-workflows",
		"how do I decide the current stage":      "kb-authorization-workflows",
		"what are workflow definitions":          "kb-authorization-workflows",
		"what is the elevated access lifecycle":  "kb-authorization-workflows",
		"what does NO_REQUIREMENTS_DEFINED mean": "kb-evidence-requirements",
		"use of the Requirement catalog":         "kb-evidence-requirements",
		"how do I withdraw a requirement":        "kb-evidence-requirements",
		"how do I record a journal":              "kb-finance-journals",
		"how do I ingest a statement line":       "kb-finance-journals",
		"what is the Reconciliation register":    "kb-finance-journals",
	} {
		r := x.env.Pipeline.Answer(ctx, inputFor(q))
		if r.Status != contract.Answered {
			t.Errorf("%q: %s %s: %q", q, r.Status, r.Reason, r.Text)
			continue
		}
		cited := false
		for _, c := range r.Citations {
			for _, w := range strings.Split(want, "|") {
				cited = cited || strings.HasPrefix(c.SourceVersionID, w+"-")
			}
		}
		if !cited || strings.Contains(r.Text, "I'm here to help with ZoikoSuite capabilities") || strings.Contains(r.Text, "SYNTHETIC") {
			t.Errorf("%q: cites %v, want %s: %q", q, r.Citations, want, r.Text)
		}
	}
}

func inputFor(q string) orchestrator.Input {
	return orchestrator.Input{RequestID: "req_console", Message: q, Locale: "en-US"}
}

// A jurisdiction question with a country is still market availability.
func TestJurisdictionWithCountryStaysMarketQuestion(t *testing.T) {
	x := publicKBServer(t)
	r := x.env.Pipeline.Answer(ctx, inputFor("Do you support the UK jurisdiction?"))
	if r.IntentID != "I04" {
		t.Fatalf("intent %s: %q", r.IntentID, r.Text)
	}
}
