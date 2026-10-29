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
	"ui.header":                   "ZoikoSuite Assistant",
	"ui.header.title":             "ZoikoSuite Assistant",
	"ui.header.logo_mark_url":     "/logo-z.png",
	"ui.header.logo_full_url":     "/logo.png",
	"ui.launcher.open":            "Chat with ZoikoSuite", // Doc01 §06
	"ui.launcher.close":           "Close chat",
	"ui.welcome.greeting":         "Hi, I can explain ZoikoSuite plans, modules, trust and integrations. I can't see or change any account.",
	"ui.draft":                    "Draft content",
	"ui.status.VERIFIED":          "Verified",
	"ui.header.subtitle":          "AI assistant. Public ZoikoSuite information only.",
	"ui.header.subtitle_short":    "AI assistant. Public information only.",
	"ui.welcome.disclosure":       "AI assistant. Answers use public ZoikoSuite information only.",
	"ui.more_topics":              "More topics",
	"ui.uncertain":                "I couldn't verify that.",
	"ui.offline":                  "You're offline. Check your connection, then try again.",
	"ui.jump":                     "Jump to latest",
	"ui.sensitive.remove":         "Remove it",
	"ui.end.title":                "Before you clear the chat",
	"ui.end.prompt":               "Was this chat helpful?",
	"ui.end.keep":                 "Keep chatting",
	"ui.retention.link":           "Retention notice",
	"ui.disclosure":               "AI assistant. Answers come from approved ZoikoSuite information only.",
	"ui.privacy":                  "Don't share passwords, payment, tax or account details.",
	"ui.privacy.link":             "Privacy and AI notice",
	"ui.placeholder":              "Ask about ZoikoSuite",
	"ui.welcome":                  "Hi, I can explain ZoikoSuite plans, modules, trust and integrations. I can't see or change any account.",
	"ui.thinking":                 "Checking approved sources…",
	"ui.clear":                    "Clear chat",
	"ui.clear.notice":             "Clearing the chat ends this session in your browser. It doesn't erase records the service must keep under its retention policy.",
	"ui.sensitive.warning":        "Looks like sensitive info. Remove it before sending.",
	"ui.empty":                    "Type a question first.",
	"ui.clarify.overview":         "Show general overview",
	"ui.clarify.other":            "Other",
	"ui.error.unreachable":        "I can't reach the assistant right now. Check your connection, then try again.",
	"ui.error.timeout":            "That took too long. Please try again.",
	"ui.error.busy":               "I'm still finishing your previous message. Please try again in a moment.",
	"ui.error.rate_limited":       "You're sending messages quickly. Wait a moment and try again.",
	"ui.unavailable":              "The assistant is unavailable right now. These official pages can help in the meantime.",
	"ui.retry":                    "Try again",
	"ui.feedback.prompt":          "Was this helpful?",
	"ui.feedback.thanks":          "Thanks for the feedback.",
	"ui.privacy.title":            "Privacy and AI notice",
	"ui.privacy.body":             "This assistant uses AI to answer from approved, public ZoikoSuite information. It can't see or change any account. Your messages are processed to answer you and to keep the service safe. Please don't share personal, payment, tax or account details.",
	"ui.privacy.retention":        "Chat records are kept for [period from signed policy], then deleted.",
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
}

// Chip is one welcome-card starter.
type Chip struct {
	Label   string `json:"label"`
	Message string `json:"message"`
}

var (
	chipPricing      = Chip{"Pricing and trial", "What plans are available, and is there a free trial?"}
	chipModules      = Chip{"Explore modules", "What modules does ZoikoSuite include?"}
	chipDemo         = Chip{"Book a demo", "I'd like to book a demo"}
	chipTrust        = Chip{"Security and trust", "How is data secured?"}
	chipIntegrations = Chip{"Integrations", "Which integrations does ZoikoSuite support?"}
	chipSupport      = Chip{"Get support", "I need help from support"}
)

// Chips are the page-aware starter sets, keyed by page_context.
var Chips = map[string][]Chip{
	"home":         {chipPricing, chipModules, chipDemo, chipTrust, chipIntegrations, chipSupport},
	"pricing":      {chipPricing, chipDemo, chipModules, chipSupport},
	"core_modules": {chipModules, chipIntegrations, chipDemo, chipPricing},
	"demo":         {chipDemo, chipModules, chipPricing, chipTrust},
	"support":      {chipSupport, chipTrust, chipModules, chipDemo},
	"other":        {chipPricing, chipModules, chipDemo, chipTrust, chipIntegrations, chipSupport},
}
