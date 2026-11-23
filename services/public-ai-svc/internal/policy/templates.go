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
		"sign-in", false, ""},
	X02ActionDenied: {contract.RefusedAction,
		// Doc 02 T21.
		"I cannot view or change your financial, payroll or tax records in this public chat. To work with your account securely, use the authorized ZoikoSuite application or approved support channel.",
		"sign-in", false, ""},
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
	X09UnsupportedCover: {contract.Abstain,
		// Doc 02 §19: reject the absolute, qualify, refer to approved coverage.
		"I can't confirm blanket coverage, compatibility or free-forever terms. Coverage, obligations and supported systems vary by scope, market and jurisdiction, and trials have defined terms. The team can confirm what applies to your situation.",
		"demo", false, ""},
}

// Named outcome templates used by the orchestrator.
var (
	TplAbstainNoEvidence = Template{contract.Abstain, "I couldn't verify that from approved ZoikoSuite sources, so I won't guess.", "", false, ""}
	TplAbstainStale      = Template{contract.Abstain, "I can't verify the current position on that right now, so I won't guess.", "", false, ""}
	TplAbstainConflict   = Template{contract.Abstain, "I can't give a reliable answer on that right now because the official information is being reviewed.", "", false, ""}
	TplClarifyMarket     = Template{contract.Clarify, "I can help you compare ZoikoSuite plans. Prices, taxes, billing options and availability are market-dependent. Which country are you evaluating ZoikoSuite for?", "", false, ""} // Doc 02 T05
	// TplClarifyTrialMarket asks for the market when the question is about the
	// trial: Doc 02 §18 S01 turn 1, word for word. The 30-day figure is the
	// published pricing page's (Doc 01 §19, Doc 03 S03); a market's trial
	// claims must state the same length or the release review flags it.
	TplClarifyTrialMarket = Template{contract.Clarify, "The published pricing page describes a 30-day trial for eligible Starter and Growth plans. Eligibility varies by market. Which country are you evaluating ZoikoSuite for?", "", false, ""}
	TplClarifyPlan        = Template{contract.Clarify, "Which plan are you asking about?", "pricing", false, ""}
	TplUnavailable        = Template{contract.Unavailable, "I'm unable to retrieve reliable information right now. Please try again or use the official ZoikoSuite Support Center.", "support", false, ""}                                           // Doc 02 T24 (the widget shows the reference)
	TplOutOfScope         = Template{contract.Abstain, "I'm here to help with ZoikoSuite capabilities, plans, security, integrations, demos and support. I can help you find the right product information or support route.", "support", false, ""} // Doc 02 T25
	TplClarifyGiveUp      = Template{contract.Abstain, "I'm not sure I can help with that here. The support team can point you in the right direction.", "support", false, ""}
	// TplAbstainCoverage: a regional capability in a named country without
	// market-specific evidence (G12, G13): no assumed coverage.
	TplAbstainCoverage = Template{contract.Abstain, "I can't confirm what's available in that country from approved ZoikoSuite sources. The team can confirm coverage for your country.", "demo", false, ""}
	// TplAbstainTrialStale: trial terms for the named market cannot be
	// verified (expired or revoked claim). {{market}} is a display name.
	TplAbstainTrialStale = Template{contract.Abstain, "I can't verify the current trial terms for {{market}} right now. The official pricing page has the current information.", "pricing", false, ""}
	TplPriceIncomplete   = Template{contract.Abstain, "I can't confirm exact pricing from an approved price book in this chat. The official pricing page has the current terms.", "pricing", false, ""}
	TplTrialIncomplete   = Template{contract.Abstain, "Trial availability depends on plan and market. The official pricing page has the current terms.", "pricing", false, ""}
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
		&TplAbstainConflict: contract.ReasonClaimConflict, &TplClarifyMarket: contract.ReasonMarketRequired, &TplClarifyTrialMarket: contract.ReasonMarketRequired,
		&TplClarifyPlan: contract.ReasonPlanRequired, &TplUnavailable: contract.ReasonKnowledgeUnavailable,
		&TplOutOfScope: contract.ReasonOutOfScope, &TplClarifyGiveUp: contract.ReasonClarifyExhausted,
		&TplAbstainCoverage: contract.ReasonMarketNotCovered, &TplWelcome: contract.ReasonGreeting,
		&TplAbstainTrialStale: contract.ReasonExpiredSource, &TplLivePlatform: contract.ReasonLivePlatformData,
		&TplNoApprovedInfo: contract.ReasonNoApprovedInfo, &TplPriceIncomplete: contract.ReasonNoEvidence,
		&TplTrialIncomplete: contract.ReasonNoEvidence,
	} {
		p.Reason = r
	}
	for code, r := range map[string]string{
		X01PrivateAction: contract.ReasonPrivateData, X02ActionDenied: contract.ReasonActionDenied,
		X03SensitiveInput: contract.ReasonSensitiveInput, X04RegulatedAdvice: contract.ReasonRegulatedAdvice,
		X05SecurityEscalate: contract.ReasonSecurityReport, X06PromptInjection: contract.ReasonPromptInjection,
		X07CrossProduct: contract.ReasonCrossProduct, X08ClaimVerification: contract.ReasonUnsupportedClaim,
		X09UnsupportedCover: contract.ReasonUnsupportedClaim,
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

// TplDataRights answers access, export or erasure requests route-only
// (Doc 09 §12): nothing is recorded, exported or deleted from the chat.
var TplDataRights = Template{contract.Handoff, "I can't act on data requests in this chat. To ask for access to, a copy of, or deletion of your personal data, please use ZoikoSuite's official privacy request route. Nothing has been recorded, exported or deleted from here.", "trust", false, contract.ReasonRouteOnly}

// Suggestion chips offered when the assistant cannot answer.
var (
	// SuggestCoverage follows "Other" country: the general answer to the same
	// question, or the demo route where the team confirms coverage.
	SuggestCoverage = []contract.Chip{{Label: "Show general overview", Message: "Show general overview"}, {Label: "Book a demo", Message: "I'd like to book a demo"}}
	// SuggestRegionalCoverage follows a regional payroll, tax or legal
	// question for a country without approved material: the demo route.
	SuggestRegionalCoverage = []contract.Chip{{Label: "Book a demo", Message: "I'd like to book a demo"}}
	SuggestLivePlatform     = []contract.Chip{{Label: "Explore modules", Message: "What modules does ZoikoSuite include?"}, {Label: "Get support", Message: "I need help from support"}}
	// SuggestTeams answers T26's "which team?" (Doc 02 I24).
	SuggestTeams = []contract.Chip{{Label: "Product demonstration", Message: "I'd like to book a demo"},
		{Label: "Existing account", Message: "I'm an existing customer and need support"},
		{Label: "Security or procurement review", Message: "I need a security and procurement review"}}
	SuggestAreas = []contract.Chip{{Label: "Explore modules", Message: "What modules does ZoikoSuite include?"}, {Label: "Pricing and trial", Message: "What plans are available, and is there a free trial?"}, {Label: "Security and trust", Message: "How is data secured?"}}
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
var TplAbstainCertification = Template{contract.Abstain, "I can't confirm a certification status from an approved trust claim in this chat, and readiness activity for a security framework is not the same as an independent certification. For due diligence, please review the current Trust Center or request scoped evidence.", "trust", false, contract.ReasonNoEvidence}

// TplDemoAccepted follows "yes" to a demo offer (Doc 02 S10): the official
// form is opened; nothing is booked, scheduled or submitted from the chat.
var TplDemoAccepted = Template{contract.Handoff, "Here is ZoikoSuite's official demo request form. Nothing has been arranged from this chat: the team receives your request only when you complete and send the form yourself.", "demo", false, contract.ReasonRouteOnly}

// RouteTemplates are route-only intents: the assistant offers an official
// link and never claims a booking, ticket or callback exists (Doc 09 §11).
var RouteTemplates = map[string]Template{
	"I08": {contract.Handoff, "You can request a demo through ZoikoSuite's official demo page. I haven't booked anything for you.", "demo", false, ""},
	"I22": {contract.Handoff, "For account-specific information, please sign in through the official ZoikoSuite sign-in page. Do not share passwords, one-time codes or account records here.", "sign-in", false, ""}, // Doc 02 T17
	// Doc 02 T30: an accessibility concern is always routed, never answered
	// from help-centre text (it is feedback, not a product question).
	"I27": {contract.Handoff, "Thank you for raising an accessibility concern. Please use the official accessibility feedback route so the team can review the affected page and interaction. You do not need to share sensitive personal details in this chat.", "support", false, ""},
	// Doc 02 T16 (I21/I23) copy.
	"I23": {contract.Handoff, "I can help you find public guidance, but I cannot view your workspace or case history from this chat. Please use the secure Support Center to access the channels available to your account and plan.", "support", false, ""},
	"I24": {contract.Handoff, "I can direct you to the appropriate ZoikoSuite team. Is your enquiry about a product demonstration, an existing account, or security/procurement review?", "support", true, ""}, // Doc 02 T26
	// Doc 02 I26: ZoikoSuite facts only, never claims about other vendors.
	"I26": {contract.Abstain, "I can only describe ZoikoSuite, not other vendors' products. You can explore ZoikoSuite's modules to compare them with your requirements.", "core_modules", false, ""},
	"I28": {contract.Handoff, "I can't see billing details. Please use your ZoikoSuite account or the official support page for billing questions.", "support", false, ""},
}

// Support-channel replies (Doc 02 S06, T28): no live agent or ticket is
// promised from the chat.
var (
	TplLiveSupport = Template{contract.Handoff, "Live assistance may depend on your plan and current channel availability. The Support Center shows the support routes available for your situation; I cannot guarantee a live agent in this chat.", "support", false, contract.ReasonRouteOnly} // T28
	TplTicket      = Template{contract.Handoff, "I can direct you to the secure request form. A ticket is created only after you submit it there.", "support", false, contract.ReasonRouteOnly}                                                                                                  // S06
)

var (
	liveSupportRe = regexp.MustCompile(`(?i)\blive (?:chat|agent|specialist|person|support)\b|\b24\s*(?:/\s*7|hours?)\b|\baround the clock\b|\b(?:phone|chat) support\b|\bconnect me to (?:a |an )?(?:agent|person|human|someone)\b`)
	ticketRe      = regexp.MustCompile(`(?i)\b(?:raise|open|create|log|submit|file)\s+(?:a\s+|an\s+)?(?:support\s+)?(?:ticket|case|request)\b`)
)

// SupportTemplate picks the S06/T28 support reply for a support question, if
// one applies.
func SupportTemplate(text string) (Template, bool) {
	switch {
	case liveSupportRe.MatchString(text):
		return TplLiveSupport, true
	case ticketRe.MatchString(text):
		return TplTicket, true
	}
	return Template{}, false
}

// TplSecurityReview routes a security or procurement review (Doc 02 §22
// Trust chip, CTA "Start security review"); no evidence is supplied here.
var TplSecurityReview = Template{contract.Handoff, "Security and procurement reviews run through the official Trust Center, where you can request scoped evidence for the deployment you are evaluating. I can describe ZoikoSuite's published security capabilities here, but I cannot supply review evidence in this chat.", "trust", false, contract.ReasonRouteOnly}

// SecurityReviewRe asks to start a security or procurement review.
var SecurityReviewRe = regexp.MustCompile(`(?i)^\s*(?:start (?:a )?)?security review\s*[.!?]?\s*$|\b(?:start|request|run|begin|arrange) (?:a |an )?(?:security|procurement|vendor) (?:review|assessment|questionnaire)\b|\bsecurity (?:and|&) procurement review\b`)

// TplPartnership routes partner, reseller and referral interest (Doc 10
// COM-10): the official contact route; nothing is recorded here.
var TplPartnership = Template{contract.Handoff, "Partnership enquiries, including integration, referral and reseller interest, go through ZoikoSuite's official contact route, where you can choose the partnership purpose. Nothing is recorded or sent from this chat.", "support", false, contract.ReasonRouteOnly}

// PartnershipRe marks partner, reseller or referral interest.
var PartnershipRe = regexp.MustCompile(`(?i)\b(?:become|be|becoming) (?:an? )?(?:integration |technology |referral |reseller |channel |agency )?partners?\b|\bpartnership\b|\breseller\b|\breferral partner\b`)

// TplNoTools refuses browsing and tool use (Doc 07 QA-T041, Doc 04 §07: the
// model has no tools).
var TplNoTools = Template{contract.RefusedAction, "I can't browse the web, call other systems or use external tools. I answer only from approved, public ZoikoSuite information.", "", false, contract.ReasonActionDenied}

// ToolRequestRe asks the assistant to browse, search the web or run a tool.
var ToolRequestRe = regexp.MustCompile(`(?i)\b(?:search|browse|google|look up) (?:the )?(?:web|internet|online)\b|\b(?:use|run|call|invoke) (?:an? )?(?:external |third-party )?(?:tool|plugin|api|function)s?\b|\bweb search\b` +
	// QA-T071 paraphrases: "Google this for me", "Fetch this URL and tell me what it says".
	`|\bgoogle (?:this|that|it)\b|\b(?:fetch|open|visit|read|load|scrape|crawl) (?:this|that|the|a) (?:url|link|website|web ?page)\b`)

// TplCredentialShared answers a visitor who says they shared a secret
// (Doc 06 §20, Doc 02 T22) without the secret itself being in the message.
var TplCredentialShared = ExceptionTemplates[X03SensitiveInput]

// CredentialSharedRe: "I pasted my bank password", "I shared my PIN".
// QA-T071 adds "I just typed…", "I accidentally shared my bank login…",
// "just pasted my bank pwd" and "my credentials are in my last message".
var CredentialSharedRe = regexp.MustCompile(`(?i)\b(?:i|we)(?:'ve| have)?\s+(?:just\s+|accidentally\s+|mistakenly\s+|also\s+)?(?:pasted|shared|typed|sent|posted|entered|included|put|gave|wrote)\b.{0,40}\b(?:password|passcode|pin|pwd|otp|one-time code|card number|bank details|bank login|banking login|login details|credentials?|api key|token)\b` +
	`|^\s*(?:just\s+|oops,?\s+)?(?:pasted|typed|shared)\b.{0,30}\b(?:password|passcode|pwd|pin|credentials?|login details)\b` +
	`|\b(?:password|passcode|pwd|credentials?|login details)\s+(?:is|are)\s+in\s+(?:my|the)\s+(?:last|previous|earlier)\s+message\b`)

// TplSessionLimit: the conversation reached its per-session token cap
// (Doc 06 §13). Only fixed content is served here; a new conversation has a
// fresh cap. Not an outage, so not T24.
var TplSessionLimit = Template{contract.Abstain, "This conversation has reached its length limit, so I can't add new answers here. Start a new conversation from the menu to keep asking; your earlier answers stay in your chat history.", "", false, contract.ReasonSpendCeiling}

// TplUptime is Doc 02 T27: no uptime guarantee or SLA is inferred.
var TplUptime = Template{contract.Abstain, "I cannot confirm a contractual uptime guarantee from the public information currently available. An agreed SLA, if offered, must be evidenced in the applicable approved contract.", "trust", false, contract.ReasonNoApprovedInfo}

// UptimeQuestionRe marks uptime, availability-percentage and SLA questions.
var UptimeQuestionRe = regexp.MustCompile(`(?i)\buptime\b|\bslas?\b|\bservice[- ]level\b|\b99(?:\.\d+)?\s?%|\bavailability (?:guarantee|commitment|target)\b`)

// TplReadiness separates readiness from certification (Doc 02 S03, §19).
var TplReadiness = Template{contract.Abstain, "Readiness for a security framework is not the same as certification. I can only state a certification when current independent evidence of issuer, scope and period is approved. The Trust Center has the current position.", "trust", false, contract.ReasonNoEvidence}

// ReadinessRe marks "readiness", "working towards" or "preparing for" a
// framework.
var ReadinessRe = regexp.MustCompile(`(?i)\breadiness\b|\bworking towards?\b|\bpreparing for (?:soc|iso)|\bready for (?:soc|iso)|\bin progress\b.*\b(?:soc|iso)`)

// TplPlanEntitlement answers "does plan X include feature Y" (Doc 01 AT-12):
// plan and configuration qualifiers and the authoritative comparison.
var TplPlanEntitlement = Template{contract.Abstain, "Whether a feature such as SSO is included depends on the plan and how it is configured. The official plan comparison shows what each plan includes; I can't confirm an entitlement for a specific account here.", "pricing", false, contract.ReasonNoApprovedInfo}

// EntitlementRe asks whether a plan includes a capability.
var EntitlementRe = regexp.MustCompile(`(?i)\b(?:sso|scim|single sign-on|saml|audit log|api access|include[sd]?|come with|entitle\w*|available on|support(?:ed)? on)\b`)

// TplNonEnglish is the English fallback for a language without approved
// content (Doc 02 §23).
var TplNonEnglish = Template{contract.Abstain, "I can currently answer in English only. Please ask your question in English, or use the Support Center for help in other languages.", "support", false, contract.ReasonOutOfScope}

var nonEnglishRe = regexp.MustCompile(`[¿¡ñáéíóúü]|\b(?:qué|cómo|cuánto|dónde|por favor|gracias|precio|empresa|quiero|necesito|puedo|est-ce|combien|wie viel|bitte|preço|quanto)\b`)

// IsNonEnglish reports a message written in another language.
func IsNonEnglish(text string) bool { return nonEnglishRe.MatchString(strings.ToLower(text)) }

// SecondarySentence is the fixed sentence for the other part of a two-part
// question whose first part is refused (Doc 02 S05, S09). It never answers.
func SecondarySentence(exception, primary, text string) string {
	switch {
	case exception == X04RegulatedAdvice && (primary == "I17" || primary == "I18"):
		return "Payroll and tax coverage for a specific country must be checked against ZoikoSuite's approved regional coverage; the team can confirm it."
	case (exception == X01PrivateAction || exception == X02ActionDenied) && exceptionRules[X07CrossProduct][0].MatchString(text):
		return "Other Zoiko products are separate, with their own capabilities and policies."
	}
	return ""
}

// DefaultRoute is the CTA offered alongside answers for each intent.
var DefaultRoute = map[string]string{
	"I01": "core_modules", "I02": "core_modules", "I03": "pricing", "I04": "demo", "I05": "pricing",
	"I06": "pricing", "I07": "pricing", "I08": "demo", "I09": "demo", "I10": "core_modules",
	"I11": "demo", "I12": "trust", "I13": "trust", "I14": "trust", "I15": "core_modules",
	"I16": "core_modules", "I17": "core_modules", "I18": "core_modules", "I19": "core_modules", "I20": "core_modules",
	"I21": "support", "I22": "sign-in", "I23": "support", "I24": "support", "I25": "core_modules",
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
