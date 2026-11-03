package policy

// Guided topic menu and follow-ups for the structured widget (ADR-008).
//
// Nothing here is invented: categories are the Doc 02 §22 public page
// families, labelled with the Homepage chips (Explore platform, Compare
// plans, Security & trust, Book demo) and "Support Center"; items are each
// family's recommended chips, mapped to their Doc 02 §06 intent. Follow-ups
// come from the Doc 02 §08 secondary-intent pairs, then the other items of
// the same family, in menu order. The model never chooses any of them.
//
// Every item sends a fixed message that classifies to its intent like a
// typed question (menu_test.go), so the menu cannot bypass the policy. Plan
// items ask about price, so they take the market-then-signed-claims path
// (Doc 02 I05) instead of a general answer.

// MenuItem is one menu entry: a label, the intent it stands for and the
// fixed message it sends.
type MenuItem struct {
	ID       string
	Label    string
	IntentID string
	Message  string
}

// MenuCategory is one Doc 02 §22 page family.
type MenuCategory struct {
	ID    string
	Label string
	Items []MenuItem
}

// MaxFollowUps bounds followups[] on a turn.
const MaxFollowUps = 4

// Menu is the guided menu, in display order.
var Menu = []MenuCategory{
	{ID: "platform", Label: "Explore platform", Items: []MenuItem{
		{"platform.governance", "Governance", "I15", "How do governance and approvals work in ZoikoSuite?"},
		{"platform.multi_entity", "Multi-entity", "I03", "Does ZoikoSuite support multiple entities?"},
		{"platform.evidence", "Evidence", "I16", "How does ZoikoSuite keep audit evidence?"},
		{"platform.integrations", "Integrations", "I10", "Which integrations does ZoikoSuite support?"},
	}},
	{ID: "pricing", Label: "Compare plans", Items: []MenuItem{
		{"pricing.plans", "Plan differences", "I05", "What do the plans cost?"},
		{"pricing.trial", "Trial terms", "I06", "Is there a free trial?"},
		{"pricing.entities", "Entity limits", "I03", "How many entities can each plan include?"},
		{"pricing.sales", "Talk to sales", "I24", "I'd like to talk to sales"},
	}},
	{ID: "trust", Label: "Security & trust", Items: []MenuItem{
		{"trust.assurance", "Independent assurance", "I12", "Which security certifications does ZoikoSuite hold?"},
		{"trust.residency", "Data residency", "I14", "Where is ZoikoSuite data stored?"},
		{"trust.privacy", "Privacy", "I13", "How does ZoikoSuite handle privacy and personal data?"},
		{"trust.review", "Security review", "I12", "How can we run a security review of ZoikoSuite?"},
	}},
	{ID: "demo", Label: "Book demo", Items: []MenuItem{
		{"demo.coverage", "What can be covered", "I08", "What can a demo cover?"},
		{"demo.plans", "Plans", "I05", "What do the plans cost?"},
		{"demo.enterprise", "Enterprise", "I09", "Tell me about enterprise options"},
		{"demo.integrations", "Integrations", "I10", "Which integrations does ZoikoSuite support?"},
	}},
	{ID: "support", Label: "Support Center", Items: []MenuItem{
		{"support.sign_in", "Sign in", "I22", "How do I sign in?"},
		{"support.request", "Submit a request", "I23", "How do I raise a support ticket?"},
		{"support.knowledge_base", "Knowledge base", "I21", "Where is the ZoikoSuite documentation?"},
		{"support.security", "Security concern", "X05", "I want to report a security vulnerability"},
	}},
}

// secondaryIntents are the Doc 02 §08 primary/secondary pairs, used first
// as follow-ups to the primary.
var secondaryIntents = map[string][]string{
	"I02": {"I15"}, "I03": {"I02"}, "I18": {"I04"}, "I12": {"I14"}, "I15": {"I16"}, "I05": {"I06"},
}

// IntentCTA is the Doc 02 §06 "Default CTA" label for each intent.
var IntentCTA = map[string]string{
	"I01": "Explore platform", "I02": "View capability", "I03": "Compare plans", "I04": "Talk to specialist",
	"I05": "Compare plans", "I06": "Start eligible trial", "I07": "View plan terms", "I08": "Book demo",
	"I09": "Talk to solutions architect", "I10": "View integration guide", "I11": "Book migration review",
	"I12": "Open Trust Center", "I13": "View privacy information", "I14": "Start security review",
	"I15": "Explore governance", "I16": "Explore evidence", "I17": "Request regional review", "I18": "Explore workforce",
	"I19": "Explore legal", "I20": "Explore reporting", "I21": "Open support center", "I22": "Sign in securely",
	"I23": "Submit secure request", "I24": "Open secure form", "I25": "Request update", "I26": "Explore platform",
	"I27": "Send site feedback", "I28": "Open billing help", "I29": "Help / human route", "I30": "Review responsible AI",
}

// RouteTitles names the official page behind a route key when a citation
// points at a route rather than a knowledge source.
var RouteTitles = map[string]string{
	"pricing": "Pricing and plans", "signup_or_trial": "Start a trial", "demo": "Book a demo",
	"support": "Support center", "trust": "Trust Center", "core_modules": "Core modules", "integrations": "Integrations",
}

// itemFor returns the first menu item for an intent.
func itemFor(intent string) (MenuItem, bool) {
	for _, c := range Menu {
		for _, it := range c.Items {
			if it.IntentID == intent {
				return it, true
			}
		}
	}
	return MenuItem{}, false
}

// FollowUps returns up to MaxFollowUps menu items to offer after an answer
// to intent: its §08 secondary intents, then the other items of every
// family that lists it. Never the intent itself, never twice the same
// message.
func FollowUps(intent string) []MenuItem {
	var out []MenuItem
	seen := map[string]bool{}
	add := func(it MenuItem) {
		if len(out) < MaxFollowUps && it.IntentID != intent && !seen[it.Message] {
			seen[it.Message] = true
			out = append(out, it)
		}
	}
	for _, sec := range secondaryIntents[intent] {
		if it, ok := itemFor(sec); ok {
			add(it)
		}
	}
	for _, c := range Menu {
		listed := false
		for _, it := range c.Items {
			listed = listed || it.IntentID == intent
		}
		if !listed {
			continue
		}
		for _, it := range c.Items {
			add(it)
		}
	}
	return out
}
