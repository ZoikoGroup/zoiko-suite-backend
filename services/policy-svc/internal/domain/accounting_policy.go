package domain

// REF-07 Accounting Policy (ZS-SVC-I-001 s4.17): effective-dated approved
// accounting-policy versions and rule mappings by book, entity and jurisdiction.
//
// An accounting policy is a policy of type ACCOUNTING_POLICY. Its family is one
// (tenant, policy domain, book, jurisdiction): the versions of a family chain
// and supersede each other, and a request is answered from the families that
// could apply to it. "Resolution uses book + entity/jurisdiction + effective
// date + policy domain."

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PolicyTypeAccounting is the policy_type of every accounting policy family.
const PolicyTypeAccounting = "ACCOUNTING_POLICY"

// Impact analysis scopes: what the change reaches. Closed periods are never
// among them.
const (
	AffectsFutureOnly  = "FUTURE_ONLY"
	AffectsOpenPeriods = "OPEN_PERIODS"
)

// Rule mapping kinds with their own meaning. Any other kind is a free token
// naming the thing depended on (a rule set, a jurisdiction pack, ...).
const MappingKindPolicyVersion = "POLICY_VERSION"

// Bounds.
const (
	MaxRuleMappings   = 100
	MaxPrecedence     = 1000
	MaxAccountingText = 2000
)

var (
	domainToken    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
	frameworkToken = regexp.MustCompile(`^[A-Z][A-Z0-9_.-]{1,63}$`)
	mappingKind    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,31}$`)
	semver         = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)
)

// AccountingApplicability says where a family of accounting policies applies.
type AccountingApplicability struct {
	PolicyID     string `json:"policy_id"`
	PolicyDomain string `json:"policy_domain"`
	Framework    string `json:"framework"`
	// BookID and JurisdictionID nil mean every book / jurisdiction.
	BookID         *string   `json:"book_id"`
	JurisdictionID *string   `json:"jurisdiction_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// Specificity is how many of book and jurisdiction the family narrows to.
func (a AccountingApplicability) Specificity() int {
	n := 0
	if a.BookID != nil {
		n++
	}
	if a.JurisdictionID != nil {
		n++
	}
	return n
}

// ImpactAnalysis is the author's statement of what a policy change reaches.
// "Policy change includes impact analysis for future/open periods and does not
// mutate closed-history treatment."
type ImpactAnalysis struct {
	Summary string `json:"summary"`
	// AffectedPeriods is AffectsFutureOnly or AffectsOpenPeriods.
	AffectedPeriods string `json:"affected_periods"`
	// ClosedHistoryUnchanged must be true: a policy change never restates a
	// closed period.
	ClosedHistoryUnchanged bool `json:"closed_history_unchanged"`
	// RetroactiveJustification is required when the version takes effect
	// before it was drafted.
	RetroactiveJustification string `json:"retroactive_justification,omitempty"`
	Notes                    string `json:"notes,omitempty"`
}

// RuleMapping is one dependency of a policy version, pinned to the version it
// was approved against (PolicyRuleMapping).
type RuleMapping struct {
	MappingNo  int    `json:"mapping_no"`
	RefKind    string `json:"ref_kind"`
	Ref        string `json:"ref"`
	RefVersion string `json:"ref_version"`
	Rationale  string `json:"rationale"`
	MapsTo     string `json:"maps_to,omitempty"`
	// DependsOnVersionID is set for a dependency on another policy version.
	DependsOnVersionID *string `json:"depends_on_version_id,omitempty"`
}

// AccountingVersionDetails is what an accounting policy version rests on.
type AccountingVersionDetails struct {
	SemanticVersion string         `json:"semantic_version"`
	Precedence      int            `json:"precedence"`
	FrameworkSource string         `json:"framework_source"`
	PolicyTextRef   string         `json:"policy_text_ref"`
	Impact          ImpactAnalysis `json:"impact_analysis"`
}

// AccountingPolicyVersion is a policy version with its accounting content.
type AccountingPolicyVersion struct {
	PolicyVersion
	PolicyCode    string                   `json:"policy_code"`
	Applicability AccountingApplicability  `json:"applicability"`
	Details       AccountingVersionDetails `json:"details"`
	RuleMappings  []RuleMapping            `json:"rule_mappings"`
}

// Pin is what a consumer persists with a calculation or posting: the exact
// policy version that governed it and the digest of its content, so the result
// can be explained and reproduced later.
type Pin struct {
	PolicyID        string `json:"policy_id"`
	PolicyVersionID string `json:"policy_version_id"`
	VersionNumber   int    `json:"version_number"`
	SemanticVersion string `json:"semantic_version"`
	ArtifactDigest  string `json:"artifact_digest"`
	RowVersion      int    `json:"row_version"`
}

// PinOf is the pin of a version.
func PinOf(v *AccountingPolicyVersion) Pin {
	return Pin{
		PolicyID: v.PolicyID, PolicyVersionID: v.PolicyVersionID, VersionNumber: v.VersionNumber,
		SemanticVersion: v.Details.SemanticVersion, ArtifactDigest: v.ArtifactDigest, RowVersion: v.RowVersion,
	}
}

// AccountingCandidate is a version that was ever bound, with what resolution
// needs to rank it.
type AccountingCandidate struct {
	ApplicablePolicyVersion
	Applicability   AccountingApplicability
	SemanticVersion string
	Precedence      int
}

// AccountingCandidateQuery selects the candidates for a request: the tenant's
// accounting versions whose scope fits the entity and whose family applies to
// the book and jurisdiction. A family narrowed to a book applies only to a
// request that names that book.
type AccountingCandidateQuery struct {
	TenantID       string
	LegalEntityID  *string
	BookID         *string
	JurisdictionID *string
	// PolicyDomain empty means every domain.
	PolicyDomain string
}

// DraftAccountingPolicyParams is the DraftAccountingPolicy command.
type DraftAccountingPolicyParams struct {
	TenantID       string
	PolicyDomain   string
	Framework      string
	BookID         *string
	JurisdictionID *string
	LegalEntityID  *string

	SemanticVersion string
	Precedence      int
	FrameworkSource string
	PolicyTextRef   string
	RuleMappings    []RuleMapping
	Impact          ImpactAnalysis

	EffectiveFrom time.Time
	EffectiveTo   *time.Time
	Rationale     string
	Source        string

	PolicyVersionID      string
	CreatedByPrincipalID string
	RequestKey           string
	Meta                 EventMeta
}

// Validate reports everything wrong with the command that can be known without
// the store. now is when it is drafted: a version that takes effect before it
// need an explicit statement that it is retroactive.
func (p DraftAccountingPolicyParams) Validate(now time.Time) []string {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if !domainToken.MatchString(p.PolicyDomain) {
		add("policy_domain must be upper-case letters, digits and underscores, starting with a letter (2 to 64 characters)")
	}
	if !frameworkToken.MatchString(p.Framework) {
		add("framework must be upper-case letters, digits and . _ - (2 to 64 characters), for example IFRS")
	}
	if p.BookID != nil && !isUUID(*p.BookID) {
		add("book_id must be a UUID")
	}
	if p.JurisdictionID != nil && !isUUID(*p.JurisdictionID) {
		add("jurisdiction_id must be a UUID")
	}
	if p.LegalEntityID != nil && !isUUID(*p.LegalEntityID) {
		add("legal_entity_id must be a UUID")
	}
	if len(p.SemanticVersion) > 32 || !semver.MatchString(p.SemanticVersion) {
		add("semantic_version must be a semantic version such as 2.1.0 (at most 32 characters)")
	}
	if p.Precedence < 0 || p.Precedence > MaxPrecedence {
		add("precedence must be between 0 and %d", MaxPrecedence)
	}
	for field, v := range map[string]string{"framework_source": p.FrameworkSource, "policy_text_ref": p.PolicyTextRef, "rationale": p.Rationale} {
		switch {
		case strings.TrimSpace(v) == "":
			add("%s is required", field)
		case len(v) > MaxAccountingText:
			add("%s is longer than %d characters", field, MaxAccountingText)
		}
	}
	if p.EffectiveFrom.IsZero() {
		add("effective_from is required")
	} else if p.EffectiveTo != nil && !p.EffectiveTo.After(p.EffectiveFrom) {
		add("effective_to must be after effective_from")
	}
	errs = append(errs, validateMappings(p.RuleMappings)...)
	errs = append(errs, p.Impact.validate(p.EffectiveFrom, now)...)
	return errs
}

func (i ImpactAnalysis) validate(effectiveFrom, now time.Time) []string {
	var errs []string
	if strings.TrimSpace(i.Summary) == "" {
		errs = append(errs, "impact_analysis.summary is required: say what this change reaches")
	}
	if len(i.Summary) > MaxAccountingText || len(i.Notes) > MaxAccountingText || len(i.RetroactiveJustification) > MaxAccountingText {
		errs = append(errs, fmt.Sprintf("impact_analysis text is longer than %d characters", MaxAccountingText))
	}
	switch i.AffectedPeriods {
	case AffectsFutureOnly, AffectsOpenPeriods:
	default:
		errs = append(errs, fmt.Sprintf("impact_analysis.affected_periods must be %s or %s", AffectsFutureOnly, AffectsOpenPeriods))
	}
	if !i.ClosedHistoryUnchanged {
		errs = append(errs, "impact_analysis.closed_history_unchanged must be true: a policy change does not restate a closed period")
	}
	// Taking effect in the past reaches back into periods that may already
	// have been calculated, so it must say so and say why.
	if !effectiveFrom.IsZero() && effectiveFrom.Before(now) {
		if i.AffectedPeriods != AffectsOpenPeriods {
			errs = append(errs, "a version that takes effect before it is drafted reaches open periods: impact_analysis.affected_periods must be OPEN_PERIODS")
		}
		if strings.TrimSpace(i.RetroactiveJustification) == "" {
			errs = append(errs, "a version that takes effect before it is drafted needs impact_analysis.retroactive_justification")
		}
	}
	return errs
}

func validateMappings(ms []RuleMapping) []string {
	var errs []string
	if len(ms) > MaxRuleMappings {
		return []string{fmt.Sprintf("a policy version carries at most %d rule mappings", MaxRuleMappings)}
	}
	seen := map[string]int{}
	for i, m := range ms {
		n := i + 1
		if !mappingKind.MatchString(m.RefKind) {
			errs = append(errs, fmt.Sprintf("rule_mappings[%d].ref_kind must be upper-case letters, digits and underscores (2 to 32 characters)", n))
		}
		switch {
		case strings.TrimSpace(m.Ref) == "" || len(m.Ref) > 512:
			errs = append(errs, fmt.Sprintf("rule_mappings[%d].ref is required (at most 512 characters)", n))
		case m.RefKind == MappingKindPolicyVersion && !isUUID(m.Ref):
			errs = append(errs, fmt.Sprintf("rule_mappings[%d].ref must be a policy version id for a %s dependency", n, MappingKindPolicyVersion))
		}
		if strings.TrimSpace(m.RefVersion) == "" || len(m.RefVersion) > 64 {
			errs = append(errs, fmt.Sprintf("rule_mappings[%d].ref_version is required: pin the version depended on (at most 64 characters)", n))
		}
		if strings.TrimSpace(m.Rationale) == "" || len(m.Rationale) > MaxAccountingText {
			errs = append(errs, fmt.Sprintf("rule_mappings[%d].rationale is required", n))
		}
		if len(m.MapsTo) > 512 {
			errs = append(errs, fmt.Sprintf("rule_mappings[%d].maps_to is longer than 512 characters", n))
		}
		key := m.RefKind + "|" + m.Ref
		if first, dup := seen[key]; dup {
			errs = append(errs, fmt.Sprintf("rule_mappings[%d] repeats rule_mappings[%d]", n, first))
		}
		seen[key] = n
	}
	return errs
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// FamilyKey is the dedup key of a family: the same tenant, domain, book and
// jurisdiction are the same family. It is also what its policy_code is built
// from.
func FamilyKey(domain string, bookID, jurisdictionID *string) string {
	b, j := "*", "*"
	if bookID != nil {
		b = strings.ToLower(*bookID)
	}
	if jurisdictionID != nil {
		j = strings.ToLower(*jurisdictionID)
	}
	return domain + "|" + b + "|" + j
}

// RulePayload is the stored content of an accounting policy version: all of it,
// so that the version's artifact digest covers what the policy is, where it
// applies and what it depends on. Maps marshal with sorted keys, so the same
// content is the same bytes.
func (p DraftAccountingPolicyParams) RulePayload() json.RawMessage {
	mappings := make([]map[string]any, 0, len(p.RuleMappings))
	for i, m := range p.RuleMappings {
		row := map[string]any{
			"mapping_no": i + 1, "ref_kind": m.RefKind, "ref": m.Ref, "ref_version": m.RefVersion, "rationale": m.Rationale,
		}
		if m.MapsTo != "" {
			row["maps_to"] = m.MapsTo
		}
		mappings = append(mappings, row)
	}
	impact := map[string]any{
		"summary": p.Impact.Summary, "affected_periods": p.Impact.AffectedPeriods, "closed_history_unchanged": p.Impact.ClosedHistoryUnchanged,
	}
	if p.Impact.RetroactiveJustification != "" {
		impact["retroactive_justification"] = p.Impact.RetroactiveJustification
	}
	if p.Impact.Notes != "" {
		impact["notes"] = p.Impact.Notes
	}
	payload := map[string]any{
		"kind": PolicyTypeAccounting, "policy_domain": p.PolicyDomain, "framework": p.Framework,
		"book_id": p.BookID, "jurisdiction_id": p.JurisdictionID,
		"semantic_version": p.SemanticVersion, "precedence": p.Precedence,
		"framework_source": p.FrameworkSource, "policy_text_ref": p.PolicyTextRef,
		"rule_mappings": mappings, "impact_analysis": impact,
	}
	b, _ := json.Marshal(payload)
	return b
}

var (
	// ErrInvalidAccountingPolicy: the draft is not acceptable; the handler
	// reports the individual problems.
	ErrInvalidAccountingPolicy = errorString("invalid accounting policy")
	// ErrAccountingDependencyNotFound: a rule mapping names a policy version
	// that does not exist.
	ErrAccountingDependencyNotFound = errorString("a rule mapping depends on a policy version that does not exist")
	// ErrNotAccountingPolicy: the policy is not an accounting policy.
	ErrNotAccountingPolicy = errorString("not an accounting policy")
	// ErrAccountingPolicyRequired: a generic policy endpoint was used on an
	// accounting policy, which has its own commands.
	ErrAccountingPolicyRequired = errorString("accounting policies are managed through /v1/accounting-policies")
)

// Rank orders candidates of different families: the narrower scope first (the
// tenant, the legal entity, the book and the jurisdiction each narrow it), then
// the explicit precedence. Two candidates equal in both are ambiguous.
func (c *AccountingCandidate) Rank() []int {
	n := c.Applicability.Specificity()
	if c.TenantID != nil {
		n++
	}
	if c.LegalEntityID != nil {
		n++
	}
	return []int{n, c.Precedence}
}
