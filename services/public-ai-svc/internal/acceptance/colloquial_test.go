package acceptance

import (
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
)

// QA-T073 (Doc 07): "Test typos, abbreviations and colloquial finance
// phrases." Each message must reach the same intent (or, for a safety case,
// the same refusal) as its plain wording. intent "" accepts any intent.
type colloquial struct {
	kind, msg, intent string
	status            []contract.Status
}

var colloquialCases = []colloquial{
	// Typos.
	{"typo", "whats the pricng for the growth plan", "I05", st(clr, ans, abs)},
	{"typo", "is zoikosuite soc2 certifed?", "I12", st(ans, abs)},
	{"typo", "do u intergrate with quickbooks", "I10", st(ans, abs, clr)},
	{"typo", "multi entitiy support?", "I03", st(ans, abs, clr)},
	{"typo", "can i book a demmo", "I08", st(hof)},
	{"typo", "do you have a free trail", "I06", st(clr, ans, abs)},
	{"typo", "what moduels do you have", "I02", st(ans)},
	{"typo", "pay my invioces", "X02", st(ref)},
	{"typo", "is my data encrytped", "I12", st(ans, abs)},
	{"typo", "how do i migrte from sage", "I11", st(ans, abs, clr)},
	// Abbreviations.
	{"abbrev", "AP automation?", "I02", st(ans)},
	{"abbrev", "do you do AR and GL", "I02", st(ans)},
	{"abbrev", "SoD controls?", "I15", st(ans, abs)},
	{"abbrev", "can I get a DPA", "I13", st(ans, hof, abs)},
	{"abbrev", "SSO support?", "", st(ans, abs)},
	{"abbrev", "PO approvals", "", st(ans, abs)},
	{"abbrev", "pls show me ur pricing", "I05", st(clr, ans)},
	{"abbrev", "info on HR & payroll", "I18", st(ans, abs)},
	{"abbrev", "is there an API for AP", "", st(ans, abs)},
	{"abbrev", "VAT filing in DE?", "I17", st(ans, abs, ref)},
	// Colloquial finance phrases.
	{"colloquial", "can it chase customers who pay late", "I02", st(ans, abs)},
	{"colloquial", "does it help with month-end close", "I02", st(ans, abs)},
	{"colloquial", "we need to keep the books for 5 companies", "I03", st(ans, abs, clr)},
	{"colloquial", "how do we pay our suppliers with it", "I02", st(ans, abs)},
	{"colloquial", "can I see who signed off on spend", "I15", st(ans, abs)},
	{"colloquial", "how much does this thing set us back", "I05", st(clr, ans)},
	{"colloquial", "we're drowning in paper invoices", "I02", st(ans, abs)},
	{"colloquial", "can it handle cross-border payroll", "I18", st(ans, abs)},
	{"colloquial", "money in, money out across our group", "", st(ans, abs, clr)},
	{"colloquial", "can it tie out bank statements", "", st(ans, abs)},
}

func TestQAT073TyposAbbreviationsColloquial(t *testing.T) {
	x := newH(t)
	for _, c := range colloquialCases {
		t.Run(c.kind+"/"+c.msg, func(t *testing.T) {
			r := x.ask(c.msg)
			if !oneOf(r.Status, c.status) {
				t.Errorf("%q: status %s (%s/%s), want one of %v: %q", c.msg, r.Status, r.IntentID, r.Reason, c.status, r.Text)
			}
			if c.intent != "" && r.IntentID != c.intent {
				t.Errorf("%q: intent %s, want %s", c.msg, r.IntentID, c.intent)
			}
		})
	}
}
