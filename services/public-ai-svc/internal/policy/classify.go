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
	X09UnsupportedCover  = "X09" // SAFETY_UNSUPPORTED_COVERAGE (set by the claim stage)
	X10AbuseRate         = "X10" // SAFETY_ABUSE_RATE (set by the edge limiter)
)

// exceptionOrder is highest risk first: when several match, the first wins.
var exceptionOrder = []string{X03SensitiveInput, X06PromptInjection, X05SecurityEscalate, X02ActionDenied, X01PrivateAction, X04RegulatedAdvice, X08ClaimVerification, X07CrossProduct}

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
			"override your", "reveal your", "print your", "new instructions", "do anything now"),
		regexp.MustCompile(`(?i)</?\s*(system|assistant|tool)\s*>|\[\s*system\s*\]|###\s*(system|instruction)|BEGIN SYSTEM`),
	},
	X05SecurityEscalate: {
		pattern("vulnerability", "vulnerabilities", "found a bug in your security", "security flaw", "exploit", "data breach",
			"was hacked", "been hacked", "been compromised", "was compromised", "phishing", "report a security", "responsible disclosure", "bug bounty"),
	},
	X01PrivateAction: {
		regexp.MustCompile(`(?i)\b(?:my|our)\s+(?:\w+\s+){0,2}(?:invoices?|balances?|ledgers?|payroll|payslips?|pay ?stubs?|transactions?|bank statements?|tax returns?|journals?|employees?|staff records?|salar(?:y|ies)|accounts? receivable|accounts? payable|cash position|expenses?|bills?|vendors?|customers?|records?|filings?)\b`),
		pattern("show me my", "look up my", "check my account", "what is my balance", "what's my balance", "access my data", "pull my data", "in my account", "my tenant"),
	},
	X02ActionDenied: {
		regexp.MustCompile(`(?i)\b(?:pay|approve|reject|submit|file|export|delete|transfer|send|wire|refund|cancel|run|post|reconcile|issue)\s+(?:(?:my|our|the|this|these|that|a|an|all)\s+)?(?:\w+\s+){0,2}(?:invoices?|bills?|payments?|payroll|tax(?:es)?|returns?|filings?|data|accounts?|subscriptions?|money|funds|refunds?|journals?|entr(?:y|ies)|employees?)\b`),
		pattern("do it for me", "make the payment", "process the payment", "execute the transfer"),
	},
	X04RegulatedAdvice: {
		pattern("tax advice", "legal advice", "accounting advice", "financial advice", "investment advice", "employment law advice",
			"should i deduct", "can i deduct", "should i claim", "can i claim", "should i file", "how much tax should", "how much tax do i",
			"how do i avoid tax", "reduce my tax", "is it legal", "am i compliant", "are we compliant", "should i invest", "can i fire"),
	},
	X08ClaimVerification: {
		pattern("guarantee", "guaranteed", "promise me", "confirm in writing", "legally binding", "put it in writing", "are you sure it is free"),
	},
	X07CrossProduct: {
		pattern("zoiko one", "zoikologia", "zoiko mobile", "zoiko telecom"),
	},
}

// Intent rules (Doc 02 §06). Each phrase scores 1, or 2 for multi-word
// phrases. tieOrder resolves equal scores (more specific / higher risk first).
var intentRules = map[string]*regexp.Regexp{
	"I01": pattern("what is zoikosuite", "what does zoikosuite do", "tell me about zoikosuite", "about zoikosuite", "overview", "hello", "hi", "hey", "good morning", "who are you"),
	"I02": pattern("feature*", "capabilit*", "module*", "can zoikosuite", "does zoikosuite", "accounts payable", "accounts receivable", "general ledger", "treasury", "procurement", "cash management", "what can you do"),
	"I03": pattern("multi-entity", "multi entity", "multiple entities", "entities", "subsidiar*", "consolidat*", "intercompany", "group companies"),
	"I04": pattern("which countries", "what countries", "available in", "jurisdiction*", "countries", "country", "markets do you", "do you support the uk", "do you operate in"),
	"I05": pattern("price*", "pricing", "cost*", "how much", "per user", "per month", "per year", "plans", "plan cost", "fees", "quote", "expensive", "cheap*"),
	"I06": pattern("trial", "free trial", "try it", "try for free", "test drive", "sandbox account"),
	"I07": pattern("upgrade*", "downgrade*", "switch plan", "change plan", "change my plan", "move to the growth", "add users", "remove users"),
	"I08": pattern("demo", "book a demo", "schedule a demo", "see it in action", "walkthrough", "product tour"),
	"I09": pattern("enterprise", "large organization", "large organisation", "sso", "scim", "solutions architect", "custom contract", "volume discount", "procurement team"),
	"I10": pattern("integrat*", "connector*", "connect to", "connect with", "api", "apis", "webhook*", "quickbooks", "salesforce", "sync with", "bank feeds"),
	"I11": pattern("migrat*", "switch from", "move from", "moving from", "import our data", "import data", "data import", "onboarding data"),
	"I12": pattern("soc 2", "soc2", "iso 27001", "iso27001", "certif*", "security", "encrypt*", "penetration test", "pentest", "secure", "trust center", "compliance report"),
	"I13": pattern("privacy", "gdpr", "dpa", "data processing agreement", "personal data", "data protection", "ccpa", "subprocessor*", "delete my data"),
	"I14": pattern("data residency", "where is the data stored", "where is my data", "where is data stored", "data stored", "hosted", "hosting", "data center", "data centre", "eu data", "uk data"),
	"I15": pattern("governance", "approval workflow*", "approvals", "audit trail", "segregation of duties", "controls", "policy engine"),
	"I16": pattern("evidence", "audit evidence", "audit-ready", "audit ready", "auditor*"),
	"I17": pattern("tax", "vat", "gst", "sales tax", "tax compliance", "corporate tax", "withholding"),
	"I18": pattern("payroll", "workforce", "human resources", "hr", "paye", "benefits", "leave management", "employee onboarding"),
	"I19": pattern("contract*", "legal workflow*", "clause*", "board resolution*", "legal module", "obligations"),
	"I20": pattern("report*", "dashboard*", "analytics", "forecast*", "insights", "kpi*"),
	"I21": pattern("help", "how do i", "how to", "documentation", "docs", "guide", "faq", "knowledge base", "tutorial"),
	"I22": pattern("log in", "login", "sign in", "signin", "password reset", "reset my password", "forgot my password", "forgot password", "my account", "locked out"),
	"I23": pattern("support ticket", "contact support", "customer support", "not working", "bug", "error", "problem with", "issue with", "broken"),
	"I24": pattern("speak to a human", "talk to a human", "talk to a person", "speak to someone", "real person", "human", "agent", "representative", "talk to sales", "contact sales", "call me", "live chat"),
	"I25": pattern("coming soon", "roadmap", "release date", "when will", "is it available", "availability", "launch*", "planned"),
	"I26": pattern("compare", "comparison", "vs", "versus", "better than", "alternative to", "competitor*", "netsuite", "sage intacct"),
	"I27": pattern("accessibility", "screen reader", "wcag", "accessible", "keyboard navigation"),
	"I28": pattern("billing", "payment method", "receipt*", "charged", "invoice from zoikosuite", "update my card", "billing address"),
	"I30": pattern("responsible ai", "how do you use ai", "ai model", "which model", "training data", "train on my data", "trained on", "hallucinat*", "are you an ai", "are you a bot"),
}

var tieOrder = []string{"I22", "I24", "I12", "I13", "I14", "I05", "I06", "I08", "I09", "I10", "I11", "I17", "I18", "I19", "I03", "I04", "I07", "I15", "I16", "I20", "I25", "I26", "I27", "I28", "I30", "I23", "I21", "I02", "I01"}

// Slots extracted from the visitor's own words (never from IP, Doc 02 §09).
type Slots struct {
	MarketCode     string
	PlanName       string // lower-case plan word as written; mapped to plan_code by the claim registry
	BillingCadence string // monthly | annual
	EntityBand     string // 1 | 2-5 | 6+
	Integration    string
}

// Decision is the policy outcome for one message.
type Decision struct {
	Exception string // X0n, or "" when none applies
	Primary   string // I01–I30
	Secondary string
	Slots     Slots
	Redaction Redaction
}

// Classify applies sensitive-input interception, exception rules and intent
// rules. It is pure and deterministic.
func Classify(msg string) Decision {
	d := Decision{Redaction: Redact(msg)}
	text := d.Redaction.Text
	if d.Redaction.Found() {
		d.Exception = X03SensitiveInput
	}
	if d.Exception == "" {
		for _, x := range exceptionOrder {
			if x == X03SensitiveInput {
				continue
			}
			for _, re := range exceptionRules[x] {
				if re.MatchString(text) {
					d.Exception = x
					break
				}
			}
			if d.Exception != "" {
				break
			}
		}
	}
	d.Primary, d.Secondary = classifyIntent(text)
	d.Slots = extractSlots(text)
	return d
}

func classifyIntent(text string) (string, string) {
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
		return "I29", ""
	}
	return best, second
}

var markets = []struct {
	code string
	re   *regexp.Regexp
}{
	{"US", pattern("united states", "usa", "us market", "in the us", "america")},
	{"GB", pattern("united kingdom", "uk", "britain", "great britain", "england", "scotland", "wales")},
	{"IE", pattern("ireland")},
	{"DE", pattern("germany")},
	{"FR", pattern("france")},
	{"NL", pattern("netherlands")},
	{"ES", pattern("spain")},
	{"IT", pattern("italy")},
	{"CA", pattern("canada")},
	{"AU", pattern("australia")},
	{"IN", pattern("india")},
	{"SG", pattern("singapore")},
	{"AE", pattern("uae", "united arab emirates", "dubai")},
}

var (
	planRe        = regexp.MustCompile(`(?i)\b(starter|growth|enterprise)\b`)
	monthlyRe     = pattern("monthly", "per month", "a month", "month-to-month")
	annualRe      = pattern("annual*", "yearly", "per year", "a year")
	entityRe      = regexp.MustCompile(`(?i)\b(\d{1,4})\s+(?:legal\s+)?(?:entities|entity|companies|subsidiaries)\b`)
	integrationRe = regexp.MustCompile(`(?i)\b(?:integrat\w*|connect\w*|sync)\s+(?:with|to)\s+([A-Z][A-Za-z0-9.&-]{1,40})`)
)

func extractSlots(text string) Slots {
	var s Slots
	for _, m := range markets {
		if m.re.MatchString(text) {
			s.MarketCode = m.code
			break
		}
	}
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
		if n, err := strconv.Atoi(m[1]); err == nil {
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
