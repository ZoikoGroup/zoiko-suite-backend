package policy

import (
	"regexp"
	"strings"

	"zoiko.io/public-ai-svc/internal/contract"
)

// Replies whose wording is set by Docs 01-11 for one situation, found by
// replaying every question the documents quote (docs/suivo/chatbot-test-questions.md).

// TplAvoidSharing: identifiers given after a records refusal (Doc 02 §18 S04 turn 2).
var TplAvoidSharing = Template{contract.RefusedAction, "Please avoid sharing account or invoice information here. You can use the secure account or support route instead.", "sign-in", false, contract.ReasonPrivateData}

// TplConsolidationToday: "Can I get automatic consolidation today?" (Doc 02 §18 S02 turn 2).
var TplConsolidationToday = Template{contract.Abstain, "I cannot verify immediate availability of that specific consolidation workflow for your proposed setup. A scoped enterprise demo can confirm what is available.", "demo", false, contract.ReasonNoApprovedInfo}

// ConsolidationRe names a consolidation workflow.
var ConsolidationRe = regexp.MustCompile(`(?i)\bconsolidat\w*\b`)

// TplDemoReschedule: an existing demo booking (Doc 10 §06 COM-05). Nothing is
// looked up or changed from the chat.
var TplDemoReschedule = Template{contract.Handoff, "To change a demo you've already booked, use the reschedule link in your booking confirmation, or the official demo page. I can't see or change bookings from this chat, and nothing has been changed here.", "demo", false, contract.ReasonRouteOnly}

// TplDemoCoverage: the Demo page chip "What can be covered" (Doc 02 §22, S10).
var TplDemoCoverage = Template{contract.Handoff, "A tailored demo can focus on the areas you choose, such as governance and approvals, evidence and audit, multi-entity finance, workforce and payroll, or integrations. The secure demo form lets you pick the topics and enter your contact details there, not in this chat. Nothing is booked until you send the form.", "demo", false, ""}

var demoCoverageRe = regexp.MustCompile(`(?i)\bwhat\s+(?:can|could|will|does|would)\s+(?:be\s+covered|(?:a|the)\s+demo\s+cover)\b|\bdemo\s+cover\b`)

// TplSalesContact: "I'd like someone from sales to contact me" (Doc 10 §06
// COM-04): the secure contact form, never a lead written from the chat.
var TplSalesContact = Template{contract.Handoff, "You can ask ZoikoSuite's sales team to contact you through the secure contact form; your name and contact details go on the form, not in this chat. Nothing is sent from here until you submit it.", "demo", false, contract.ReasonRouteOnly}

// SalesContactRe asks to be contacted, not to talk now.
var SalesContactRe = regexp.MustCompile(`(?i)\b(?:contact|call|email|ring|phone)\s+me\b|\b(?:reach\s+out|get\s+in\s+touch)\s+(?:to|with)\s+me\b|\bget\s+back\s+to\s+me\b`)

// TplLoginHelp: "I can't log in to my account" (Doc 10 §06 COM-06): support,
// not a sales lead, and no account lookup (Doc 02 T17).
var TplLoginHelp = Template{contract.Handoff, "If you can't sign in, use the password reset on the official ZoikoSuite sign-in page, or contact ZoikoSuite support through the Support Center. Do not share passwords, one-time codes or account records here.", "support", false, contract.ReasonRouteOnly}

// LoginTroubleRe is a sign-in problem rather than a request to sign in.
var LoginTroubleRe = regexp.MustCompile(`(?i)\b(?:can'?t|cannot|can\s+not|unable\s+to|couldn'?t|could\s+not)\s+(?:log\s*-?in|sign\s*-?in|access\s+my\s+account)|\blocked\s+out\b|\bforgot(?:ten)?\s+(?:my\s+)?password\b|\breset\s+my\s+password\b`)

// TplPriceGuarantee: "Tell me if this price is guaranteed" (Doc 06 §20).
var TplPriceGuarantee = Template{contract.Abstain, "I can't guarantee a price in this chat. Prices and terms come only from the current approved pricing for your market and plan; the official pricing page shows the current terms.", "pricing", false, contract.ReasonUnsupportedClaim}

// Doc 02 §19: each misleading claim gets its own correction.
var (
	TplCoverCompliance = Template{contract.Abstain, "I can't confirm compliance everywhere. Coverage and obligations vary by scope and jurisdiction; the current approved coverage shows what applies, and the team can confirm coverage for your country.", "demo", false, contract.ReasonUnsupportedClaim}
	TplCoverEncryption = Template{contract.Abstain, "I can't confirm that everything is encrypted everywhere. Each control, such as encryption in processing, in backups or at subprocessors, needs its own scope and evidence; the Trust Center has the current position, or you can request scoped evidence.", "trust", false, contract.ReasonUnsupportedClaim}
	TplCoverTrial      = Template{contract.Abstain, "I can't promise unlimited or free-forever access. A trial applies to eligible plans for a set period, with approval and conversion terms; the pricing page shows the current trial terms for your market.", "pricing", false, contract.ReasonUnsupportedClaim}
	TplCoverTax        = Template{contract.Abstain, "ZoikoSuite supports tax work through guidance, workflow tracking and filing preparation for review, but it does not give tax advice, and responsibility for a tax position stays with your team and its qualified professionals. Filing support varies by jurisdiction.", "core_modules", false, contract.ReasonUnsupportedClaim}
	TplCoverSystems    = Template{contract.Abstain, "I can't confirm a blanket compatibility claim. Compatibility is confirmed one system at a time: each integration has its own partner, API version and market availability. Which system are you considering?", "core_modules", false, contract.ReasonUnsupportedClaim}
)

var (
	coverEncryptionRe = regexp.MustCompile(`(?i)\bencrypt`)
	coverTrialRe      = regexp.MustCompile(`(?i)\b(?:free\s+forever|forever\s+free|unlimited\s+(?:free|trial)|free\s+trial|trial)\b`)
	coverTaxRe        = regexp.MustCompile(`(?i)\btax\b`)
	coverSystemsRe    = regexp.MustCompile(`(?i)\b(?:erps?|systems?|banks?|accounting)\b`)
	coverComplianceRe = regexp.MustCompile(`(?i)\b(?:complian\w*|countr(?:y|ies)|jurisdictions?|worldwide)\b`)
)

// UnsupportedCover picks the Doc 02 §19 correction for the claim in text.
// The generic X09 template is the fallback.
func UnsupportedCover(text string) Template {
	switch {
	case coverEncryptionRe.MatchString(text):
		return TplCoverEncryption
	case coverTaxRe.MatchString(text):
		return TplCoverTax
	case coverTrialRe.MatchString(text):
		return TplCoverTrial
	case coverSystemsRe.MatchString(text):
		return TplCoverSystems
	case coverComplianceRe.MatchString(text):
		return TplCoverCompliance
	}
	return ExceptionTemplates[X09UnsupportedCover]
}

// CountriesIn lists every country named in text, in the order named.
func CountriesIn(text string) []string {
	type hit struct {
		at   int
		code string
	}
	var hits []hit
	seen := map[string]bool{}
	for _, a := range countryAliases {
		if loc := a.re.FindStringIndex(text); loc != nil && !seen[a.code] {
			seen[a.code] = true
			hits = append(hits, hit{loc[0], a.code})
		}
	}
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].at < hits[j-1].at; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.code
	}
	return out
}

// CoverageFor is the abstention for a market without approved coverage,
// naming the countries asked about and, for tax and payroll, Doc 02 T12/T13.
func CoverageFor(intent string, codes []string) Template {
	names := make([]string, 0, len(codes))
	for _, c := range codes {
		if n := CountryName(c); n != "" {
			names = append(names, n)
		}
	}
	where := "that country"
	if len(names) > 0 {
		where = joinAnd(names)
	}
	t := TplAbstainCoverage
	switch intent {
	case "I17":
		t.Text = "ZoikoSuite documents tax and compliance workflows, but filing capabilities and supported jurisdictions are not universal. I can't confirm tax coverage for " + where + " from approved ZoikoSuite sources, and I can't provide tax advice. The team can confirm coverage for your country."
	case "I18":
		t.Text = "ZoikoSuite presents workforce and payroll operations within its governed platform. Support for a particular payroll activity, filing or employment rule depends on the jurisdiction, configuration and released coverage, and I can't confirm it for " + where + " from approved ZoikoSuite sources. The team can confirm coverage for your country."
	default:
		if len(names) == 0 {
			return t
		}
		t.Text = "I can't confirm what's available in " + where + " from approved ZoikoSuite sources. The team can confirm coverage for your country."
	}
	return t
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// Doc 03 §17, Doc 04 §10: whether a plan includes a feature needs typed plan
// fields; the reply names the feature asked about (from a fixed list, never
// echoing free text) instead of a fixed example.
var entitlementFeatures = []struct {
	re   *regexp.Regexp
	name string
}{
	{regexp.MustCompile(`(?i)\bintegrations?\b|\bconnectors?\b`), "integrations"},
	{regexp.MustCompile(`(?i)\bsso\b|\bsingle\s+sign-?on\b`), "single sign-on (SSO)"},
	{regexp.MustCompile(`(?i)\bapis?\b`), "API access"},
	{regexp.MustCompile(`(?i)\bconsolidat\w*`), "consolidation"},
	{regexp.MustCompile(`(?i)\bmulti-?entity\b|\bentit(?:y|ies)\b`), "multi-entity support"},
	{regexp.MustCompile(`(?i)\bpayroll\b`), "payroll"},
	{regexp.MustCompile(`(?i)\baudit\b|\bevidence\b`), "audit and evidence features"},
	{regexp.MustCompile(`(?i)\bsupport\b`), "a support level"},
}

// PlanEntitlement is TplPlanEntitlement for the feature named in text.
func PlanEntitlement(text string) Template {
	t := TplPlanEntitlement
	for _, f := range entitlementFeatures {
		if f.re.MatchString(text) {
			t.Text = "Whether a plan includes " + f.name + " depends on the plan and how it is configured. The official plan comparison shows what each plan includes; I can't confirm an entitlement for a specific account here."
			return t
		}
	}
	return t
}

// TrustCenterRe names the Trust Center.
var TrustCenterRe = regexp.MustCompile(`(?i)trust[\s-]*cent(?:er|re)`)

// MentionsAI reports a question about AI (Doc 02 I30, G15): it keeps its
// best-matching passage, which is about AI governance, not the general topic.
func MentionsAI(text string) bool {
	return intentRules["I30"].MatchString(text) || aiWordRe.MatchString(text)
}

var aiWordRe = regexp.MustCompile(`(?i)\b(?:ai|a\.i\.|artificial intelligence)\b`)
