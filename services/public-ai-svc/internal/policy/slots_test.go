package policy

import (
	"strings"
	"testing"
)

// Doc 02 §09 optional slots: enums only, never from page or IP.
func TestContextSlots(t *testing.T) {
	for _, c := range []struct {
		msg, field, want string
	}{
		{"We are an accounting firm, can we manage clients?", "persona", "accountant"},
		{"I'm the CFO, what does consolidation look like?", "persona", "finance"},
		{"Can someone show me approvals and audit evidence?", "workstream", "governance"},
		{"Does it handle intercompany eliminations?", "workstream", "multi_entity"},
		{"I can't log in to my account", "support_topic", "access"},
		{"Do you support payroll in Germany?", "jurisdiction", "DE"},
		{"Book a demo", "handoff_intent", "demo"},
	} {
		s := Classify(c.msg).Slots
		got := map[string]string{"persona": s.Persona, "workstream": s.Workstream, "support_topic": s.SupportTopic,
			"jurisdiction": s.Jurisdiction, "handoff_intent": s.HandoffIntent}[c.field]
		if got != c.want {
			t.Errorf("%q: %s = %q, want %q (slots %+v)", c.msg, c.field, got, c.want, s)
		}
	}
	// A country on a non-regulated question is a market, not a jurisdiction.
	if s := Classify("What plans are available in Germany?").Slots; s.Jurisdiction != "" {
		t.Errorf("jurisdiction set for a plan question: %+v", s)
	}
}

func TestDemoReply(t *testing.T) {
	tpl, ask := DemoReply("Book a demo", "")
	if !ask || tpl.Text != TplDemoQualify.Text || len(DemoAreaChips) != 4 {
		t.Fatalf("no area: want the optional question, got %q ask=%v", tpl.Text, ask)
	}
	tpl, ask = DemoReply("Can someone show me approvals and audit evidence?", "governance")
	if ask || !strings.HasPrefix(tpl.Text, "A tailored demo can focus on governance, approvals and evidence.") {
		t.Fatalf("S10: %q", tpl.Text)
	}
	tpl, ask = DemoReply("I need to reschedule my demo.", "")
	if ask || tpl.Text != RouteTemplates["I08"].Text {
		t.Fatalf("reschedule must get the plain route: %q", tpl.Text)
	}
	for _, r := range []string{tpl.Text, TplDemoQualify.Text} {
		if strings.Contains(strings.ToLower(r), "booked for you") && !strings.Contains(r, "haven't booked") {
			t.Fatalf("implies a booking: %q", r)
		}
	}
}

func TestWithQualifier(t *testing.T) {
	got, id := WithQualifier("I15", "How do approvals work?", "Approvals follow policy.", Slots{}, 240)
	if id != "T15" || !strings.HasSuffix(got, GroundedQualifiers["I15"].Text) {
		t.Fatalf("T15 not appended: %q %s", got, id)
	}
	// Already said: not repeated.
	again, id := WithQualifier("I15", "q", got, Slots{}, 240)
	if again != got || id != "" {
		t.Fatalf("qualifier repeated: %q", again)
	}
	// T03 asks "which area?" only after an open discovery question.
	if _, id := WithQualifier("I02", "What is treasury and cash position?", "Treasury is in Finance.", Slots{}, 240); id != "" {
		t.Fatalf("a named capability must not be asked which area: %s", id)
	}
	if got, id := WithQualifier("I02", "What modules does ZoikoSuite include?", "Modules: ...", Slots{}, 240); id != "T03" || !strings.Contains(got, "Which area") {
		t.Fatalf("discovery question must ask which area: %q", got)
	}
	// T08: no "which system?" when the visitor named one.
	if got, _ := WithQualifier("I10", "Does it integrate with Xero?", "There is an API.", Slots{Integration: "Xero"}, 240); strings.Contains(got, "Which system") {
		t.Fatalf("asked which system after one was named: %q", got)
	}
	// Never past the word cap.
	long := strings.Repeat("word ", 235)
	if got, id := WithQualifier("I15", "q", long, Slots{}, 240); got != long || id != "" {
		t.Fatal("qualifier pushed the answer past the cap")
	}
}

// Doc 02 App. C: the versions are stable content hashes.
func TestIntentAndTemplateVersions(t *testing.T) {
	if len(IntentVersion()) != 12 || len(TemplateVersion()) != 12 || IntentVersion() == TemplateVersion() {
		t.Fatalf("versions %q %q", IntentVersion(), TemplateVersion())
	}
	if hashOf(intentRuleText()) != IntentVersion() || hashOf(templateText()) != TemplateVersion() {
		t.Fatal("versions are not deterministic")
	}
}
