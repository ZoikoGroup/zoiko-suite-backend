package acceptance

import (
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/policy"
)

// Answer quality on the public KB: each question is answered from the
// article it is about (never one that only shares a word), with the
// approved caveat intact, or gets the Doc 02 reply for its intent when no
// approved article exists. Built from a 47-question sweep (2026-10-28).
func TestAnswerQuality(t *testing.T) {
	k := newKB(t)
	cases := []struct {
		q, source string   // source "" means a template reply (no citation)
		has       []string // phrases the answer must contain
	}{
		{"What can ZoikoSuite do?", "kb-overview", []string{"governance-first", "depend on market, plan and deployment status"}},
		{"Does it support multiple entities?", "kb-multi-entity", []string{"multi-entity", "depends on the approved plan"}},
		{"How does ZoikoSuite handle approvals?", "kb-governance", []string{"policy-aware approvals", "configured role"}},
		{"Who can approve on behalf of someone else?", "kb-governance", []string{"delegated authority"}},
		{"How do I migrate from spreadsheets?", "kb-migration", []string{"scoped review is needed before any timeline"}},
		{"How does the compliance module work?", "kb-compliance", []string{"evidential", "reviewed rather than reconstructed"}},
		{"What is Delegated Authority?", "kb-delegated-authority", []string{"time-bound", "authority that was in force"}},
		{"How is my data secured?", "kb-security-trust", []string{"mTLS", "never stated by this assistant without a verified claim"}},
		// Doc 02 T13: workforce answers keep the jurisdiction caveat.
		{"Does it support HR?", "kb-workforce", []string{"Human Resources", "depends on the jurisdiction, configuration and released coverage"}},
		{"Does it handle payroll?", "kb-workforce", []string{"Payroll", "depends on the jurisdiction, configuration and released coverage"}},
		{"What evidence does ZoikoSuite keep?", "kb-evidence", []string{"auditors"}},
		{"Do you support VAT?", "kb-tax", []string{"VAT"}},
		{"Can it manage board resolutions?", "kb-legal-contracts", []string{"board resolutions", "approval-bound execution"}},
		{"How do purchase requests work?", "kb-purchase-requests", []string{"before any purchase order"}},
		{"How are vendor invoices approved?", "kb-commercial-ops", []string{"supplier invoices"}},
		{"Does it support eSignature?", "kb-integrations", []string{"eSignature", "approved integration matrix"}}, // Doc 02 T08
		{"Where is data stored?", "", []string{policy.BoundaryTemplates["I14"].Text}},
		{"What is on the roadmap?", "", []string{policy.BoundaryTemplates["I25"].Text}},
		{"Is the site accessible?", "", []string{policy.BoundaryTemplates["I27"].Text}},
		{"Does ZoikoSuite use AI?", "", []string{policy.BoundaryTemplates["I30"].Text}},
	}
	for _, c := range cases {
		r := k.ask(c.q)
		switch {
		case c.source != "" && (r.Status != contract.Answered || !cited(r, c.source)):
			t.Errorf("%q: want an answer from %s, got %s %s %+v %q", c.q, c.source, r.Status, r.Reason, r.Citations, r.Text)
			continue
		case c.source == "" && len(r.Citations) != 0:
			t.Errorf("%q: a boundary reply cites nothing, got %+v", c.q, r.Citations)
		}
		for _, h := range c.has {
			if !strings.Contains(r.Text, h) {
				t.Errorf("%q: answer must contain %q: %q", c.q, h, r.Text)
			}
		}
		for _, cit := range r.Citations {
			if c.source != "" && !strings.HasPrefix(cit.SourceVersionID, c.source+"-") {
				t.Errorf("%q: cites an unrelated article %s", c.q, cit.SourceVersionID)
			}
		}
	}
}

// Shaping: a general question gets the module list as bullets; a specific
// one gets only the group it asked about, with the caveat, and nothing else.
func TestAnswerShaping(t *testing.T) {
	k := newKB(t)
	r := k.ask("What modules does ZoikoSuite include?")
	for _, want := range []string{"grouped into domains:\n- **Finance:** ", "\n- **Workforce:** Payroll and Human Resources", "\n- **Commercial:** ", "\nThe modules available to you depend on"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("modules list must contain %q: %q", want, r.Text)
		}
	}
	focused := []struct{ q, group, notGroup string }{
		{"What is treasury and cash position?", "Finance (Accounting and General Ledger", "Workforce ("},
	}
	for _, c := range focused {
		r := k.ask(c.q)
		if !cited(r, "kb-core-modules") || !strings.Contains(r.Text, "core modules include "+c.group) ||
			strings.Contains(r.Text, c.notGroup) || strings.Contains(r.Text, "\n") || !strings.Contains(r.Text, "depend on market, plan and deployment status") {
			t.Errorf("%q: want only %s with the caveat, got %q", c.q, c.group, r.Text)
		}
		if strings.Contains(r.Text, "Governance Platform") {
			t.Errorf("%q: a focused answer adds no other section: %q", c.q, r.Text)
		}
	}
}

// Doc 02 G12 and G14 on the public KB: a capability question is answered
// with its caveat (T12, T14), while a request to act on the visitor's own
// records is still refused (T21).
func TestDoc02CapabilityVersusAction(t *testing.T) {
	k := newKB(t)
	r := k.ask("Do you file taxes in Germany?")
	// G12: "regional evidence gate; no advice": Germany has no approved
	// coverage, so the coverage gate answers; it is never a refusal.
	if r.IntentID != "I17" || r.Status != contract.Abstain || !strings.Contains(r.Text, "confirm coverage for your country") {
		t.Errorf("G12 capability question: %s %s %q", r.IntentID, r.Status, r.Text)
	}
	r = k.ask("Are your audit logs legally admissible in court?")
	if r.IntentID != "I16" || !strings.Contains(r.Text, "legal admissibility require separate evidence and cannot be assumed") || strings.Contains(strings.ToLower(r.Text), "are admissible") {
		t.Errorf("G14: %s %s %q", r.IntentID, r.Status, r.Text)
	}
	for _, q := range []string{"Can you file my taxes in Germany?", "File our VAT return", "Do you pay my invoices for me?"} {
		if r := k.ask(q); r.Status != contract.RefusedAction {
			t.Errorf("%q must still be refused, got %s %s", q, r.IntentID, r.Status)
		}
	}
}
