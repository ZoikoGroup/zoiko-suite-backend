package acceptance

import (
	"fmt"
	"strings"
	"testing"
)

// Docs 01-11, reviewed reply by reply (2026-11-11): the outcome check in
// TestDocsQuestions passed while some replies answered from an unrelated
// article or did not use the document's wording. Each case pins what its
// document asks for: the article it is answered from (by source id prefix),
// and/or words its approved wording must carry.
func TestDocsReplyTopics(t *testing.T) {
	x := publicKBServer(t)
	cases := []struct {
		before   []string
		msg      string
		source   string   // "" = any; else a cited source id must start with it
		has      []string // lower-case substrings the reply must contain
		lacks    []string
		docRefer string
	}{
		{nil, "Explore capabilities", "kb-core-modules", nil, nil, "Doc 02 T03"},
		{[]string{"Explore capabilities"}, "Can we manage five entities?", "kb-multi-entity", []string{"plan"}, []string{"tax module"}, "Doc 01 J01"},
		{nil, "bills", "kb-finance", []string{"payables"}, []string{"workspace"}, "Doc 02 §08 phrase table"},
		{nil, "legal entity", "kb-multi-entity", []string{"plan"}, nil, "Doc 02 §08 phrase table"},
		{nil, "how are approvals audited?", "kb-governance", []string{"recorded"}, nil, "Doc 02 §08 phrase table"},
		{nil, "Show general overview", "kb-overview", []string{"governance-first"}, []string{"tax jurisdiction"}, "Doc 02 §10 / T02"},
		{nil, "Explore platform", "kb-overview", nil, nil, "Doc 02 §22 / T02"},
		{nil, "What does the Trust Center say about data residency?", "", []string{"deployment", "trust center"}, []string{"mtls"}, "Doc 02 S07"},
		{nil, "Entity limits", "kb-multi-entity", []string{"plan"}, []string{"approval limit"}, "Doc 02 §22"},
		{nil, "Privacy", "kb-privacy", []string{"personal data"}, []string{"register a privacy notice records"}, "Doc 02 §22"},
		{nil, "Independent assurance", "", []string{"trust center", "readiness"}, nil, "Doc 02 §22 / T10"},
		{nil, "Are you ISO 27001 certified?", "", []string{"readiness", "trust center"}, []string{"we are iso"}, "Doc 01 AT-03 / T10"},
		{nil, "What are the six modules on the homepage?", "kb-overview", nil, nil, "Doc 03 KB-015"},
		{nil, "How does ZoikoSuite compare with QuickBooks?", "", nil, []string{"index", "search"}, "Doc 07 QA-T066 / T29"},
		{nil, "Book a demo and call me.", "", []string{"demo form"}, nil, "Doc 06 §20"},
		{nil, "Can AI run approvals automatically?", "", []string{"person"}, nil, "Doc 02 G15"},
		{nil, "Enterprise-grade encrypted everywhere", "", []string{"scope and evidence"}, []string{"free-forever"}, "Doc 02 §19"},
		{nil, "Unlimited free trial", "", []string{"set period", "conversion terms"}, nil, "Doc 02 §19"},
		{nil, "Automatic tax advice and filing", "", []string{"does not give tax advice", "filing preparation"}, nil, "Doc 02 §19"},
		{nil, "Works with every ERP", "", []string{"api version"}, []string{"every erp"}, "Doc 02 §19"},
		{nil, "100% compliant in every country", "", []string{"jurisdiction"}, []string{"free-forever"}, "Doc 02 §19"},
		{[]string{"Check my overdue invoices and send reminders."}, "My company is ACME and the invoice is 4519.", "", []string{"avoid sharing account or invoice information"}, []string{"acme", "4519"}, "Doc 02 S04"},
		{[]string{"Can my group of six subsidiaries use it?"}, "Can I get automatic consolidation today?", "", []string{"cannot verify immediate availability", "enterprise demo"}, nil, "Doc 02 S02"},
		{nil, "Tax filing in Germany", "", []string{"germany", "not universal", "tax advice"}, nil, "Doc 02 G12 / T12"},
		{nil, "Payroll in Jamaica", "", []string{"jamaica", "jurisdiction"}, nil, "Doc 02 G13 / T13"},
		{nil, "What can be covered", "", []string{"governance", "evidence", "nothing is booked"}, nil, "Doc 02 §22 / S10"},
		{nil, "Tell me if this price is guaranteed.", "", []string{"pricing"}, nil, "Doc 06 §20"},
		{nil, "I'd like someone from sales to contact me.", "", []string{"contact form"}, nil, "Doc 10 COM-04"},
		{nil, "I need to reschedule my demo.", "", []string{"reschedule"}, []string{"request a demo through"}, "Doc 10 COM-05"},
		{nil, "I can't log in to my account.", "", []string{"password reset", "support"}, nil, "Doc 10 COM-06"},
		{nil, "Does Growth include integrations?", "", []string{"integrations"}, []string{"sso"}, "Doc 03 §17"},
	}
	for i, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			conv := &conversation{x: x, s: x.session(fmt.Sprintf("topics-%04d", i))}
			for _, b := range c.before {
				conv.say(b)
			}
			e := conv.say(c.msg)
			low := strings.ToLower(e.Text)
			if c.source != "" {
				ok := false
				for _, ci := range e.Citations {
					ok = ok || strings.HasPrefix(ci.SourceVersionID, c.source)
				}
				if !ok {
					t.Errorf("%s: %q cites %+v, want %s*: %q", c.docRefer, c.msg, e.Citations, c.source, e.Text)
				}
			}
			for _, w := range c.has {
				if !strings.Contains(low, w) {
					t.Errorf("%s: %q reply lacks %q: %q", c.docRefer, c.msg, w, e.Text)
				}
			}
			for _, w := range c.lacks {
				if strings.Contains(low, w) {
					t.Errorf("%s: %q reply says %q: %q", c.docRefer, c.msg, w, e.Text)
				}
			}
		})
	}
}
