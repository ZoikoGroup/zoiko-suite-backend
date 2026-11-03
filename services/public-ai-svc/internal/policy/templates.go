package policy

import (
	"errors"
	"regexp"
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
	Reason   string // contract.Reason*: why the turn ended this way
}

// ExceptionTemplates answer intercepted exception classes.
var ExceptionTemplates = map[string]Template{
	X01PrivateAction: {contract.RefusedAction,
		// Doc 02 T21.
		"I cannot view or change your financial, payroll or tax records in this public chat. To work with your account securely, use the authorized ZoikoSuite application or approved support channel.",
		"support", false, ""},
	X02ActionDenied: {contract.RefusedAction,
		// Doc 02 T21.
		"I cannot view or change your financial, payroll or tax records in this public chat. To work with your account securely, use the authorized ZoikoSuite application or approved support channel.",
		"support", false, ""},
	X03SensitiveInput: {contract.RefusedAction,
		// Doc 02 T22.
		"Please do not share passwords, codes, bank details or personal financial or employee records here. I will not repeat that information. Use the official secure support route if you need help with your account.",
		"support", false, ""},
	X04RegulatedAdvice: {contract.RefusedAction,
		// Doc 02 T20.
		"I can explain ZoikoSuite's published product capabilities, but I cannot provide advice tailored to your accounting, tax, employment or legal position. A qualified professional should assess the facts and jurisdiction.",
		"core_modules", false, ""},
	X05SecurityEscalate: {contract.Handoff,
		// Doc 02 T23.
		"If you believe there is a security incident or vulnerability, please use ZoikoSuite's official security disclosure/reporting channel. Do not post credentials, exploit details or customer records in this chat.",
		"trust", true, ""},
	X06PromptInjection: {contract.RefusedAction,
		"I can't change how I work or share my instructions. I can help with questions about ZoikoSuite's products, plans, integrations and security.",
		"", false, ""},
	X07CrossProduct: {contract.Abstain,
		// Doc 02 T19, with "This assistant" for the uncleared working name.
		"This assistant covers ZoikoSuite public information. Zoiko One and ZoikoLogia are separate products with their own capabilities and policies. I can point you to the relevant official product page if one is available.",
		"", false, ""},
	X08ClaimVerification: {contract.Abstain,
		"I can't give guarantees or confirm commitments in this chat. Official terms are published on ZoikoSuite's pages, and the sales team can confirm details in writing.",
		"demo", false, ""},
}

// Named outcome templates used by the orchestrator.
var (
	TplAbstainNoEvidence = Template{contract.Abstain, "I couldn't verify that from approved ZoikoSuite sources, so I won't guess.", "", false, ""}
	TplAbstainStale      = Template{contract.Abstain, "I can't verify the current position on that right now, so I won't guess.", "", false, ""}
	TplAbstainConflict   = Template{contract.Abstain, "I can't give a reliable answer on that right now because the official information is being reviewed.", "", false, ""}
	TplClarifyMarket     = Template{contract.Clarify, "I can help you compare ZoikoSuite plans. Prices, taxes, billing options and availability are market-dependent. Which country are you evaluating ZoikoSuite for?", "", false, ""} // Doc 02 T05
	TplClarifyPlan       = Template{contract.Clarify, "Which plan are you asking about?", "pricing", false, ""}
	TplUnavailable       = Template{contract.Unavailable, "I'm unable to retrieve reliable information right now. Please try again or use the official ZoikoSuite Support Center.", "support", false, ""}                                           // Doc 02 T24 (the widget shows the reference)
	TplOutOfScope        = Template{contract.Abstain, "I'm here to help with ZoikoSuite capabilities, plans, security, integrations, demos and support. I can help you find the right product information or support route.", "support", false, ""} // Doc 02 T25
	TplClarifyGiveUp     = Template{contract.Abstain, "I'm not sure I can help with that here. The support team can point you in the right direction.", "support", false, ""}
	// TplAbstainCoverage: a regional capability in a named country without
	// market-specific evidence (G12, G13): no assumed coverage.
	TplAbstainCoverage = Template{contract.Abstain, "I can't confirm what's available in that country from approved ZoikoSuite sources. The team can confirm coverage for your country.", "demo", false, ""}
	// TplAbstainTrialStale: trial terms for the named market cannot be
	// verified (expired or revoked claim). {{market}} is a display name.
	TplAbstainTrialStale = Template{contract.Abstain, "I can't verify the current trial terms for {{market}} right now. The official pricing page has the current information.", "pricing", false, ""}
	// TplLivePlatform: live platform state, tenant data or dashboard counts.
	TplLivePlatform = Template{contract.RefusedAction, "I can't see live platform data or account information. I can explain what ZoikoSuite's services and modules do, using public information.", "", false, ""}
	// TplNoApprovedInfo: a public question with no approved content yet.
	TplNoApprovedInfo = Template{contract.Abstain, "I don't have approved information on that yet. Which ZoikoSuite area are you asking about?", "", false, ""}
	// TplWelcome answers a bare greeting (Doc 02 T01).
	TplWelcome = Template{contract.Clarify, "Hello! I'm the ZoikoSuite AI guide. I can help you understand capabilities, current plans, security information, or how to arrange a demonstration. What would you like to explore?", "core_modules", false, ""} // Doc 02 T01
)

func init() {
	for p, r := range map[*Template]string{
		&TplAbstainNoEvidence: contract.ReasonNoEvidence, &TplAbstainStale: contract.ReasonExpiredSource,
		&TplAbstainConflict: contract.ReasonClaimConflict, &TplClarifyMarket: contract.ReasonMarketRequired,
		&TplClarifyPlan: contract.ReasonPlanRequired, &TplUnavailable: contract.ReasonKnowledgeUnavailable,
		&TplOutOfScope: contract.ReasonOutOfScope, &TplClarifyGiveUp: contract.ReasonClarifyExhausted,
		&TplAbstainCoverage: contract.ReasonMarketNotCovered, &TplWelcome: contract.ReasonGreeting,
		&TplAbstainTrialStale: contract.ReasonExpiredSource, &TplLivePlatform: contract.ReasonLivePlatformData,
		&TplNoApprovedInfo: contract.ReasonNoApprovedInfo,
	} {
		p.Reason = r
	}
	for code, r := range map[string]string{
		X01PrivateAction: contract.ReasonPrivateData, X02ActionDenied: contract.ReasonActionDenied,
		X03SensitiveInput: contract.ReasonSensitiveInput, X04RegulatedAdvice: contract.ReasonRegulatedAdvice,
		X05SecurityEscalate: contract.ReasonSecurityReport, X06PromptInjection: contract.ReasonPromptInjection,
		X07CrossProduct: contract.ReasonCrossProduct, X08ClaimVerification: contract.ReasonUnsupportedClaim,
	} {
		t := ExceptionTemplates[code]
		t.Reason = r
		ExceptionTemplates[code] = t
	}
	for id, t := range RouteTemplates {
		t.Reason = contract.ReasonRouteOnly
		RouteTemplates[id] = t
	}
}

// Suggestion chips offered when the assistant cannot answer.
var (
	// SuggestCoverage follows "Other" country: the general answer to the same
	// question, or the demo route where the team confirms coverage.
	SuggestCoverage = []contract.Chip{{Label: "Show general overview", Message: "Show general overview"}, {Label: "Book a demo", Message: "I'd like to book a demo"}}
	// SuggestRegionalCoverage follows a regional payroll, tax or legal
	// question for a country without approved material: the demo route.
	SuggestRegionalCoverage = []contract.Chip{{Label: "Book a demo", Message: "I'd like to book a demo"}}
	SuggestLivePlatform     = []contract.Chip{{Label: "Explore modules", Message: "What modules does ZoikoSuite include?"}, {Label: "Get support", Message: "I need help from support"}}
	SuggestAreas            = []contract.Chip{{Label: "Explore modules", Message: "What modules does ZoikoSuite include?"}, {Label: "Pricing and trial", Message: "What plans are available, and is there a free trial?"}, {Label: "Security and trust", Message: "How is data secured?"}}
)

// CountryName is a market's name on its own, as on a button ("United
// Kingdom"), where MarketName is its name inside a sentence ("the United
// Kingdom").
func CountryName(code string) string {
	n := strings.TrimPrefix(MarketName(code), "the ")
	return strings.ToUpper(n[:1]) + n[1:]
}

// MarketName is how a market code is named in template text.
func MarketName(code string) string {
	if n, ok := marketNames[code]; ok {
		return n
	}
	return "that country"
}

// BoundaryTemplates are Doc 02's approved replies for intents whose answer
// needs an approved source that does not exist yet. They are used only when
// no retrieved article is tagged for the intent, so an approved article
// always wins once it is published.
var BoundaryTemplates = map[string]Template{
	// T07 Enterprise.
	"I09": {contract.Handoff, "For multi-entity organizations or advanced governance, ZoikoSuite offers an enterprise discussion route. A specialist can confirm the deployment, integration, coverage and subscription scope relevant to your requirements.", "demo", false, contract.ReasonRouteOnly},
	// T11 Data residency.
	"I14": {contract.Abstain, "Data residency needs to be evaluated across storage, processing, backups, logs and subprocessors for the proposed deployment. I cannot guarantee a single-country boundary without an approved deployment-specific residency assessment.", "", false, contract.ReasonNoApprovedInfo},
	// T18 No verified status (availability, roadmap, release dates).
	"I25": {contract.Abstain, "I couldn't verify the current availability of that feature for your market and plan from an approved source. I can help you request confirmation from the appropriate ZoikoSuite team.", "demo", false, contract.ReasonNoApprovedInfo},
	// T30 Accessibility feedback.
	"I27": {contract.Handoff, "Thank you for raising an accessibility concern. Please use the official accessibility feedback route so the team can review the affected page and interaction. You do not need to share sensitive personal details in this chat.", "support", false, contract.ReasonRouteOnly},
	// I30 Responsible AI: the assistant's own disclosure (ui.welcome.disclosure).
	"I30": {contract.Abstain, "I'm an AI assistant, and my answers use public ZoikoSuite information only. I don't have approved information yet on how ZoikoSuite products use AI.", "", false, contract.ReasonNoApprovedInfo},
}

// AssuranceQuestionRe marks questions about legal admissibility or
// independent assurance (Doc 02 G14). They are answered only from a passage
// that addresses admissibility, else with TplEvidenceAssurance.
var AssuranceQuestionRe = regexp.MustCompile(`(?i)\badmissib|\bindependent(?:ly)? assur|\bassurance\b|\bstand up in court\b|\bcourt-?proof\b`)

// TplEvidenceAssurance is Doc 02 T14: evidence workflows, with no legal or
// assurance guarantee.
var TplEvidenceAssurance = Template{contract.Abstain, "ZoikoSuite describes evidence-linked workflows designed to preserve decision context, policies, approvals and supporting records. Independent assurance and legal admissibility require separate evidence and cannot be assumed from this description.", "", false, contract.ReasonNoApprovedInfo}

// TplAbstainCertification answers a certification question that has no
// verified trust claim: T10's due-diligence guidance, never a status.
var TplAbstainCertification = Template{contract.Abstain, "I can't confirm a certification status from an approved trust claim in this chat. For due diligence, please review the current Trust Center or request scoped evidence.", "trust", false, contract.ReasonNoEvidence}

// RouteTemplates are route-only intents: the assistant offers an official
// link and never claims a booking, ticket or callback exists (Doc 09 §11).
var RouteTemplates = map[string]Template{
	"I08": {contract.Handoff, "You can request a demo through ZoikoSuite's official demo page. I haven't booked anything for you.", "demo", false, ""},
	"I22": {contract.Handoff, "For account-specific information, please sign in through the official ZoikoSuite sign-in page. Do not share passwords, one-time codes or account records here.", "support", false, ""}, // Doc 02 T17
	// Doc 02 T16 (I21/I23) copy.
	"I23": {contract.Handoff, "I can help you find public guidance, but I cannot view your workspace or case history from this chat. Please use the secure Support Center to access the channels available to your account and plan.", "support", false, ""},
	"I24": {contract.Handoff, "I can direct you to the appropriate ZoikoSuite team. Is your enquiry about a product demonstration, an existing account, or security/procurement review?", "support", true, ""}, // Doc 02 T26
	// Doc 02 I26: ZoikoSuite facts only, never claims about other vendors.
	"I26": {contract.Abstain, "I can only describe ZoikoSuite, not other vendors' products. You can explore ZoikoSuite's modules to compare them with your requirements.", "core_modules", false, ""},
	"I28": {contract.Handoff, "I can't see billing details. Please use your ZoikoSuite account or the official support page for billing questions.", "support", false, ""},
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
