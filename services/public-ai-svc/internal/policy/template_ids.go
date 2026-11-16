package policy

// Template identity (Doc 02 Appendix C: template_id + locale + version).
// Analytics and release review key on a stable id, never on the wording,
// which changes with Legal/Product review. Doc 02 numbers its templates
// T01-T30; templates the document does not number get a named id.

// TemplateLocale is the only approved locale in R0 (Doc 02 §23).
const TemplateLocale = "en-US"

var templateIDs map[string]string

func init() {
	ids := map[string]Template{
		"T01": TplWelcome, "T05": TplClarifyMarket, "T06": TplClarifyTrialMarket,
		"T07": BoundaryTemplates["I09"], "T11": BoundaryTemplates["I14"], "T14": TplEvidenceAssurance,
		"T10": TplAbstainCertification, "T16": RouteTemplates["I23"], "T17": RouteTemplates["I22"],
		"T18": BoundaryTemplates["I25"], "T19": ExceptionTemplates[X07CrossProduct], "T20": ExceptionTemplates[X04RegulatedAdvice],
		"T21": ExceptionTemplates[X01PrivateAction], "T22": ExceptionTemplates[X03SensitiveInput],
		"T23": ExceptionTemplates[X05SecurityEscalate], "T24": TplUnavailable, "T25": TplOutOfScope,
		"T26": RouteTemplates["I24"], "T27": TplUptime, "T28": TplLiveSupport, "T30": RouteTemplates["I27"],
		"TX-INJECTION": ExceptionTemplates[X06PromptInjection], "TX-CLAIM-VERIFY": ExceptionTemplates[X08ClaimVerification],
		"TX-UNSUPPORTED-COVERAGE": ExceptionTemplates[X09UnsupportedCover],
		"TX-NO-EVIDENCE":          TplAbstainNoEvidence, "TX-STALE": TplAbstainStale, "TX-CONFLICT": TplAbstainConflict,
		"TX-CLARIFY-PLAN": TplClarifyPlan, "TX-CLARIFY-GIVE-UP": TplClarifyGiveUp, "TX-COVERAGE": TplAbstainCoverage,
		"TX-TRIAL-STALE": TplAbstainTrialStale, "TX-PRICE-INCOMPLETE": TplPriceIncomplete, "TX-TRIAL-INCOMPLETE": TplTrialIncomplete,
		"TX-LIVE-PLATFORM": TplLivePlatform, "TX-NO-APPROVED-INFO": TplNoApprovedInfo, "TX-DATA-RIGHTS": TplDataRights,
		"TX-DEMO-ACCEPTED": TplDemoAccepted, "TX-TICKET": TplTicket, "TX-READINESS": TplReadiness,
		"TX-PLAN-ENTITLEMENT": TplPlanEntitlement, "TX-NON-ENGLISH": TplNonEnglish,
		"TX-ROUTE-DEMO": RouteTemplates["I08"], "TX-ROUTE-COMPARE": RouteTemplates["I26"], "TX-ROUTE-BILLING": RouteTemplates["I28"],
		"TX-AI": BoundaryTemplates["I30"],
	}
	templateIDs = make(map[string]string, len(ids))
	for id, t := range ids {
		templateIDs[t.Text] = id
	}
}

// TemplateID names the approved template a reply was built from, or "" for
// a generated (grounded) answer or a rendered template.
func TemplateID(text string) string { return templateIDs[text] }
