package policy

import (
	"regexp"
	"strings"

	"zoiko.io/public-ai-svc/internal/contract"
)

// Doc 02 §09 optional slots. They are self-declared session context for
// wording and routing only: never an access entitlement, never inferred from
// IP, page or language, and never personal data (enums only).

type pattern2 struct {
	value string
	re    *regexp.Regexp
}

var personaRes = []pattern2{
	{"bureau", regexp.MustCompile(`(?i)\b(?:payroll|accounting|bookkeeping)\s+bureau\b|\bbureau\b`)},
	{"accountant", regexp.MustCompile(`(?i)\b(?:i'?m|i\s+am|we\s+are|we'?re)\s+(?:an?\s+)?(?:accountants?|accounting\s+firm|bookkeepers?|cpa)\b`)},
	{"finance", regexp.MustCompile(`(?i)\b(?:i'?m|i\s+am|we\s+are|we'?re)\s+(?:the\s+|an?\s+)?(?:cfo|controller|finance\s+(?:team|lead|director|manager))\b`)},
	{"it", regexp.MustCompile(`(?i)\b(?:i'?m|i\s+am|we\s+are|we'?re)\s+(?:the\s+|an?\s+)?(?:it\s+(?:team|admin|manager|lead)|cto|security\s+team)\b`)},
	{"legal", regexp.MustCompile(`(?i)\b(?:i'?m|i\s+am|we\s+are|we'?re)\s+(?:the\s+|an?\s+)?(?:legal\s+team|general\s+counsel|lawyers?|in-house\s+counsel)\b`)},
	{"business", regexp.MustCompile(`(?i)\b(?:i'?m|i\s+am|we\s+are|we'?re)\s+(?:a\s+)?(?:small\s+business|business\s+owner|founder|start-?up)\b`)},
}

// Workstreams map visitor wording to one canonical domain id (Doc 02 §09:
// "map aliases to canonical registry; no independent terminology").
var workstreamRes = []pattern2{
	{"governance", regexp.MustCompile(`(?i)\b(?:governance|approvals?|delegated\s+authority|audit(?:\s+evidence)?|evidence|controls?)\b`)},
	{"multi_entity", regexp.MustCompile(`(?i)\b(?:multi-?entity|consolidation|intercompany|subsidiar(?:y|ies))\b`)},
	{"workforce", regexp.MustCompile(`(?i)\b(?:payroll|workforce|hr|people\s+operations)\b`)},
	{"tax_compliance", regexp.MustCompile(`(?i)\b(?:tax|vat|gst|compliance\s+filings?)\b`)},
	{"legal", regexp.MustCompile(`(?i)\b(?:legal|contracts?|board\s+resolutions?)\b`)},
	{"integrations", regexp.MustCompile(`(?i)\b(?:integrations?|apis?|connectors?)\b`)},
	{"finance", regexp.MustCompile(`(?i)\b(?:accounting|ledger|accounts\s+payable|accounts\s+receivable|invoices?|bills?|close)\b`)},
}

// WorkstreamLabels are how a demo reply names the area (Doc 02 S10).
var WorkstreamLabels = map[string]string{
	"governance":     "governance, approvals and evidence",
	"multi_entity":   "multi-entity finance and consolidation",
	"workforce":      "workforce and payroll operations",
	"tax_compliance": "tax and compliance workflows",
	"legal":          "legal and contract workflows",
	"integrations":   "integrations and APIs",
	"finance":        "accounting and finance operations",
}

var supportTopicRes = []pattern2{
	{"access", regexp.MustCompile(`(?i)\b(?:log\s?in|sign\s?in|password|locked\s+out|2fa|mfa|one-time\s+code)\b`)},
	{"billing", regexp.MustCompile(`(?i)\b(?:billing|invoice\s+from\s+zoikosuite|refund|charged|subscription)\b`)},
	{"technical", regexp.MustCompile(`(?i)\b(?:bug|error|not\s+working|broken|crash(?:es|ed)?|outage|down)\b`)},
	{"security", regexp.MustCompile(`(?i)\b(?:vulnerabilit(?:y|ies)|security\s+(?:issue|concern|incident)|breach)\b`)},
}

func firstMatch(ps []pattern2, text string) string {
	for _, p := range ps {
		if p.re.MatchString(text) {
			return p.value
		}
	}
	return ""
}

// handoffIntents is the Doc 02 §09 handoff_intent for route intents.
var handoffIntents = map[string]string{
	"I08": "demo", "I09": "sales", "I07": "sales", "I13": "security", "I16": "security",
	"I21": "support", "I22": "support", "I23": "support", "I24": "support", "I27": "support", "I28": "support",
	X05SecurityEscalate: "security",
}

// regulated intents need a jurisdiction to make a firm statement (Doc 02 §09).
var jurisdictionIntents = map[string]bool{"I04": true, "I17": true, "I18": true, "I19": true}

// fillContextSlots sets the optional §09 slots once the intent is known.
func fillContextSlots(s *Slots, text, intent string) {
	s.Persona = firstMatch(personaRes, text)
	s.Workstream = firstMatch(workstreamRes, text)
	if intent == "I21" || intent == "I22" || intent == "I23" || intent == "I24" || intent == "I28" {
		s.SupportTopic = firstMatch(supportTopicRes, text)
	}
	if jurisdictionIntents[intent] {
		// Only a country the visitor named (never IP, currency or language).
		s.Jurisdiction = s.MarketCode
	}
	s.HandoffIntent = handoffIntents[intent]
}

// DemoAreaChips are the optional I08 qualification answers (Doc 01 FR-04,
// Doc 02 I08 "audience, area (optional)").
var DemoAreaChips = []contract.Chip{
	{Label: "Governance & approvals", Message: "A demo focused on governance and approvals"},
	{Label: "Multi-entity finance", Message: "A demo focused on multi-entity finance"},
	{Label: "Workforce & payroll", Message: "A demo focused on workforce and payroll"},
	{Label: "Integrations", Message: "A demo focused on integrations"},
}

var rescheduleRe = regexp.MustCompile(`(?i)\b(?:reschedul\w*|cancel\w*|move|change)\b.{0,20}\b(?:demo|meeting|booking|call)\b`)

// DemoReply is the I08 reply: the area the visitor named (Doc 02 S10), else
// the route with one optional qualification question. A reschedule or
// cancellation gets the plain route (Doc 10 COM-05), never a question.
func DemoReply(text, workstream string) (Template, bool) {
	if rescheduleRe.MatchString(text) {
		return RouteTemplates["I08"], false
	}
	if label := WorkstreamLabels[workstream]; label != "" {
		return Template{RouteTemplates["I08"].Status, "A tailored demo can focus on " + label + ". The secure demo form lets you choose that topic and enter your contact details there, not in this chat. Nothing is booked until you send the form.", "demo", false, ""}, false
	}
	return TplDemoQualify, true
}

// TplDemoQualify asks the one optional question (Doc 02 §09: at most one).
var TplDemoQualify = Template{RouteTemplates["I08"].Status, "You can request a demo through ZoikoSuite's secure demo form; I haven't booked anything for you. If you like, tell me which area the demo should focus on and I'll note it for the form.", "demo", false, ""}

// GroundedQualifier is the fixed Doc 02 §17 qualifier line a grounded answer
// for these intents always carries, by template id, so the conditions are
// stated in approved wording rather than left to the model.
type GroundedQualifier struct {
	ID, Text string
	// Question is the template's follow-up question, asked only when the
	// visitor has not already answered it (Doc 02 §09: ask only when it
	// changes the answer).
	Question string
}

var GroundedQualifiers = map[string]GroundedQualifier{
	"I01": {"T02", "The precise capabilities available depend on market, plan and deployment status.", ""},
	"I02": {"T03", "", "Which area would you like to explore: accounting, workforce, governance, or integrations?"},
	"I03": {"T04", "Whether a particular consolidation or intercompany workflow is available for your organization depends on the approved plan and implementation scope.", ""},
	"I06": {"T06", "Availability, entitlements and post-trial conditions depend on your market and current terms.", ""},
	"I10": {"T08", "I cannot confirm a specific connector or version unless it is in the current approved integration matrix.", "Which system are you considering?"},
	"I11": {"T09", "A scoped review is needed before any timeline or compatibility commitment.", ""},
	"I17": {"T12", "Filing capabilities and supported jurisdictions are not universal; I can point you to current published coverage, not provide tax advice.", ""},
	"I18": {"T13", "Support for a particular payroll activity, filing or employment rule depends on the jurisdiction, configuration and released coverage.", ""},
	"I15": {"T15", "The exact enforcement model and available workflows depend on the configured role and product scope.", ""},
}

// discoveryRe is an open capability question ("what modules ...?"), the
// only kind T03's "which area?" follows; a named capability is not asked again.
var discoveryRe = regexp.MustCompile(`(?i)\b(?:modules?|capabilit(?:y|ies)|features?|what\s+can\s+(?:zoikosuite|it|you)\s+do|what\s+does\s+zoikosuite\s+(?:do|include|offer))\b`)

// WithQualifier appends the intent's qualifier (and its question when the
// visitor has not answered it) unless the answer already says it or the
// result would exceed maxWords. It returns the template id it used.
func WithQualifier(intent, question, text string, s Slots, maxWords int) (string, string) {
	q, ok := GroundedQualifiers[intent]
	if !ok {
		return text, ""
	}
	low := strings.ToLower(text)
	var add []string
	if q.Text != "" && !strings.Contains(low, strings.ToLower(strings.TrimRight(q.Text, ".?"))) {
		add = append(add, q.Text)
	}
	ask := q.Question != "" && !strings.Contains(low, strings.ToLower(q.Question))
	switch intent {
	case "I02":
		ask = ask && s.Workstream == "" && discoveryRe.MatchString(question)
	case "I10":
		ask = ask && s.Integration == ""
	}
	if ask {
		add = append(add, q.Question)
	}
	if len(add) == 0 {
		return text, ""
	}
	extra := strings.Join(add, " ")
	if len(strings.Fields(text))+len(strings.Fields(extra)) > maxWords {
		return text, ""
	}
	return strings.TrimSpace(text) + "\n\n" + extra, q.ID
}
