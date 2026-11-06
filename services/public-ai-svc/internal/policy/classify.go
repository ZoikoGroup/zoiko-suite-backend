package policy

import (
	"regexp"
	"strconv"
	"strings"
)

// Exception classes (Doc 02 §07), intercepted before generation.
const (
	X01PrivateAction     = "X01" // SAFETY_PRIVATE_ACTION: ledger/balance/records requests
	X02ActionDenied      = "X02" // SAFETY_ACTION_DENIED: pay, approve, export, submit
	X03SensitiveInput    = "X03" // SAFETY_SENSITIVE_INPUT
	X04RegulatedAdvice   = "X04" // SAFETY_REGULATED_ADVICE
	X05SecurityEscalate  = "X05" // SAFETY_SECURITY_ESCALATE
	X06PromptInjection   = "X06" // SAFETY_PROMPT_INJECTION
	X07CrossProduct      = "X07" // SAFETY_CROSS_PRODUCT
	X08ClaimVerification = "X08" // SAFETY_CLAIM_VERIFICATION
	X09UnsupportedCover  = "X09" // SAFETY_UNSUPPORTED_COVERAGE: absolute coverage or compatibility claims (Doc 02 §19)
	X10AbuseRate         = "X10" // SAFETY_ABUSE_RATE (set by the edge limiter)
)

// exceptionOrder is highest risk first: when several match, the first wins.
var exceptionOrder = []string{X03SensitiveInput, X06PromptInjection, X05SecurityEscalate, X02ActionDenied, X01PrivateAction, X04RegulatedAdvice, X08ClaimVerification, X09UnsupportedCover, X07CrossProduct}

// pattern compiles a phrase list into one case-insensitive regexp. A phrase
// ending in "*" matches as a word prefix; others match whole words.
func pattern(phrases ...string) *regexp.Regexp {
	parts := make([]string, len(phrases))
	for i, p := range phrases {
		if strings.HasSuffix(p, "*") {
			parts[i] = `\b` + regexp.QuoteMeta(strings.TrimSuffix(p, "*"))
		} else {
			parts[i] = `\b` + regexp.QuoteMeta(p) + `\b`
		}
		parts[i] = strings.ReplaceAll(parts[i], `\ `, `\s+`)
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(parts, "|") + `)`)
}

var exceptionRules = map[string][]*regexp.Regexp{
	X06PromptInjection: {
		pattern("ignore previous", "ignore all previous", "ignore the above", "ignore prior", "ignore your instructions", "ignore all instructions",
			"disregard previous", "disregard your", "disregard all", "system prompt", "your instructions", "your prompt", "developer mode",
			"jailbreak", "you are now", "pretend you are", "pretend to be", "act as the system", "act as an admin", "act as a developer",
			"override your", "reveal your", "print your", "new instructions", "do anything now",
			// Doc 01 AT-06, Doc 02 §26: "ignore rules" and role spoofing.
			"ignore rules", "ignore the rules", "ignore your rules", "ignore all rules", "ignore your guidelines",
			"i am your administrator", "i am your admin", "i'm your administrator", "i'm your admin", "i am the administrator",
			"i am an administrator", "as your administrator", "i am the system administrator", "i am a zoikosuite admin*",
			"ignore instructions", "ignore policy", "ignore the policy", "ignore your policy", "ignore policies", "ignore public only",
			"ignore restrictions", "ignore the restrictions", "ignore safety", "bypass the filter", "bypass your", "without restrictions"),
		regexp.MustCompile(`(?i)</?\s*(system|assistant|tool)\s*>|\[\s*system\s*\]|###\s*(system|instruction)|BEGIN SYSTEM`),
		// Doc 06 §08/§25, QA-T040: the same instructions in other languages
		// (Spanish, French, German, Portuguese, Italian, Dutch).
		regexp.MustCompile(`(?i)\b(?:ignora|ignore|ignorez|ignoriere|ignorieren|negeer|olvida|oublie|oubliez|vergiss|esque[cç]a|dimentica)\b.{0,30}\b(?:instrucciones|instructions|anweisungen|instru[cç][oõ]es|istruzioni|instructies|reglas|r[eè]gles|regeln|regras|regole|regels)\b` +
			`|\b(?:prompt|mensaje|invite)\s+(?:del|de|du)\s+(?:sistema|syst[eè]me)\b|\bsystemprompt\b|\bsystem-prompt\b` +
			`|\b(?:muestra|revela|montre|r[eé]v[eè]le|zeige|verrate|mostra|rivela)\b.{0,25}\b(?:instrucciones|instructions|anweisungen|prompt|istruzioni)\b`),
	},
	X05SecurityEscalate: {
		pattern("vulnerability", "vulnerabilities", "found a bug in your security", "security flaw", "exploit", "data breach",
			"was hacked", "been hacked", "been compromised", "was compromised", "security breach", "security incident", "been breached", "were breached", "under attack", "ransomware", "cyber attack", "cyberattack", "phishing", "report a security", "responsible disclosure", "bug bounty",
			"security concern", "security issue to report", "report a vulnerability",
			// Doc 02 §26 paraphrases (G20).
			"being breached", "breached", "hacked", "breach in progress"),
	},
	X01PrivateAction: {
		livePlatformRe,
		regexp.MustCompile(`(?i)\b(?:my|our)\s+(?:\w+\s+){0,2}(?:invoices?|balances?|ledgers?|payroll|payslips?|pay ?stubs?|transactions?|bank statements?|tax returns?|journals?|employees?|staff records?|salar(?:y|ies)|payments?|accounts? receivable|accounts? payable|cash position|expenses?|bills?|vendors?|customers?|records?|filings?)\b`),
		pattern("show me my", "look up my", "check my account", "what is my balance", "what's my balance", "access my data", "pull my data", "in my account", "my tenant",
			"private records", "all tenants", "other tenants", "every tenant", "all customers' data", "other customers' data", "customer records",
			"status of my", "status of our", "my support account", "my subscription status"),
	},
	X02ActionDenied: {
		regexp.MustCompile(`(?i)\b(?:pay|approve|reject|submit|file|export|delete|transfer|send|wire|refund|cancel|run|post|reconcile|issue)\s+(?:(?:my|our|the|this|these|that|a|an|all)\s+)?(?:\w+\s+){0,2}(?:invoices?|bills?|payments?|payroll|tax(?:es)?|returns?|filings?|data|accounts?|subscriptions?|money|funds|refunds?|journals?|entr(?:y|ies)|employees?|salar(?:y|ies)|wages)\b`),
		pattern("do it for me", "make the payment", "process the payment", "execute the transfer", "make a payment", "make a transfer",
			"make a bank transfer", "bank transfer for me", "send money", "move money", "wire money", "transfer money", "transfer funds"),
	},
	X04RegulatedAdvice: {
		pattern("tax advice", "legal advice", "accounting advice", "financial advice", "investment advice", "employment law advice",
			"should i deduct", "can i deduct", "should i claim", "can i claim", "should i file", "how much tax should", "how much tax do i",
			"how do i avoid tax", "reduce my tax", "is it legal", "am i compliant", "are we compliant", "should i invest", "can i fire",
			"tax liability", "how much tax do i owe", "legal guarantee", "deductible", "tax deductible",
			// Doc 02 S05: pay or rate advice is advice, whatever else is asked.
			"what should i pay", "how much should i pay", "how much should we pay", "what should we pay", "what rate should i",
			// Spanish (G33): regulated-advice requests are refused whatever the language.
			"asesor*", "despedir", "despido", "consejo legal", "consejo fiscal", "impuestos debo", "debo pagar", "cuánto debo", "cuanto debo",
			"qué debo pagar", "que debo pagar", "debo declarar", "cuántos impuestos", "cuantos impuestos"),
		// "What should my German VAT return say?" asks for the visitor's own filing (G12/G19 paraphrases).
		regexp.MustCompile(`(?i)\bwhat\s+should\s+(?:my|our)\s+(?:\w+\s+){0,2}(?:tax|vat|gst|returns?|filings?|declarations?)\b`),
	},
	X08ClaimVerification: {
		pattern("guarantee", "guaranteed", "promise me", "confirm in writing", "legally binding", "put it in writing", "are you sure it is free",
			"promise in writing", "promise that", "in writing that"),
	},
	// Doc 02 §19: absolute coverage, compatibility or free-forever claims are
	// never confirmed; the reply qualifies and refers (SAFETY_UNSUPPORTED_COVERAGE).
	X09UnsupportedCover: {
		pattern("100% compliant", "fully compliant", "compliant in every country", "compliant everywhere", "every country", "all countries",
			"every jurisdiction", "all jurisdictions", "worldwide", "every erp", "all erps", "any erp", "every accounting system",
			"every bank", "all banks", "every system", "free forever", "forever free", "unlimited free", "unlimited trial",
			"encrypted everywhere", "enterprise-grade encrypt*", "encrypt everything"),
	},
	X07CrossProduct: {
		// Other Zoiko products named in the documents, written as one word,
		// two words or hyphenated (ZoikoOne, Zoiko One, Zoiko-Tech).
		regexp.MustCompile(`(?i)\bzoiko[\s-]*(one|logia|tech|mobile|telecom|contracting)\b`),
	},
}

// Intent rules (Doc 02 §06). Each phrase scores 1, or 2 for multi-word
// phrases. tieOrder resolves equal scores (more specific / higher risk first).
var intentRules = map[string]*regexp.Regexp{
	"I01": pattern("what is zoikosuite", "what can zoikosuite do", "what does zoikosuite offer", "what is zoiko suite", "what's zoikosuite", "whats zoikosuite", "what does zoikosuite do", "what does zoiko suite do",
		"what is the zoiko suite", "what is the zoikosuite", "what's the zoiko suite", "tell me about zoiko suite", "about zoiko suite", "explain zoiko suite",
		"tell me about zoikosuite", "about zoikosuite", "explain zoikosuite", "describe zoikosuite", "introduce zoikosuite", "how does zoikosuite work",
		"new to zoikosuite", "what kind of platform", "overview", "hello", "hi", "hey", "good morning", "who are you",
		"explore platform", "explore the platform", "explore capabilities",
		// Doc 02 §26 paraphrases (G01).
		"what zoikosuite is", "what zoiko suite is", "this platform about", "what is this platform", "what is this product"),
	"I02": pattern("feature*", "capabilit*", "module*", "accounts payable", "accounts receivable", "general ledger", "treasury", "procurement", "cash management", "what can you do",
		"invoice*", "bills", "vendor payment*", "supplier payment*", "payables", "receivables", "expense*"),
	"I03": pattern("multi-entity", "multi entity", "multiple entities", "entities", "subsidiar*", "consolidat*", "intercompany", "group companies",
		"entity limits", "entity limit"),
	"I04": pattern("which countries", "what countries", "available in", "jurisdiction*", "countries", "country", "markets do you", "do you support the uk", "do you operate in"),
	"I05": regexp.MustCompile(pattern("price*", "pricing", "cost*", "how much", "per user", "per month", "per year", "plans", "plan cost", "fees", "quote", "expensive", "cheap*", "compare plans", "plan comparison", "plan differences", "difference between plans", "differences between the plans").String() + `|` + amountRe.String()),
	"I06": pattern("trial", "free trial", "try it", "try for free", "test drive", "sandbox account"),
	"I07": pattern("upgrade*", "downgrade*", "switch plan", "change plan", "change my plan", "move to the growth", "add users", "remove users"),
	"I08": pattern("demo", "book a demo", "what can be covered", "what can a demo cover", "demo cover", "can someone show me", "could someone show me", "someone show me", "show me how it works", "walk me through", "schedule a demo", "see it in action", "walkthrough", "product tour"),
	"I09": pattern("enterprise quote", "custom quote", "enterprise pricing", "enterprise", "quote for", "discounted enterprise", "large organization", "large organisation", "sso", "scim", "solutions architect", "custom contract", "volume discount", "procurement team"),
	"I10": pattern("integrat*", "connector*", "connect to", "connect with", "api", "apis", "webhook*", "quickbooks", "salesforce", "sync with", "bank feeds"),
	"I11": pattern("migrat*", "switch from", "move from", "moving from", "import our data", "import data", "data import", "onboarding data"),
	"I12": pattern("soc 2", "soc2", "iso 27001", "iso27001", "certif*", "secur*", "encrypt*", "penetration test", "pentest", "trust center", "compliance report", "mfa", "single sign-on"),
	"I13": pattern("privacy", "gdpr", "dpa", "data processing agreement", "personal data", "data protection", "ccpa", "subprocessor*", "delete my data"),
	"I14": pattern("data residency", "where is the data stored", "where is my data", "where is data stored", "data stored", "hosted", "hosting", "data center", "data centre", "eu data", "uk data", "never leave*", "data leave*", "leave the country", "data stay*", "stays in the", "data sovereignty", "data location", "deploy*", "on-premise", "on premise", "on-prem", "self-host*", "private cloud"),
	"I15": pattern("governance", "approval workflow*", "approvals", "run approvals", "approve automatically", "approvals automatically", "automatic approval*", "auto-approv*", "approve on behalf", "on behalf of", "who can approve", "approval limit*", "act for", "audit trail", "segregation of duties", "controls", "policy engine"),
	"I16": pattern("evidence", "audit evidence", "audit-ready", "audit ready", "auditor*", "audit log*", "audit trail evidence", "admissib*", "assurance"),
	"I17": pattern("tax", "taxes", "taxation", "file tax*", "tax filing*", "tax return*", "vat", "gst", "sales tax", "tax compliance", "corporate tax", "withholding"),
	"I18": pattern("payroll tax*", "payroll", "workforce", "human resources", "hr", "paye", "benefits", "leave management", "employee onboarding"),
	"I19": pattern("contract*", "legal workflow*", "clause*", "board resolution*", "legal module", "obligations"),
	"I20": pattern("report*", "dashboard*", "analytics", "forecast*", "insights", "kpi*"),
	// "how do i" / "how to" are openers, not a topic: they no longer outscore
	// the topic word ("How do I migrate…" is I11).
	"I21": pattern("help", "documentation", "docs", "guide", "faq", "knowledge base", "tutorial"),
	"I22": pattern("log in", "login", "sign in", "sign-in", "signin", "account access", "access my account", "can't log", "cannot log", "can't sign", "cannot sign", "password reset", "reset my password", "forgot my password", "forgot password", "my account", "locked out"),
	"I23": pattern("support ticket", "help from support", "need support", "get support", "support team", "talk to support", "speak to support", "reach support", "existing customer", "my ticket", "a ticket", "submit a request", "live chat", "chat support", "phone support", "24 hours", "24/7", "around the clock", "live agent", "live specialist", "ticket status", "raise a ticket", "open a ticket", "contact support", "customer support", "not working", "bug", "error", "problem with", "issue with", "broken"),
	"I24": pattern("speak to a human", "talk to a human", "talk to a person", "speak to someone", "talk to someone", "talk to somebody", "speak to a person", "real person", "human", "agent", "representative", "talk to sales", "contact sales", "call me", "someone from sales", "sales to contact", "sales team to contact", "have sales"),
	"I25": regexp.MustCompile(pattern("coming soon", "roadmap", "release date", "when will", "is it available", "availability", "launch*", "planned").String() +
		`|(?i)\b(?:get|have|use|available|enable[ds]?|live|ready)\b[^.?!]{0,40}\b(?:today|right now|yet|currently)\b`),
	"I26": pattern("compare", "comparison", "vs", "versus", "better than", "alternative to", "competitor*", "netsuite", "sage intacct"),
	"I27": pattern("accessibility issue", "accessibility problem", "accessibility concern", "accessibility complaint", "accessibility", "screen reader", "wcag", "accessible", "keyboard navigation"),
	"I28": pattern("billing", "payment method", "receipt*", "charged", "invoice from zoikosuite", "update my card", "billing address"),
	"I30": pattern("responsible ai", "can ai", "does ai", "will ai", "how do you use ai", "use ai", "uses ai", "artificial intelligence", "ai features", "ai model", "which model", "training data", "train on my data", "trained on", "hallucinat*", "are you an ai", "are you a bot"),
}

var tieOrder = []string{"I22", "I24", "I12", "I13", "I14", "I05", "I06", "I08", "I09", "I10", "I11", "I17", "I18", "I19", "I03", "I04", "I07", "I15", "I16", "I20", "I25", "I26", "I27", "I28", "I30", "I23", "I21", "I02", "I01"}

// Slots extracted from the visitor's own words (never from IP, Doc 02 §09).
type Slots struct {
	MarketCode     string
	PlanName       string // lower-case plan word as written; mapped to plan_code by the claim registry
	BillingCadence string // monthly | annual
	EntityBand     string // 1 | 2-5 | 6+
	Integration    string
	// Doc 02 §09 optional context slots (enums only; see slots.go).
	Persona       string
	Workstream    string
	SupportTopic  string
	Jurisdiction  string
	HandoffIntent string
}

// Decision is the policy outcome for one message.
type Decision struct {
	// Confidence is a coarse bucket of how strongly the message matched its
	// intent (Doc 02 §27 intent_classified): high, medium, low or none.
	Confidence string
	Exception  string // X0n, or "" when none applies
	Primary    string // I01–I30
	Secondary  string
	Slots      Slots
	Redaction  Redaction
}

var (
	greetingRe = regexp.MustCompile(`(?i)^\s*(hi|hello|hey|hiya|good (morning|afternoon|evening)|thanks|thank you|thx)(\s+there)?[\s!.,]*$`)
	priceRe    = regexp.MustCompile(pattern("how much", "cost", "costs", "price*", "per user", "per month", "per year", "fee", "fees", "quote", "expensive", "cheap*", "charge*").String() + `|` + amountRe.String())
)

// amountRe is a currency amount ("$10", "€ 49", "USD 20"): a price
// question even without the word "price" (Doc 01 AT-04, Doc 02 G35).
var amountRe = regexp.MustCompile(`(?i)[$€£]\s?\d|\b(?:usd|eur|gbp|xts)\s?\d`)

// IsGreeting reports whether the whole message is a greeting or thanks.
func IsGreeting(text string) bool { return greetingRe.MatchString(text) }

var overviewRe = regexp.MustCompile(`(?i)^\s*(?:` + regexp.QuoteMeta("GENERAL_OVERVIEW") + `|show (?:me )?(?:a |the )?general overview|general overview)\s*[.!]?\s*$`)

var otherMarketRe = regexp.MustCompile(`(?i)^\s*(?:OTHER|other(?: country| market)?|another country|somewhere else|none of these|a different country)\s*[.!]?\s*$`)

// IsOtherMarket reports whether the visitor chose "Other" (a country not
// offered as a choice) in answer to the market question.
func IsOtherMarket(text string) bool { return otherMarketRe.MatchString(text) }

// livePlatformRe catches questions about live platform state, tenant data or
// dashboard counts. The public assistant cannot see any of it (X01).
var livePlatformRe = regexp.MustCompile(`(?i)\bactive\s+(?:micro)?services?\b|\bmicroservices?\b.*\b(?:active|running|up|down|ready|status|live)\b|\bhow many\b.*\b(?:services?|domains?|tenants?|users?|entities)\b.*\b(?:ready|running|active|up|live|do (?:i|we) have|are there in my)\b|\bservices?\s+(?:are\s+)?(?:ready|running|down|healthy)\b|\b(?:show|list|open)\s+(?:me\s+)?(?:my|our)\s+(?:domains?|dashboard|tenants?|services?|entities|workspace)\b|\bcluster\s+(?:readiness|status|health)\b|\b(?:live|current|platform|service|system)\s+status\b|\baction required\b|\bprobes?\s+responding\b|\b(?:domains?|services?|microservices?|cluster|platform)\s+(?:status|health|readiness|uptime)\b|\b(?:domains?\s+monitored|operational\s+status|needs\s+attention|services?\s+ready)\b`)

// IsLivePlatform reports whether a message asks about live platform state,
// tenant data or dashboard counts.
func IsLivePlatform(text string) bool { return livePlatformRe.MatchString(text) }

// moduleTermRe names ZoikoSuite areas, modules and their aliases. A question
// that names one is never "out of scope" (classifier fallback ladder).
var moduleTermRe = regexp.MustCompile(`(?i)\b(?:finance(?: and payments)?|payments?|legal|contracts?|commercial ops|commercial operations|purchase requests?|purchase orders?|procurement|requisitions?|tax(?:es)?|vat|gst|compliance|evidence|delegated authority|delegation of authority|board resolutions?|corporate actions?|intelligence|reporting|security|trust|integrations?|connectors?|banking|hris|e-?signatures?|siem|mtls|cap tables?|ar|ap|gl|general ledger|payables|receivables|payroll|workforce|hr|modules?` +
	// Console areas of My Work and Identity, Scope & Foundation (KB articles
	// kb-my-work, kb-identity-context, kb-identity-foundation).
	`|my work|tasks?(?: and approvals)?|approvals?|exceptions?|assigned actions?|identity(?: context)?|authentication|federation|federated|sign-?on|sso|mfa|second factor|sessions?|support access|password resets?` +
	`|tenants? and entities|legal entit(?:y|ies)|feature flags?|config(?:uration)?|secret vault|secrets?|gateway(?: authentication)?|notifications?|roles?(?: and access)?|access control|permission bundles?|foundation` +
	// Governance Platform and Overview console areas.
	`|jurisdictions?(?: and rules)?|rule packs?|governance log|polic(?:y|ies)|obligations?|authori[sz]ation|segregation of duties|workflows?(?: and approvals)?|executive dashboard|domain health|dashboard|filing requirements?|evidence requirements?|requirement catalog|no_requirements_defined|journals?|statement lines?|reconciliation)\b`)

// areaDefinitionRe asks what a console area is or does.
var areaDefinitionRe = regexp.MustCompile(`(?i)^\s*(?:what\s+(?:is|are|does)|what's|explain|describe|define|tell me about|use of|how does)\s+(?:the\s+)?(?:domain health|executive dashboard|dashboards?|governance log)\b`)

// ownDataRe names the visitor's own data.
var ownDataRe = regexp.MustCompile(`(?i)\b(?:my|our|mine|ours)\b`)

// consoleJurisdictionRe is a question about the Jurisdictions & Rules area
// (registering, rules, rule packs), not about which markets ZoikoSuite is
// offered in (I04).
var consoleJurisdictionRe = regexp.MustCompile(`(?i)\b(?:register(?:ing|ed)?|add(?:ing)?|creat(?:e|ing)|delet\w*|remov\w*|deactivat\w*|resolv\w*|nest\w*|rule packs?|rules|jurisdictions?\s*(?:&|and)\s*rules|legal drift|effective[- ]dated)\b`)

// IsModuleQuestion reports whether a message names a known area, module or
// alias.
func IsModuleQuestion(text string) bool {
	return moduleTermRe.MatchString(strings.ReplaceAll(text, "&", " and "))
}

// IsGeneralOverview reports whether the visitor chose "Show general overview"
// in answer to a clarifying question (Master prompt §4 step 6).
func IsGeneralOverview(text string) bool { return overviewRe.MatchString(text) }

var dataRightsRe = regexp.MustCompile(`(?i)\b(?:delete|erase|remove|export|download|access|see|get a copy of)\s+(?:all\s+)?(?:my|our)\s+(?:personal\s+)?(?:data|information|details|records)\b|\b(?:right to (?:be forgotten|erasure|access)|data subject (?:access )?request|dsar|subject access request)\b`)

// IsDataRightsRequest reports an access, export or erasure request for the
// visitor's own data (Doc 09 §12).
func IsDataRightsRequest(text string) bool { return dataRightsRe.MatchString(text) }

// AsksPrice reports whether the message asks for a price (as opposed to,
// say, which plans exist). Only price questions need a market (Doc 02 I05).
func AsksPrice(text string) bool { return priceRe.MatchString(text) }

// LooksLikeInjection reports whether text contains instruction-injection
// patterns. Ingestion uses it to quarantine poisoned knowledge (Doc 03 §09).
func LooksLikeInjection(text string) bool {
	for _, re := range exceptionRules[X06PromptInjection] {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// Classify applies sensitive-input interception, exception rules and intent
// rules. It is pure and deterministic.
// capabilityQuestion is "Do you / does it / can ZoikoSuite <verb> …" with no
// "my", "our" or "for me": a question about the product, not a request to
// act on the visitor's records (Doc 02 G12: "Tax filing in Germany" is I17).
var (
	capabilityRe = regexp.MustCompile(`(?i)^\s*(?:do you|does (?:it|zoikosuite|zoiko suite|the platform)|can (?:it|zoikosuite|zoiko suite|the platform)|is (?:it|zoikosuite) able to)\b`)
	personalRe   = regexp.MustCompile(`(?i)\b(?:my|our|mine|for me|for us)\b`)
)

func capabilityQuestion(text string) bool {
	return capabilityRe.MatchString(text) && !personalRe.MatchString(text)
}

// capabilityLeadRe ends in "can ZoikoSuite", "does it" or "do you": the
// words just before an action verb that make it a product question.
// "Can you pay…" asks the assistant to act (Doc 01 AT-02); "do you" and
// "can ZoikoSuite" ask what the product does.
var capabilityLeadRe = regexp.MustCompile(`(?i)\b(?:(?:can|could|does|do|is)\s+(?:zoikosuite|zoiko suite|it|the platform)|do\s+you)\s+(?:\w+\s+){0,1}$`)

// capabilityClause reports whether every match of an action rule is asked as
// a capability ("Can ZoikoSuite file payroll taxes in France, and what should
// I pay…" asks what the product does; the rest of the sentence is judged on
// its own, Doc 02 S05).
func capabilityClause(text string, re *regexp.Regexp) bool {
	locs := re.FindAllStringIndex(text, -1)
	for _, l := range locs {
		if personalRe.MatchString(text[l[0]:l[1]]) || !capabilityLeadRe.MatchString(text[:l[0]]) {
			return false
		}
	}
	return len(locs) > 0
}

// injectionLeadRe is an "ignore your instructions" opener in front of an
// otherwise ordinary question.
var injectionLeadRe = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:ignore|disregard|forget)\s+(?:all\s+)?(?:(?:of\s+)?(?:your|the|my|any)\s+)?(?:previous|prior|above|earlier|old)?\s*(?:instructions?|prompts?|rules|messages?)?\s*(?:and\s+(?:just\s+)?|then\s+|[,.:;-]\s*)`)

// infoRequestRe opens a request for public information.
var infoRequestRe = regexp.MustCompile(`(?i)^(?:tell me|explain|describe|what|which|how|does|do you|is|are|where|who|can you tell|could you tell|give me an overview)\b`)

// InjectionRemainder strips an injection opener from a message whose rest
// is a permitted ZoikoSuite question (Doc 02 S07: the instruction is ignored
// and recorded, the question is still answered). ok is false when the rest
// is itself an injection, an exception, or not a ZoikoSuite question:
// prompt-extraction attempts are never continued.
func InjectionRemainder(msg string) (string, bool) {
	if Classify(msg).Exception != X06PromptInjection {
		return "", false
	}
	loc := injectionLeadRe.FindStringIndex(msg)
	if loc == nil {
		return "", false
	}
	rest := strings.TrimSpace(msg[loc[1]:])
	// Only a request for information continues; "call the payments API for
	// me" is an instruction to act and stays refused (A19).
	if rest == "" || !infoRequestRe.MatchString(rest) || personalRe.MatchString(rest) || LooksLikeInjection(rest) || encodedInstruction(rest) {
		return "", false
	}
	d := Classify(rest)
	if d.Exception != "" || (d.Primary == "I29" && !IsModuleQuestion(rest)) {
		return "", false
	}
	return rest, true
}

// recordIDRe names a business record by its number or the visitor's company
// ("the invoice is 4519", "my company is ACME").
var recordIDRe = regexp.MustCompile(`(?i)\b(?:invoice|bill|account|customer|vendor|supplier|order|po|payment|transaction|employee|reference|ref|case|ticket)\s*(?:number|no\.?|#|id)?\s*(?:is\s+|:\s*)?#?\d{3,}\b|\b(?:my|our)\s+(?:company|business|firm|organi[sz]ation)(?:'s)?\s+(?:name\s+)?is\b`)

// HasRecordIdentifiers reports whether a message supplies identifiers of the
// visitor's own records. After a records refusal they are refused again and
// never repeated (Doc 02 S04).
func HasRecordIdentifiers(text string) bool { return recordIDRe.MatchString(text) }

var affirmativeRe = regexp.MustCompile(`(?i)^\s*(?:yes|yeah|yep|yup|sure|ok|okay|please|yes please|please do|sounds good|go ahead|let's do it|i'd like that|that would be great)[\s!.,]*$`)

// IsAffirmative reports a bare "yes" to an offer.
func IsAffirmative(text string) bool { return affirmativeRe.MatchString(text) }

func Classify(msg string) Decision {
	d := Decision{Redaction: Redact(msg)}
	text := d.Redaction.Text
	if d.Redaction.Found() {
		d.Exception = X03SensitiveInput
	} else if encodedInstruction(text) {
		// An instruction hidden in base64 is still an injection (Doc 06 §08).
		d.Exception = X06PromptInjection
	}
	if d.Exception == "" {
		for _, x := range exceptionOrder {
			if x == X03SensitiveInput {
				continue
			}
			for _, re := range exceptionRules[x] {
				if re.MatchString(text) && !(x == X02ActionDenied && (capabilityQuestion(text) || capabilityClause(text, re))) {
					d.Exception = x
					break
				}
			}
			if d.Exception != "" {
				break
			}
		}
	}
	if d.Exception == X01PrivateAction && areaDefinitionRe.MatchString(text) && !ownDataRe.MatchString(text) {
		// "What is Domain Health?" asks what a console area is, not for live
		// figures; "show our domain health" stays refused.
		d.Exception = ""
	}
	if d.Exception == "" && recordIDRe.MatchString(text) {
		// Doc 02 S04/G32: a record number is a request for that record, even
		// without "my" ("Can I see invoice 4519?").
		d.Exception = X01PrivateAction
	}
	d.Primary, d.Secondary, d.Confidence = classifyIntentScored(text)
	d.Slots = extractSlots(text)
	switch {
	case d.Primary == "I26" && len(planRe.FindAllString(text, -1)) > 0 && !competitorRe.MatchString(text):
		// "Compare Starter and Growth" compares ZoikoSuite plans (Doc 02 §08).
		d.Primary, d.Secondary = "I05", ""
	case d.Primary == "I04" && d.Slots.MarketCode == "" && consoleJurisdictionRe.MatchString(text):
		// "How to register a jurisdiction" is about the Jurisdictions &
		// Rules area, never a market-availability question (no country named).
		d.Primary, d.Secondary = "I02", ""
	case d.Primary == "I04" && plansRe.MatchString(text) && !AsksPrice(text):
		// "Which plans are available in my market?" is plan availability (Doc 01 AT-01).
		d.Primary = "I05"
		if d.Secondary == "I05" {
			d.Secondary = ""
		}
	}
	// "What about the UK, monthly?" with nothing else: a billing term and a
	// country only make sense for price (Doc 02 I05 slots market, billing).
	if d.Primary == "I29" && d.Slots.BillingCadence != "" && d.Slots.MarketCode != "" {
		d.Primary = "I05"
	}
	intent := d.Primary
	if d.Exception != "" {
		intent, d.Confidence = d.Exception, "high" // a rule matched outright
	}
	fillContextSlots(&d.Slots, text, intent)
	return d
}

// classifyIntentScored also returns the confidence bucket: a rule-based
// exception or a multi-word phrase match is high, two single words medium,
// one word low, no match none.
func classifyIntentScored(text string) (string, string, string) {
	scores := map[string]int{}
	for id, re := range intentRules {
		for _, m := range re.FindAllString(text, -1) {
			if strings.ContainsAny(strings.TrimSpace(m), " \t") {
				scores[id] += 2
			} else {
				scores[id]++
			}
		}
	}
	best, second := "", ""
	for _, id := range tieOrder {
		s := scores[id]
		if s == 0 {
			continue
		}
		switch {
		case best == "" || s > scores[best]:
			best, second = id, best
		case second == "" || s > scores[second]:
			second = id
		}
	}
	if best == "" {
		return "I29", "", "none"
	}
	conf := "low"
	switch {
	case scores[best] >= 3:
		conf = "high"
	case scores[best] == 2:
		conf = "medium"
	}
	return best, second, conf
}

var (
	planRe        = regexp.MustCompile(`(?i)\b(starter|growth|enterprise)\b`)
	monthlyRe     = pattern("monthly", "per month", "a month", "month-to-month")
	annualRe      = pattern("annual*", "yearly", "per year", "a year")
	entityRe      = regexp.MustCompile(`(?i)\b(\d{1,4}|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|a dozen|dozens of|several|many)\s+(?:legal\s+)?(?:entities|entity|companies|subsidiaries)\b`)
	competitorRe  = regexp.MustCompile(`(?i)\b(?:netsuite|sage|intacct|xero|quickbooks|sap|oracle|workday|dynamics|odoo|freshbooks|zoho|competitor\w*|other vendors?|alternatives?)\b`)
	plansRe       = regexp.MustCompile(`(?i)\bplans?\b`)
	integrationRe = regexp.MustCompile(`(?i)\b(?:integrat\w*|connect\w*|sync)\s+(?:with|to)\s+([A-Z][A-Za-z0-9.&-]{1,40})`)
)

// entityCount reads "5" or "five"; "several" or "dozens of" count as 6+.
func entityCount(w string) (int, error) {
	words := map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8,
		"nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "a dozen": 12, "dozens of": 24, "several": 6, "many": 6}
	if n, ok := words[strings.ToLower(w)]; ok {
		return n, nil
	}
	return strconv.Atoi(w)
}

func extractSlots(text string) Slots {
	var s Slots
	// Any ISO 3166-1 country the visitor names (countries.go), longest
	// alias first; naming a country is never coverage (claims decide that).
	s.MarketCode = MarketOf(text)
	if m := planRe.FindStringSubmatch(text); m != nil {
		s.PlanName = strings.ToLower(m[1])
	}
	switch {
	case annualRe.MatchString(text):
		s.BillingCadence = "annual"
	case monthlyRe.MatchString(text):
		s.BillingCadence = "monthly"
	}
	if m := entityRe.FindStringSubmatch(text); m != nil {
		if n, err := entityCount(m[1]); err == nil {
			switch {
			case n <= 1:
				s.EntityBand = "1"
			case n <= 5:
				s.EntityBand = "2-5"
			default:
				s.EntityBand = "6+"
			}
		}
	}
	if m := integrationRe.FindStringSubmatch(text); m != nil {
		s.Integration = m[1]
	}
	return s
}
