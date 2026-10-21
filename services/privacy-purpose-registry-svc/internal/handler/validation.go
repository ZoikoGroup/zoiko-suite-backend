package handler

import (
	"context"
	"errors"
	"strings"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

// runStructuralValidation evaluates the eight §8.2 minimum activation gates
// against a version and returns every finding, each with a §32 code. Any
// finding means the version stays DRAFT (PRV-I13: never a partial permit).
//
// It is called twice for every activation: at /validate (DRAFT -> VALIDATED)
// and again at /activate, because the registries it checks against
// (jurisdictions, data categories, linked purposes) are live. A store error
// is returned, not turned into a finding — an outage must not masquerade as a
// content problem.
//
// Code mapping. §32 fixes the MEANING of each code but does not map gates to
// codes; the mapping below is the service's, documented in the README:
//
//	gate 1  owners                    PRV-003 (accountable party unresolved)
//	gate 2  role                      PRV-003 PRIVACY_ROLE_UNRESOLVED
//	gate 3  purpose / rationale       PRV-001 PURPOSE_NOT_REGISTERED
//	gate 4  lawful basis              PRV-005 POLICY_UNAVAILABLE
//	        special-category cond.    PRV-010 DATA_CATEGORY_RESTRICTED
//	gate 5  jurisdictions             PRV-004 JURISDICTION_UNRESOLVED
//	        subjects / categories     PRV-010 DATA_CATEGORY_RESTRICTED
//	        sources / recipients      PRV-019 PRIVACY_CONTEXT_INDETERMINATE
//	gate 6  retention                 PRV-014
//	        transfer                  PRV-015 TRANSFER_NOT_AUTHORIZED
//	        processor instruction     PRV-017 PROCESSOR_INSTRUCTION_MISSING
//	gate 7  notice/consent            PRV-006
//	gate 8  DPIA/TIA                  PRV-016 ASSESSMENT_REQUIRED
func (h *Handler) runStructuralValidation(ctx context.Context, v *domain.ProcessingActivityVersion) ([]domain.ValidationFinding, error) {
	var f []domain.ValidationFinding
	add := func(code, field, msg string) {
		f = append(f, domain.ValidationFinding{Code: code, Field: field, Message: msg})
	}
	// requireList flags an empty list and any blank entry in it.
	requireList := func(code, field, what string, xs []string) {
		if len(nonBlank(xs)) == 0 {
			add(code, field, "at least one "+what+" is required")
		} else if hasBlank(xs) {
			add(code, field, field+" contains a blank entry")
		}
	}

	// Gate 1: named accountable business owner AND privacy owner.
	if strings.TrimSpace(v.Owner) == "" {
		add("PRV-003", "owner", "an accountable business owner is required")
	}
	if strings.TrimSpace(v.PrivacyOwner) == "" {
		add("PRV-003", "privacy_owner", "an accountable privacy owner is required")
	}

	// Gate 2: controller/processor role resolved.
	if !v.PrivacyRole.Valid() {
		add("PRV-003", "privacy_role", "privacy_role must be one of CONTROLLER, PROCESSOR, JOINT_CONTROLLER")
	}

	// Gates 3 & 4: every linked purpose is registered, published and in force,
	// carries a necessity/minimization rationale and an approved lawful basis;
	// sensitive categories additionally need a special-condition reference.
	requireList("PRV-001", "purpose_ids", "purpose_id", v.PurposeIDs)
	sensitive := false
	for _, c := range v.DataCategories {
		if domain.IsSensitiveCategory(c) {
			sensitive = true
		}
	}
	specialCondition := false
	now := h.now()
	for _, pid := range nonBlank(v.PurposeIDs) {
		published, err := h.store.IsPurposePublished(ctx, pid)
		if err != nil {
			return nil, err
		}
		if !published {
			add("PRV-001", "purpose_ids", "purpose "+pid+" is not a registered, published purpose")
			continue
		}
		pv, err := h.store.ResolvePurposeAsOf(ctx, pid, now, now)
		if err != nil {
			if errors.Is(err, domain.ErrPurposeNotFound) {
				add("PRV-001", "purpose_ids", "purpose "+pid+" has no published version in force yet")
				continue
			}
			return nil, err
		}
		if strings.TrimSpace(pv.NecessityRationale) == "" {
			add("PRV-001", "purpose_ids", "purpose "+pid+" has no necessity/minimization rationale (§8.2 gate 3)")
		}
		if len(nonBlank(pv.LawfulBasisRefs)) == 0 {
			add("PRV-005", "purpose_ids", "purpose "+pid+" has no approved lawful-basis reference")
		}
		if len(nonBlank(pv.SpecialConditionRefs)) > 0 {
			specialCondition = true
		}
	}
	if sensitive && !specialCondition {
		add("PRV-010", "special_condition_refs",
			"sensitive/special-category data requires a special-condition reference on at least one linked purpose")
	}

	// Gate 5: subjects, categories, sources, recipients, jurisdictions.
	requireList("PRV-004", "jurisdictions", "jurisdiction", v.Jurisdictions)
	distinct := map[string]bool{}
	for _, j := range nonBlank(v.Jurisdictions) {
		distinct[domain.NormalizeCode(j)] = true
		if !domain.IsValidJurisdiction(j) {
			add("PRV-004", "jurisdictions", "jurisdiction "+j+" is not a registered jurisdiction (ISO 3166-1 alpha-2, EU, EEA or GLOBAL)")
		}
	}
	requireList("PRV-010", "subject_classes", "subject class", v.SubjectClasses)
	requireList("PRV-010", "data_categories", "data category", v.DataCategories)
	for _, c := range nonBlank(v.DataCategories) {
		if !domain.IsValidDataCategory(c) {
			add("PRV-010", "data_categories", "data category "+c+" is not in the registered taxonomy")
		}
	}
	requireList("PRV-019", "sources", "data source", v.Sources)
	requireList("PRV-019", "recipients", "recipient", v.Recipients)

	// Gate 6: retention, transfer and processor dependencies identified.
	requireList("PRV-014", "retention_rule_refs", "retention rule reference", v.RetentionRuleRefs)
	if len(distinct) > 1 {
		requireList("PRV-015", "transfer_refs", "transfer reference (processing spans multiple jurisdictions)", v.TransferRefs)
	}
	if v.PrivacyRole == domain.RoleProcessor {
		requireList("PRV-017", "processor_refs", "processor-instruction reference (a PROCESSOR activity must cite its documented instruction)", v.ProcessorRefs)
	}

	// Gate 7: notice/consent dependency explicitly declared — never implicit.
	if v.NoticeConsentDependency == "" || !v.NoticeConsentDependency.Valid() {
		add("PRV-006", "notice_consent_dependency",
			"notice_consent_dependency must be explicitly set to REQUIRED, NOT_REQUIRED, or CONDITIONAL — never left implicit")
	}

	// Gate 8: DPIA/TIA requirement resolved by policy or marked review-required.
	if v.DPIATIAStatus == "" || !v.DPIATIAStatus.Valid() {
		add("PRV-016", "dpia_tia_status",
			"dpia_tia_status must be RESOLVED, NOT_REQUIRED, or REVIEW_REQUIRED")
	}

	return f, nil
}
