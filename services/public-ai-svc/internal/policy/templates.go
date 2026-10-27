package policy

import (
	"errors"
	"strings"

	"zoiko.io/public-ai-svc/internal/contract"
)

// Template is a pre-approved response. Wording is a neutral DEV DRAFT based
// on Doc 02 §17 and Doc 06 App. B; final copy needs Legal/Product review.
// Templates never quote the visitor's message back.
type Template struct {
	Status   contract.Status
	Text     string
	RouteKey string // official route for the single CTA; "" for none
	Human    bool   // sets flags.requires_human
}

// ExceptionTemplates answer intercepted exception classes.
var ExceptionTemplates = map[string]Template{
	X01PrivateAction: {contract.RefusedAction,
		"I can't access account, financial, payroll or employee records. I'm a public guide to ZoikoSuite and have no access to customer data. Please sign in to ZoikoSuite or contact support through the official support page.",
		"support", false},
	X02ActionDenied: {contract.RefusedAction,
		"I can't carry out actions such as payments, approvals, filings, exports or account changes. I only provide public product information. Please use your ZoikoSuite account or contact support.",
		"support", false},
	X03SensitiveInput: {contract.RefusedAction,
		"For your security, please don't share passwords, codes, card, bank or ID numbers here. I haven't kept what you sent. If you need help with your account, please use the official support page.",
		"support", false},
	X04RegulatedAdvice: {contract.RefusedAction,
		"I can't give tax, legal, accounting, employment or investment advice. Please consult a qualified professional for decisions like this. I can explain what ZoikoSuite's modules do.",
		"core_modules", false},
	X05SecurityEscalate: {contract.Handoff,
		"Thank you for raising this. Please report security concerns through ZoikoSuite's official trust and security page so the security team can review them. I can't assess security issues in this chat.",
		"trust", true},
	X06PromptInjection: {contract.RefusedAction,
		"I can't change how I work or share my instructions. I can help with questions about ZoikoSuite's products, plans, integrations and security.",
		"", false},
	X07CrossProduct: {contract.Abstain,
		"I can only help with ZoikoSuite. For other Zoiko products, please visit that product's official website.",
		"", false},
	X08ClaimVerification: {contract.Abstain,
		"I can't give guarantees or confirm commitments in this chat. Official terms are published on ZoikoSuite's pages, and the sales team can confirm details in writing.",
		"demo", false},
}

// Named outcome templates used by the orchestrator.
var (
	TplAbstainNoEvidence = Template{contract.Abstain, "I couldn't verify that from approved ZoikoSuite sources, so I won't guess.", "", false}
	TplAbstainStale      = Template{contract.Abstain, "I can't verify the current position on that right now, so I won't guess.", "", false}
	TplAbstainConflict   = Template{contract.Abstain, "I can't give a reliable answer on that right now because the official information is being reviewed.", "", false}
	TplClarifyMarket     = Template{contract.Clarify, "Which country are you evaluating ZoikoSuite for?", "", false}
	TplClarifyPlan       = Template{contract.Clarify, "Which plan are you asking about?", "pricing", false}
	TplUnavailable       = Template{contract.Unavailable, "I can't answer right now. Please use the official ZoikoSuite pages for pricing, demos and support.", "support", false}
	TplOutOfScope        = Template{contract.Abstain, "I can help with questions about ZoikoSuite's products, plans, integrations and security. Could you rephrase your question about ZoikoSuite?", "", false}
	TplClarifyGiveUp     = Template{contract.Abstain, "I'm not sure I can help with that here. The support team can point you in the right direction.", "support", false}
	// TplWelcome answers a bare greeting (Doc 02 T01).
	TplWelcome = Template{contract.Clarify, "Hi! I'm the ZoikoSuite assistant. I can answer questions about ZoikoSuite's modules, plans, integrations and security. What would you like to know?", "core_modules", false}
)

// RouteTemplates are route-only intents: the assistant offers an official
// link and never claims a booking, ticket or callback exists (Doc 09 §11).
var RouteTemplates = map[string]Template{
	"I08": {contract.Handoff, "You can request a demo through ZoikoSuite's official demo page. I haven't booked anything for you.", "demo", false},
	"I22": {contract.Handoff, "I can't access accounts or sign you in. Please use the official ZoikoSuite sign-in or support page.", "support", false},
	"I23": {contract.Handoff, "I can't open support requests from this chat. Please contact the support team through the official support page.", "support", false},
	"I24": {contract.Handoff, "I can't connect you to a person from this chat, and I haven't created a request. You can reach the team through the official contact page.", "support", true},
	// Doc 02 I26: ZoikoSuite facts only, never claims about other vendors.
	"I26": {contract.Abstain, "I can only describe ZoikoSuite, not other vendors' products. You can explore ZoikoSuite's modules to compare them with your requirements.", "core_modules", false},
	"I28": {contract.Handoff, "I can't see billing details. Please use your ZoikoSuite account or the official support page for billing questions.", "support", false},
}

// DefaultRoute is the CTA offered alongside answers for each intent.
var DefaultRoute = map[string]string{
	"I01": "core_modules", "I02": "core_modules", "I03": "pricing", "I04": "demo", "I05": "pricing",
	"I06": "pricing", "I07": "pricing", "I08": "demo", "I09": "demo", "I10": "core_modules",
	"I11": "demo", "I12": "trust", "I13": "trust", "I14": "trust", "I15": "core_modules",
	"I16": "core_modules", "I17": "core_modules", "I18": "core_modules", "I19": "core_modules", "I20": "core_modules",
	"I21": "support", "I22": "support", "I23": "support", "I24": "support", "I25": "core_modules",
	"I26": "core_modules", "I27": "support", "I28": "support", "I29": "support", "I30": "trust",
}

// ErrUnresolvedPlaceholder is returned when a rendered template still
// contains a {{placeholder}}; such text must never be shown (Doc 02 §17).
var ErrUnresolvedPlaceholder = errors.New("unresolved template placeholder")

// Render fills {{name}} placeholders.
func Render(text string, vars map[string]string) (string, error) {
	for k, v := range vars {
		text = strings.ReplaceAll(text, "{{"+k+"}}", v)
	}
	if strings.Contains(text, "{{") || strings.Contains(text, "}}") {
		return "", ErrUnresolvedPlaceholder
	}
	return text, nil
}
