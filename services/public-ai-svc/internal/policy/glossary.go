package policy

import (
	"regexp"
	"strings"

	"zoiko.io/public-ai-svc/internal/contract"
)

// Doc 02 §23: "expand SoD, SSO, SCIM, DPA or SOC 2 when asked." These are
// general definitions, not ZoikoSuite claims: none says ZoikoSuite supports
// or holds anything. Each ends by offering ZoikoSuite's published material.

type glossaryEntry struct {
	text string
	chip contract.Chip // the ZoikoSuite question it leads to
}

var glossary = map[string]glossaryEntry{
	"sso": {"SSO stands for single sign-on: people sign in once with their organisation's identity provider to reach several applications. This is a general definition, not a statement about ZoikoSuite.",
		contract.Chip{Label: "Security and trust", Message: "How is data secured?"}},
	"scim": {"SCIM stands for System for Cross-domain Identity Management: an open standard for creating, updating and removing user accounts across applications automatically. This is a general definition, not a statement about ZoikoSuite.",
		contract.Chip{Label: "Integrations", Message: "Which integrations does ZoikoSuite support?"}},
	"sod": {"SoD stands for segregation of duties: a control that splits key steps, such as creating and approving a payment, between different people. This is a general definition, not a statement about ZoikoSuite.",
		contract.Chip{Label: "Governance", Message: "How do governance and approvals work in ZoikoSuite?"}},
	"dpa": {"A DPA is a data processing agreement: the contract that sets out how a supplier processes personal data on a customer's behalf. This is a general definition, not a statement about ZoikoSuite.",
		contract.Chip{Label: "Privacy", Message: "How does ZoikoSuite handle privacy and personal data?"}},
	"soc 2": {"SOC 2 is an independent auditor's report on a service organisation's controls, against the AICPA Trust Services Criteria. It is an attestation report, not a certification. This is a general definition, not a statement about ZoikoSuite.",
		contract.Chip{Label: "Independent assurance", Message: "Which security certifications does ZoikoSuite hold?"}},
	"mfa": {"MFA stands for multi-factor authentication: signing in needs two or more kinds of proof, such as a password and a one-time code. This is a general definition, not a statement about ZoikoSuite.",
		contract.Chip{Label: "Security and trust", Message: "How is data secured?"}},
}

// glossaryRe is a bare "what is X?" / "what does X stand for?" question about
// one known acronym, and nothing else.
var glossaryRe = regexp.MustCompile(`(?i)^\s*(?:what(?:'s|\s+is|\s+are|\s+does)|whats|define|meaning\s+of|explain)\s+(?:an?\s+|the\s+)?(sso|scim|sod|dpa|soc\s*-?\s*2|mfa)(?:\s+(?:stand\s+for|mean|means))?\s*\??\s*$`)

// Glossary returns the definition template and its follow-up chip for a bare
// acronym question (Doc 02 §23).
func Glossary(text string) (Template, []contract.Chip, bool) {
	m := glossaryRe.FindStringSubmatch(text)
	if m == nil {
		return Template{}, nil, false
	}
	key := strings.ToLower(m[1])
	if strings.HasPrefix(key, "soc") {
		key = "soc 2"
	}
	e := glossary[key]
	return Template{contract.Clarify, e.text, "", false, contract.ReasonGlossary}, []contract.Chip{e.chip}, true
}

// TplMarketNoted answers a message that only names the visitor's country
// (Doc 02 §10): the market is kept for this session and used for prices,
// trials and availability. It never says the market is covered.
var TplMarketNoted = Template{contract.Clarify, "Thanks. I'll use {{country}} when you ask about plans, prices, trials or availability in this conversation. Coverage for each country comes from ZoikoSuite's approved information. What would you like to know?", "", false, contract.ReasonContextNoted}
