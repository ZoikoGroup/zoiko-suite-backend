package policy

// UI microcopy registry and page-aware starter chips (Master prompt §6.4,
// Doc 02 §21). Served through GET /config so the widget never hard-codes
// customer-facing wording; changes ship with the policy version. Chips send
// fixed messages, so they classify like typed questions.
//
// The public name is "ZoikoSuite Assistant" until Legal and Brand clear
// another one; no other working name appears in public UI.

// Microcopy is keyed ui.*; values are plain text (no markup, no links).
// ui.header.logo_mark_url and ui.header.logo_full_url are paths on the page's own origin; the widget accepts
// only same-origin paths.
var Microcopy = map[string]string{
	"ui.header":                "ZoikoSuite Assistant",
	"ui.header.title":          "ZoikoSuite Assistant",
	"ui.header.logo_mark_url":  "/logo-z.png",
	"ui.header.logo_full_url":  "/logo.png",
	"ui.launcher.open":         "Chat with ZoikoSuite", // Doc01 §06
	"ui.launcher.close":        "Close chat",
	"ui.welcome.greeting":      "Hi, I can explain ZoikoSuite plans, modules, trust and integrations. I can't see or change any account.",
	"ui.draft":                 "Draft content",
	"ui.status.VERIFIED":       "Verified",
	"ui.header.subtitle":       "AI assistant. Public ZoikoSuite information only.",
	"ui.header.subtitle_short": "AI assistant. Public information only.",
	"ui.welcome.disclosure":    "AI assistant. Answers use public ZoikoSuite information only.",
	"ui.more_topics":           "More topics",
	"ui.uncertain":             "I couldn't verify that.",
	"ui.offline":               "You're offline. Check your connection, then try again.",
	"ui.jump":                  "Jump to latest",
	"ui.sensitive.remove":      "Remove it",
	"ui.end.title":             "Before you clear the chat",
	"ui.end.prompt":            "Was this chat helpful?",
	"ui.end.keep":              "Keep chatting",
	"ui.retention.link":        "Retention notice",
	"ui.disclosure":            "AI assistant. Product information only.",                                      // Doc 02 §21
	"ui.privacy":               "Please don't share passwords, payment details, or private financial records.", // Doc 02 §21
	"ui.privacy.link":          "Privacy and AI notice",
	"ui.placeholder":           "Ask about ZoikoSuite…",
	"ui.welcome":               "Hi, I can explain ZoikoSuite plans, modules, trust and integrations. I can't see or change any account.",
	"ui.loading":               "Looking for verified information…", // Doc 02 §21
	"ui.thinking":              "Looking for verified information…", // older widgets read this key
	"ui.clear":                 "Clear chat",
	"ui.clear.notice":          "Clearing the chat ends this session in your browser. It doesn't erase records the service must keep under its retention policy.",
	"ui.sensitive.warning":     "Looks like sensitive info. Remove it before sending.",
	"ui.empty":                 "Type a question first.",
	"ui.clarify.overview":      "Show general overview",
	"ui.clarify.other":         "Other",
	"ui.error.unreachable":     "I can't reach the assistant right now. Check your connection, then try again.",
	"ui.error.timeout":         "That took too long. Please try again.",
	"ui.error.busy":            "I'm still finishing your previous message. Please try again in a moment.",
	"ui.error.rate_limited":    "You're sending messages quickly. Wait a moment and try again.",
	"ui.unavailable":           "The assistant is unavailable right now. These official pages can help in the meantime.",
	"ui.retry":                 "Try again",
	"ui.feedback.prompt":       "Was this helpful?",
	"ui.feedback.thanks":       "Thanks for the feedback.",
	"ui.privacy.title":         "Privacy and AI notice",
	"ui.privacy.body":          "This assistant uses AI to answer from approved, public ZoikoSuite information. It can't see or change any account. Your messages are processed to answer you and to keep the service safe. Please don't share personal, payment, tax or account details.",
	// Empty until the signed retention policy sets the period (DEC-06): the
	// widget hides an empty line and never shows a "[…]" placeholder.
	"ui.privacy.retention":        "",
	"ui.send":                     "Send message",
	"ui.close":                    "Close chat",
	"ui.source":                   "View source",
	"ui.clarify.skip":             "Show general overview",
	"ui.reset":                    "Start a new conversation",
	"ui.feedback.yes":             "This was helpful",
	"ui.feedback.no":              "This did not answer my question",
	"ui.timeout":                  "Your public chat session has ended. Start a new conversation to continue.",
	"ui.error":                    "I couldn't connect. Please try again or open support.",
	"ui.accessibility":            "Report an accessibility issue",
	"ui.status.Available":         "Available",
	"ui.status.Limited":           "Limited",
	"ui.status.Partner-Supported": "Partner-supported",
	"ui.status.Planned":           "Planned",
	"ui.status.Not Available":     "Not available",
	"ui.status.Unverified":        "Unverified",
	"ui.status.Readiness":         "Readiness (not a certification)",
	"ui.status.Implemented":       "Implemented",
	"ui.status.In Validation":     "In validation",
	"ui.status.MARKET_DEPENDENT":  "Depends on your country",
	// Every other visible widget string is keyed too (Doc 02 §23).
	"ui.sources":                      "Sources",
	"ui.feedback.reason":              "What was wrong?",
	"ui.feedback.reason.incorrect":    "Incorrect",
	"ui.feedback.reason.incomplete":   "Incomplete",
	"ui.feedback.reason.not_relevant": "Not relevant",
	"ui.feedback.reason.unclear":      "Unclear",
	"ui.end.yes":                      "Yes, this chat was helpful",
	"ui.end.no":                       "No, this chat wasn't helpful",
	"ui.reference":                    "Reference:",
	"ui.accessibility.message":        "I'd like to report an accessibility issue.",
	"ui.refused":                      "Not something I can do here",
	"ui.stopped":                      "Stopped. Ask again whenever you're ready.",
	"ui.stopped.short":                "Stopped.",
	"ui.cleared":                      "Chat cleared.",
	"ui.choose":                       "Choose an answer",
	"ui.suggested":                    "Suggested questions",
	"ui.link.official":                "(official ZoikoSuite page, opens in a new tab)",
	"ui.link.new_tab":                 "(opens in a new tab)",
	"ui.retry_after":                  "You can try again in {seconds} s.",
	"ui.route.default":                "Open official page",
	"ui.route.pricing":                "Compare plans",
	"ui.route.demo":                   "Book a demo",
	"ui.route.support":                "Open secure support", // Doc 09 §11
	"ui.route.core_modules":           "Explore modules",
	"ui.route.trust":                  "Open Trust Center",
	"ui.route.signup_or_trial":        "Start a trial",
	"ui.route.sign-in":                "Sign in securely", // Doc 02 T17/T21
	// Doc 09 §11: shown before the visitor leaves for a secure destination.
	"ui.channel.demo":          "Open our secure demo form to send your request.",
	"ui.channel.support":       "You are leaving the assistant for secure support.",
	"ui.channel.sign-in":       "You are leaving the assistant for the secure sign-in page.",
	"ui.channel.trust":         "This opens the official trust and security page.",
	"ui.channel.pricing":       "This opens the official pricing page.",
	"ui.fallback.pricing":      "Pricing",
	"ui.fallback.core_modules": "Core modules",
	"ui.fallback.trust":        "Trust center",
	"ui.fallback.support":      "Support",
}

// Chip is one welcome-card starter.
type Chip struct {
	Label   string `json:"label"`
	Message string `json:"message"`
}

// chip reuses a guided-menu question, so a chip classifies exactly as the
// menu item does.
func chip(id string) Chip {
	for _, c := range Menu {
		for _, it := range c.Items {
			if it.ID == id {
				return Chip{it.Label, it.Message}
			}
		}
	}
	panic("policy: unknown menu item " + id)
}

// Homepage chips: Doc 02 T01 quick actions and §22 Homepage.
var homeChips = []Chip{
	{"Explore platform", "What can ZoikoSuite do?"},
	{"Compare plans", "What do the plans cost?"},
	{"Book demo", "I'd like to book a demo"},
	{"Security & trust", "How is data secured?"},
}

// Chips are the Doc 02 §22 page-context starter sets, keyed by
// page_context. An unknown page falls back to the four general chips.
var Chips = map[string][]Chip{
	"home":         homeChips,
	"pricing":      {chip("pricing.plans"), chip("pricing.trial"), chip("pricing.entities"), chip("pricing.sales")},
	"core_modules": {chip("platform.governance"), chip("platform.multi_entity"), chip("platform.evidence"), chip("platform.integrations")},
	"trust":        {chip("trust.assurance"), chip("trust.residency"), chip("trust.privacy"), chip("trust.review")},
	"demo":         {chip("demo.coverage"), chip("demo.plans"), chip("demo.enterprise"), chip("demo.integrations")},
	"support":      {chip("support.sign_in"), chip("support.request"), chip("support.knowledge_base"), chip("support.security")},
	"other":        homeChips,
}
