package policy

// UI microcopy registry and page-aware starter chips (Master prompt §6.4,
// Doc 02 §21). Served through GET /config so the widget never hard-codes
// customer-facing wording; changes ship with the policy version. Chips send
// fixed messages, so they classify like typed questions.

// Microcopy is keyed ui.*; values are plain text (no markup, no links).
var Microcopy = map[string]string{
	"ui.header":                  "ZoikoSuite Assistant",
	"ui.disclosure":              "AI assistant. Answers come from approved ZoikoSuite information only.",
	"ui.privacy":                 "Don't share passwords, payment, tax or account details.",
	"ui.placeholder":             "Ask about ZoikoSuite…",
	"ui.welcome":                 "Hi, I can answer questions about ZoikoSuite's products, plans, integrations and security from approved ZoikoSuite information. I can't see your account.",
	"ui.thinking":                "Checking approved sources…",
	"ui.clear":                   "Clear chat",
	"ui.clear.notice":            "Clearing the chat ends this session in your browser. It doesn't erase records the service must keep under its retention policy.",
	"ui.sensitive.warning":       "Looks like sensitive info. Please remove it before sending.",
	"ui.clarify.overview":        "Show general overview",
	"ui.error.unreachable":       "I can't reach the assistant right now. Please try again, or use the official ZoikoSuite pages.",
	"ui.error.timeout":           "That took too long. Please try again.",
	"ui.error.busy":              "I'm still finishing your previous message. Please try again in a moment.",
	"ui.error.rate_limited":      "You're sending messages quickly. Please wait a moment and try again.",
	"ui.unavailable":             "The assistant is unavailable right now. These official pages have the current information.",
	"ui.feedback.prompt":         "Was this helpful?",
	"ui.feedback.thanks":         "Thanks for the feedback.",
	"ui.privacy.title":           "About this assistant",
	"ui.privacy.body":            "This assistant uses AI to answer from approved, public ZoikoSuite information. It can't see or change any account. Your messages are processed to answer you and to keep the service safe; they're kept only as long as the approved retention policy allows. Please don't share personal, payment, tax or account details.",
	"ui.status.Available":        "Available",
	"ui.status.Limited":          "Limited",
	"ui.status.Planned":          "Planned",
	"ui.status.Not Available":    "Not available",
	"ui.status.Unverified":       "Unverified",
	"ui.status.Readiness":        "Readiness (not a certification)",
	"ui.status.Implemented":      "Implemented",
	"ui.status.In Validation":    "In validation",
	"ui.status.MARKET_DEPENDENT": "Depends on your country",
}

// Chip is one welcome-card starter.
type Chip struct {
	Label   string `json:"label"`
	Message string `json:"message"`
}

var (
	chipModules      = Chip{"Explore modules", "What modules does ZoikoSuite include?"}
	chipPricing      = Chip{"Pricing & trial", "What plans are available, and is there a free trial?"}
	chipDemo         = Chip{"Book a demo", "I'd like to book a demo"}
	chipTrust        = Chip{"Security & trust", "How is data secured?"}
	chipIntegrations = Chip{"Integrations", "Which integrations does ZoikoSuite support?"}
	chipSupport      = Chip{"Get support", "I need help from support"}
)

// Chips are the page-aware starter sets, keyed by page_context.
var Chips = map[string][]Chip{
	"home":         {chipModules, chipPricing, chipDemo, chipTrust, chipIntegrations, chipSupport},
	"pricing":      {chipPricing, chipDemo, chipModules, chipSupport},
	"core_modules": {chipModules, chipIntegrations, chipDemo, chipPricing},
	"demo":         {chipDemo, chipModules, chipPricing, chipTrust},
	"support":      {chipSupport, chipTrust, chipModules, chipDemo},
	"other":        {chipModules, chipPricing, chipDemo, chipTrust, chipIntegrations, chipSupport},
}
