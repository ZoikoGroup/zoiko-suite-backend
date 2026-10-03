package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ZS-JUR-001 Wave 3: the test and certification harness (s22).
//
// The harness executes a pack's test bundle against its COMPILED ARTIFACT
// ONLY, never against live registry rows, so what is tested is exactly what
// is signed and later released. The thing it executes is effective-dated rule
// resolution with declared precedence (s7, s10) - the one deterministic
// behavior the artifact defines today. No rule formula language has been
// chosen (s38), so there are no calculation tests.
//
// Test classes implemented: GOLDEN, BOUNDARY, HISTORICAL, NEGATIVE. The signing
// and digest check (the SECURITY class) is a certification gate, not a bundle
// case. The remaining s22 classes (cross-pack conflicts, migration,
// schema/payload, regression corpus, performance) are NOT implemented and are
// listed in every run and certificate so nothing implies they were proven.

const HarnessVersion = "1.0.0"

// Test classes.
const (
	ClassGolden     = "GOLDEN"
	ClassBoundary   = "BOUNDARY"
	ClassHistorical = "HISTORICAL"
	ClassNegative   = "NEGATIVE"
)

// ClassesNotImplemented are the s22 classes this harness cannot run yet.
var ClassesNotImplemented = []string{
	"CROSS_PACK_CONFLICT", "MIGRATION", "SCHEMA", "REGRESSION_CORPUS", "PERFORMANCE",
}

// Resolution outcomes.
const (
	OutcomeResolved    = "RESOLVED"
	OutcomeNoRule      = "NO_RULE"
	OutcomeUnsupported = "UNSUPPORTED_JURISDICTION"
	OutcomeAmbiguous   = "AMBIGUOUS"
)

// ── artifact view and resolution ────────────────────────────────────────────

// ArtifactScopeEntry is a jurisdiction or regime in the artifact scope.
type ArtifactScopeEntry struct {
	ID       string
	Code     string
	ParentID *string
}

// ArtifactRule is the resolution-relevant part of a rule module.
type ArtifactRule struct {
	RuleID           string
	JurisdictionID   string
	RuleDomain       string
	RuleCode         string
	EffectiveFrom    time.Time
	EffectiveTo      *time.Time
	Precedence       *int
	SupersedesRuleID *string
	ContentDigest    string

	// Fields below are for explanation and evidence, not for resolution.
	RuleName         string
	SourceIDs        []string
	InterpretationID *string
	Payload          json.RawMessage
	// Parameters are the rule's typed calculation parameters, nil when it has none.
	Parameters json.RawMessage
}

// ArtifactSource is a source-register entry as embedded in the artifact.
type ArtifactSource struct {
	SourceID       string
	Authority      string
	SourceType     string
	AuthorityLevel string
	Title          string
	SnapshotHash   string
}

// ArtifactDoc is a parsed pack artifact.
type ArtifactDoc struct {
	PackRef       string
	PackVersion   string
	Jurisdictions []ArtifactScopeEntry
	Rules         []ArtifactRule
	Sources       map[string]ArtifactSource

	// Wave 4: calendar versions and obligation rules carried in the artifact.
	Calendars   []CalendarVersion
	Obligations []ObligationRule
}

type artifactWire struct {
	Format      string `json:"format"`
	PackRef     string `json:"pack_ref"`
	PackVersion string `json:"pack_version"`
	Scope       struct {
		Jurisdictions []struct {
			ID       string  `json:"id"`
			Code     string  `json:"code"`
			ParentID *string `json:"parent_id"`
		} `json:"jurisdictions"`
	} `json:"scope"`
	RuleModules []struct {
		RuleID           string          `json:"rule_id"`
		JurisdictionID   string          `json:"jurisdiction_id"`
		RuleDomain       string          `json:"rule_domain"`
		RuleCode         string          `json:"rule_code"`
		EffectiveFrom    string          `json:"effective_from"`
		EffectiveTo      *string         `json:"effective_to"`
		Precedence       *int            `json:"precedence"`
		SupersedesRuleID *string         `json:"supersedes_rule_id"`
		ContentDigest    string          `json:"content_digest"`
		RuleName         string          `json:"rule_name"`
		SourceIDs        []string        `json:"source_ids"`
		InterpretationID *string         `json:"interpretation_id"`
		Payload          json.RawMessage `json:"payload"`
		Parameters       json.RawMessage `json:"parameters"`
	} `json:"rule_modules"`
	Sources []struct {
		SourceID       string `json:"source_id"`
		Authority      string `json:"authority"`
		SourceType     string `json:"source_type"`
		AuthorityLevel string `json:"authority_level"`
		Title          string `json:"title"`
		SnapshotHash   string `json:"snapshot_hash"`
	} `json:"sources"`
	CalendarModules []struct {
		CalendarVersionID string    `json:"calendar_version_id"`
		CalendarCode      string    `json:"calendar_code"`
		Version           int       `json:"version"`
		EffectiveFrom     string    `json:"effective_from"`
		Timezone          string    `json:"timezone"`
		WeekendDays       []int     `json:"weekend_days"`
		CutoffTime        *string   `json:"cutoff_time"`
		Holidays          []Holiday `json:"holidays"`
		ContentDigest     string    `json:"content_digest"`
		SourceIDs         []string  `json:"source_ids"`
	} `json:"calendar_modules"`
	ObligationModules []struct {
		ObligationRuleID          string   `json:"obligation_rule_id"`
		JurisdictionID            string   `json:"jurisdiction_id"`
		RegimeID                  *string  `json:"regime_id"`
		InterpretationID          *string  `json:"interpretation_id"`
		ObligationCode            string   `json:"obligation_code"`
		RuleVersion               int      `json:"rule_version"`
		Name                      string   `json:"name"`
		PeriodBasis               string   `json:"period_basis"`
		Anchor                    string   `json:"anchor"`
		OffsetMonths              int      `json:"offset_months"`
		OffsetDays                int      `json:"offset_days"`
		OffsetToMonthEnd          bool     `json:"offset_to_month_end"`
		BusinessDayAdjustment     string   `json:"business_day_adjustment"`
		CalendarCode              *string  `json:"calendar_code"`
		EffectiveFrom             string   `json:"effective_from"`
		EffectiveTo               *string  `json:"effective_to"`
		ExtensionAllowed          bool     `json:"extension_allowed"`
		MaxExtensionDays          int      `json:"max_extension_days"`
		ExtensionRequiresEvidence bool     `json:"extension_requires_evidence"`
		CutoffApplies             bool     `json:"cutoff_applies"`
		EscalationOwner           *string  `json:"escalation_owner"`
		EscalationSLAHours        *int     `json:"escalation_sla_hours"`
		ContentDigest             string   `json:"content_digest"`
		SourceIDs                 []string `json:"source_ids"`
	} `json:"obligation_modules"`
}

// ParseArtifact reads the resolution view of a compiled artifact.
func ParseArtifact(raw []byte) (*ArtifactDoc, error) {
	var w artifactWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("artifact is not valid JSON: %w", err)
	}
	if w.Format != ArtifactFormat {
		return nil, fmt.Errorf("unexpected artifact format %q", w.Format)
	}
	doc := &ArtifactDoc{PackRef: w.PackRef, PackVersion: w.PackVersion, Sources: map[string]ArtifactSource{}}
	for _, s := range w.Sources {
		doc.Sources[s.SourceID] = ArtifactSource{SourceID: s.SourceID, Authority: s.Authority, SourceType: s.SourceType,
			AuthorityLevel: s.AuthorityLevel, Title: s.Title, SnapshotHash: s.SnapshotHash}
	}
	for _, c := range w.CalendarModules {
		from, err := time.Parse(dateLayout, c.EffectiveFrom)
		if err != nil {
			return nil, fmt.Errorf("calendar %s: bad effective_from: %w", c.CalendarVersionID, err)
		}
		doc.Calendars = append(doc.Calendars, CalendarVersion{CalendarVersionID: c.CalendarVersionID, CalendarCode: c.CalendarCode, Version: c.Version,
			EffectiveFrom: from, Timezone: c.Timezone, WeekendDays: c.WeekendDays, CutoffTime: c.CutoffTime, Holidays: c.Holidays,
			ContentDigest: c.ContentDigest, SourceIDs: c.SourceIDs})
	}
	for _, o := range w.ObligationModules {
		from, err := time.Parse(dateLayout, o.EffectiveFrom)
		if err != nil {
			return nil, fmt.Errorf("obligation %s: bad effective_from: %w", o.ObligationRuleID, err)
		}
		r := ObligationRule{ObligationRuleID: o.ObligationRuleID, JurisdictionID: o.JurisdictionID, ObligationCode: o.ObligationCode, RuleVersion: o.RuleVersion,
			Name: o.Name, PeriodBasis: o.PeriodBasis, Anchor: o.Anchor, OffsetMonths: o.OffsetMonths, OffsetDays: o.OffsetDays, OffsetToMonthEnd: o.OffsetToMonthEnd,
			BusinessDayAdjustment: o.BusinessDayAdjustment, EffectiveFrom: from, ExtensionAllowed: o.ExtensionAllowed, MaxExtensionDays: o.MaxExtensionDays,
			ExtensionRequiresEvidence: o.ExtensionRequiresEvidence, CutoffApplies: o.CutoffApplies, EscalationOwner: o.EscalationOwner,
			EscalationSLAHours: o.EscalationSLAHours, RegimeID: o.RegimeID, InterpretationID: o.InterpretationID, ContentDigest: o.ContentDigest, SourceIDs: o.SourceIDs}
		if o.CalendarCode != nil {
			r.CalendarCode = *o.CalendarCode
		}
		if o.EffectiveTo != nil {
			to, err := time.Parse(dateLayout, *o.EffectiveTo)
			if err != nil {
				return nil, fmt.Errorf("obligation %s: bad effective_to: %w", o.ObligationRuleID, err)
			}
			r.EffectiveTo = &to
		}
		doc.Obligations = append(doc.Obligations, r)
	}
	for _, j := range w.Scope.Jurisdictions {
		doc.Jurisdictions = append(doc.Jurisdictions, ArtifactScopeEntry{ID: j.ID, Code: j.Code, ParentID: j.ParentID})
	}
	for _, r := range w.RuleModules {
		from, err := time.Parse(time.RFC3339Nano, r.EffectiveFrom)
		if err != nil {
			return nil, fmt.Errorf("rule %s: bad effective_from: %w", r.RuleID, err)
		}
		ar := ArtifactRule{RuleID: r.RuleID, JurisdictionID: r.JurisdictionID, RuleDomain: r.RuleDomain, RuleCode: r.RuleCode,
			EffectiveFrom: from, Precedence: r.Precedence, SupersedesRuleID: r.SupersedesRuleID, ContentDigest: r.ContentDigest,
			RuleName: r.RuleName, SourceIDs: r.SourceIDs, InterpretationID: r.InterpretationID, Payload: r.Payload, Parameters: nullToNil(r.Parameters)}
		if r.EffectiveTo != nil {
			to, err := time.Parse(time.RFC3339Nano, *r.EffectiveTo)
			if err != nil {
				return nil, fmt.Errorf("rule %s: bad effective_to: %w", r.RuleID, err)
			}
			ar.EffectiveTo = &to
		}
		doc.Rules = append(doc.Rules, ar)
	}
	return doc, nil
}

// Resolution is the answer to "which rule applies".
type Resolution struct {
	Outcome       string   `json:"outcome"`
	RuleID        string   `json:"rule_id,omitempty"`
	ContentDigest string   `json:"content_digest,omitempty"`
	Candidates    []string `json:"candidates,omitempty"`

	// Basis says why this rule won: SINGLE_CANDIDATE, DECLARED_PRECEDENCE or SUPERSEDES.
	Basis string `json:"basis,omitempty"`
	// Considered lists every rule that applied at the instant, winner included.
	Considered []string `json:"considered,omitempty"`
}

func (r ArtifactRule) effectiveAt(t time.Time) bool {
	return !t.Before(r.EffectiveFrom) && (r.EffectiveTo == nil || t.Before(*r.EffectiveTo))
}

// beats reports whether a explicitly outranks b: it supersedes it, or both
// declare distinct precedence and a's is higher. Anything else is undeclared.
func (a ArtifactRule) beats(b ArtifactRule) bool {
	if a.SupersedesRuleID != nil && *a.SupersedesRuleID == b.RuleID {
		return true
	}
	if b.SupersedesRuleID != nil && *b.SupersedesRuleID == a.RuleID {
		return false
	}
	return a.Precedence != nil && b.Precedence != nil && *a.Precedence > *b.Precedence
}

// Resolve picks the rule that applies to a jurisdiction (by code), rule
// domain, rule code and instant. The jurisdiction chain (the jurisdiction,
// then its parents that are in the pack scope) supplies candidates; the
// winner is chosen only by DECLARED precedence or supersedes links - there is
// no implicit "most specific" or "latest wins" (s10, s39). A jurisdiction
// outside the pack scope is an explicit UNSUPPORTED result, never a guess
// (JUR-NEG-22); undeclared overlap is AMBIGUOUS.
func (a *ArtifactDoc) Resolve(jurisdictionCode, ruleDomain, ruleCode string, at time.Time) Resolution {
	byID := map[string]ArtifactScopeEntry{}
	var start *ArtifactScopeEntry
	for i := range a.Jurisdictions {
		j := a.Jurisdictions[i]
		byID[j.ID] = j
		if j.Code == jurisdictionCode {
			start = &a.Jurisdictions[i]
		}
	}
	if start == nil {
		return Resolution{Outcome: OutcomeUnsupported}
	}
	chain := map[string]bool{}
	for cur, hops := start, 0; cur != nil && hops < 64; hops++ {
		chain[cur.ID] = true
		var next *ArtifactScopeEntry
		if cur.ParentID != nil {
			if p, ok := byID[*cur.ParentID]; ok && !chain[p.ID] {
				next = &p
			}
		}
		cur = next
	}

	var cands []ArtifactRule
	for _, r := range a.Rules {
		if chain[r.JurisdictionID] && r.RuleDomain == ruleDomain && r.RuleCode == ruleCode && r.effectiveAt(at) {
			cands = append(cands, r)
		}
	}
	switch len(cands) {
	case 0:
		return Resolution{Outcome: OutcomeNoRule}
	case 1:
		return Resolution{Outcome: OutcomeResolved, RuleID: cands[0].RuleID, ContentDigest: cands[0].ContentDigest,
			Basis: "SINGLE_CANDIDATE", Considered: []string{cands[0].RuleID}}
	}
	considered := make([]string, 0, len(cands))
	for _, c := range cands {
		considered = append(considered, c.RuleID)
	}
	sort.Strings(considered)
	for _, c := range cands {
		wins := true
		for _, o := range cands {
			if o.RuleID != c.RuleID && !c.beats(o) {
				wins = false
				break
			}
		}
		if wins {
			basis := "DECLARED_PRECEDENCE"
			for _, o := range cands {
				if c.SupersedesRuleID != nil && *c.SupersedesRuleID == o.RuleID {
					basis = "SUPERSEDES"
				}
			}
			return Resolution{Outcome: OutcomeResolved, RuleID: c.RuleID, ContentDigest: c.ContentDigest, Basis: basis, Considered: considered}
		}
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.RuleID)
	}
	sort.Strings(ids)
	return Resolution{Outcome: OutcomeAmbiguous, Candidates: ids, Considered: considered}
}

// ── test bundle ─────────────────────────────────────────────────────────────

var caseIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

const maxBundleCases = 5000

// CaseInput is what a case asks the resolver.
type CaseInput struct {
	Jurisdiction string `json:"jurisdiction"`
	RuleDomain   string `json:"rule_domain"`
	RuleCode     string `json:"rule_code"`
	EffectiveAt  string `json:"effective_at"`

	// Obligation cases (Wave 4): a case with obligation_code set asks for a
	// due date instead of a rule. rule_domain, rule_code and effective_at are
	// then not used and must be empty.
	// TaxableAmount makes this a calculation case (Wave 4 tax half): the rule is resolved
	// from rule_domain, rule_code and effective_at, then its parameters are applied.
	TaxableAmount        string `json:"taxable_amount"`
	ObligationCode       string `json:"obligation_code"`
	PeriodEnd            string `json:"period_end"`
	EventDate            string `json:"event_date"`
	RegistrationDate     string `json:"registration_date"`
	AnniversaryDate      string `json:"anniversary_date"`
	AnchorYear           int    `json:"anchor_year"`
	ExtensionDays        int    `json:"extension_days"`
	ExtensionEvidenceRef string `json:"extension_evidence_ref"`
}

// CaseExpect is the expected answer. RuleID is required for RESOLVED.
type CaseExpect struct {
	Outcome       string  `json:"outcome"`
	RuleID        *string `json:"rule_id"`
	ContentDigest *string `json:"content_digest"`

	// DueDate (YYYY-MM-DD) is required for a DUE_DATE_CALCULATED expectation;
	// ObligationRuleID optionally pins which obligation rule must produce it.
	TaxAmount *string `json:"tax_amount"`
	// ParameterAmount, on a RESOLVED rule case, pins the rule's fixed statutory amount (AMOUNT family).
	ParameterAmount  *string `json:"parameter_amount"`
	DueDate          *string `json:"due_date"`
	ObligationRuleID *string `json:"obligation_rule_id"`
}

// TestCase is one executable case.
type TestCase struct {
	ID          string     `json:"id"`
	Class       string     `json:"class"`
	Description string     `json:"description"`
	Input       CaseInput  `json:"input"`
	Expect      CaseExpect `json:"expect"`

	at time.Time
}

// TestBundle is the author-supplied suite.
type TestBundle struct {
	BundleVersion string     `json:"bundle_version"`
	Cases         []TestCase `json:"cases"`
}

// ErrBundleInvalid is returned for a malformed bundle.
var ErrBundleInvalid = errorString("test bundle is invalid")

// ParseTestBundle strictly decodes and validates a bundle.
func ParseTestBundle(raw []byte) (TestBundle, error) {
	var b TestBundle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, fmt.Errorf("%w: %v", ErrBundleInvalid, err)
	}
	if dec.More() {
		return b, fmt.Errorf("%w: trailing data", ErrBundleInvalid)
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrBundleInvalid, fmt.Sprintf(format, a...))
	}
	if b.BundleVersion != "1" {
		return b, bad(`bundle_version must be "1"`)
	}
	if len(b.Cases) == 0 {
		return b, bad("a bundle needs at least one case")
	}
	if len(b.Cases) > maxBundleCases {
		return b, bad("a bundle holds at most %d cases", maxBundleCases)
	}
	seen := map[string]bool{}
	for i := range b.Cases {
		c := &b.Cases[i]
		if !caseIDRe.MatchString(c.ID) {
			return b, bad("case %d: id %q is not a valid identifier", i, c.ID)
		}
		if seen[c.ID] {
			return b, bad("case id %q is repeated", c.ID)
		}
		seen[c.ID] = true
		switch c.Class {
		case ClassGolden, ClassBoundary, ClassHistorical, ClassNegative:
		default:
			return b, bad("case %s: class must be GOLDEN, BOUNDARY, HISTORICAL or NEGATIVE", c.ID)
		}
		if strings.TrimSpace(c.Input.Jurisdiction) == "" {
			return b, bad("case %s: input.jurisdiction is required", c.ID)
		}
		if strings.TrimSpace(c.Input.ObligationCode) != "" {
			if err := validateObligationCase(c); err != nil {
				return b, fmt.Errorf("%w: %v", ErrBundleInvalid, err)
			}
			continue
		}
		if strings.TrimSpace(c.Input.TaxableAmount) != "" {
			if err := validateCalcCase(c); err != nil {
				return b, fmt.Errorf("%w: %v", ErrBundleInvalid, err)
			}
			continue
		}
		if err := validateRuleCase(c); err != nil {
			return b, fmt.Errorf("%w: %v", ErrBundleInvalid, err)
		}
	}
	return b, nil
}

// ── execution and coverage ──────────────────────────────────────────────────

// CaseResult is the outcome of one case.
type CaseResult struct {
	ID       string `json:"id"`
	Class    string `json:"class"`
	Passed   bool   `json:"passed"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// CoverageGap is a structural requirement the bundle does not meet.
type CoverageGap struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
	Message string `json:"message"`
}

// TestRunResult is the full outcome of running a bundle against an artifact.
type TestRunResult struct {
	Passed                bool           `json:"passed"`
	Total                 int            `json:"total"`
	Failed                int            `json:"failed"`
	ByClass               map[string]int `json:"by_class"`
	Results               []CaseResult   `json:"results"`
	CoverageComplete      bool           `json:"coverage_complete"`
	CoverageGaps          []CoverageGap  `json:"coverage_gaps"`
	HarnessVersion        string         `json:"harness_version"`
	ClassesNotImplemented []string       `json:"classes_not_implemented"`
}

func describe(o string, ruleID *string) string {
	if ruleID != nil {
		return o + ":" + *ruleID
	}
	return o
}

// RunTestBundle executes the bundle and checks structural coverage. A run
// passes only if every case passes AND coverage is complete.
func RunTestBundle(art *ArtifactDoc, b TestBundle) TestRunResult {
	res := TestRunResult{HarnessVersion: HarnessVersion, ClassesNotImplemented: ClassesNotImplemented,
		ByClass: map[string]int{}, Results: []CaseResult{}, CoverageGaps: []CoverageGap{}}

	dueResults := map[string]DueResult{}
	calcResults := map[string]CalcOutcome{}
	for _, c := range b.Cases {
		if c.Input.ObligationCode == "" && c.Input.TaxableAmount != "" {
			co := art.Calculate(c.Input.Jurisdiction, c.Input.RuleDomain, c.Input.RuleCode, c.at, c.Input.TaxableAmount)
			calcResults[c.ID] = co
			ok := calcCaseOK(c, co)
			expTax, expRule, gotTax := "", "", ""
			if c.Expect.TaxAmount != nil {
				expTax = *c.Expect.TaxAmount
			}
			if c.Expect.RuleID != nil {
				expRule = *c.Expect.RuleID
			}
			if co.Tax != nil {
				gotTax = co.Tax.TaxAmount
			}
			res.Results = append(res.Results, CaseResult{ID: c.ID, Class: c.Class, Passed: ok,
				Expected: describeCalc(c.Expect.Outcome, expTax, expRule), Actual: describeCalc(co.Outcome, gotTax, co.RuleID)})
			res.Total++
			res.ByClass[c.Class]++
			if !ok {
				res.Failed++
			}
			continue
		}
		if c.Input.ObligationCode != "" {
			dr := art.CalculateDue(c.Input.Jurisdiction, c.Input.ObligationCode, dueFactsOf(c.Input))
			dueResults[c.ID] = dr
			ok := obligationCaseOK(c, dr)
			exp := ""
			if c.Expect.DueDate != nil {
				exp = *c.Expect.DueDate
			}
			expRule := ""
			if c.Expect.ObligationRuleID != nil {
				expRule = *c.Expect.ObligationRuleID
			}
			res.Results = append(res.Results, CaseResult{ID: c.ID, Class: c.Class, Passed: ok,
				Expected: describeDue(c.Expect.Outcome, exp, expRule), Actual: describeDue(dr.Outcome, dr.DueDate, dr.ObligationRuleID)})
			res.Total++
			res.ByClass[c.Class]++
			if !ok {
				res.Failed++
			}
			continue
		}
		got := art.Resolve(c.Input.Jurisdiction, c.Input.RuleDomain, c.Input.RuleCode, c.at)
		var actualRule *string
		if got.RuleID != "" {
			actualRule = &got.RuleID
		}
		ok := got.Outcome == c.Expect.Outcome
		if ok && c.Expect.Outcome == OutcomeResolved {
			ok = actualRule != nil && *actualRule == *c.Expect.RuleID
			if ok && c.Expect.ContentDigest != nil {
				ok = got.ContentDigest == *c.Expect.ContentDigest
			}
			if ok && c.Expect.ParameterAmount != nil {
				ok = art.parameterAmount(got.RuleID) == *c.Expect.ParameterAmount
			}
		}
		res.Results = append(res.Results, CaseResult{ID: c.ID, Class: c.Class, Passed: ok,
			Expected: describe(c.Expect.Outcome, c.Expect.RuleID), Actual: describe(got.Outcome, actualRule)})
		res.Total++
		res.ByClass[c.Class]++
		if !ok {
			res.Failed++
		}
	}

	res.CoverageGaps = append(append(coverageGaps(art, b), obligationCoverageGaps(art, b, dueResults)...), calcCoverageGaps(art, b, calcResults)...)
	res.CoverageComplete = len(res.CoverageGaps) == 0
	res.Passed = res.Failed == 0 && res.CoverageComplete && res.Total > 0
	return res
}

func coverageGaps(art *ArtifactDoc, b TestBundle) []CoverageGap {
	codeByID := map[string]string{}
	for _, j := range art.Jurisdictions {
		codeByID[j.ID] = j.Code
	}
	ruleByID := map[string]ArtifactRule{}
	for _, r := range art.Rules {
		ruleByID[r.RuleID] = r
	}

	var gaps []CoverageGap
	gap := func(code, subject, format string, a ...any) {
		gaps = append(gaps, CoverageGap{Code: code, Subject: subject, Message: fmt.Sprintf(format, a...)})
	}
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

	hasCase := func(class, juris, rdomain, rcode string, at *time.Time, rule *string) bool {
		for _, c := range b.Cases {
			if c.Class != class || c.Input.Jurisdiction != juris || c.Input.RuleDomain != rdomain || c.Input.RuleCode != rcode {
				continue
			}
			if at != nil && !c.at.Equal(*at) {
				continue
			}
			if rule != nil && (c.Expect.Outcome != OutcomeResolved || c.Expect.RuleID == nil || *c.Expect.RuleID != *rule) {
				continue
			}
			return true
		}
		return false
	}

	rules := append([]ArtifactRule(nil), art.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	for _, r := range rules {
		jc := codeByID[r.JurisdictionID]
		rid := r.RuleID
		if !hasCase(ClassGolden, jc, r.RuleDomain, r.RuleCode, nil, &rid) {
			gap("JUR-T001", rid, "rule %s/%s (%s) has no GOLDEN case that resolves to it", r.RuleDomain, r.RuleCode, jc)
		}
		if amt := art.parameterAmount(rid); amt != "" && !b.hasParameterAmountCase(rid, amt) {
			gap("JUR-T030", rid, "rule %s/%s carries a stated statutory value: a GOLDEN case must resolve it and state expect.parameter_amount (currently %s: the amount, the ISO retention duration, or the number of mapping entries) so the packaged value is deliberately checked", r.RuleDomain, r.RuleCode, amt)
		}
		boundaries := []time.Time{r.EffectiveFrom, r.EffectiveFrom.Add(-time.Nanosecond)}
		if r.EffectiveTo != nil {
			boundaries = append(boundaries, *r.EffectiveTo, r.EffectiveTo.Add(-time.Nanosecond))
		}
		for _, t := range boundaries {
			tt := t
			if !hasCase(ClassBoundary, jc, r.RuleDomain, r.RuleCode, &tt, nil) {
				gap("JUR-T002", rid, "rule %s/%s (%s) needs a BOUNDARY case at %s", r.RuleDomain, r.RuleCode, jc, rfc(t))
			}
		}
		if r.SupersedesRuleID != nil {
			if old, ok := ruleByID[*r.SupersedesRuleID]; ok {
				oid := old.RuleID
				if !hasCase(ClassHistorical, codeByID[old.JurisdictionID], old.RuleDomain, old.RuleCode, nil, &oid) {
					gap("JUR-T003", rid, "rule %s supersedes %s: a HISTORICAL case must show the earlier rule still resolves for its own period (JUR-NEG-01)", rid, oid)
				}
			}
		}
	}
	hasUnsupported := false
	for _, c := range b.Cases {
		if c.Class == ClassNegative && c.Expect.Outcome == OutcomeUnsupported {
			hasUnsupported = true
		}
	}
	if !hasUnsupported {
		gap("JUR-T004", art.PackRef, "no NEGATIVE case expects UNSUPPORTED_JURISDICTION for a jurisdiction outside the pack scope (JUR-NEG-22)")
	}
	return gaps
}

// ErrNoTestBundle is returned when tests are run before a bundle exists.
var ErrNoTestBundle = errorString("the pack version has no test bundle")

// ErrNotCertified is returned when a pack version has no certification record.
var ErrNotCertified = errorString("the pack version has no certification")

// CertificationSigningDomain separates certification-report signatures from
// artifact signatures made with the same key.
const CertificationSigningDomain = "ZS-JUR-001/pack-certification/v1\n"

// CertificationSigningMessage is the exact byte string signed for a certification report.
func CertificationSigningMessage(reportDigest string) []byte {
	return []byte(CertificationSigningDomain + reportDigest)
}

// validateRuleCase checks a rule-resolution case (no obligation fields allowed).
func validateRuleCase(c *TestCase) error {
	in := c.Input
	if in.PeriodEnd != "" || in.EventDate != "" || in.RegistrationDate != "" || in.AnniversaryDate != "" || in.AnchorYear != 0 ||
		in.ExtensionDays != 0 || in.ExtensionEvidenceRef != "" || c.Expect.DueDate != nil || c.Expect.ObligationRuleID != nil || c.Expect.TaxAmount != nil {
		return fmt.Errorf("case %s: obligation fields need input.obligation_code, and tax_amount needs input.taxable_amount", c.ID)
	}
	for _, f := range []struct{ n, v string }{{"rule_domain", in.RuleDomain}, {"rule_code", in.RuleCode}} {
		if strings.TrimSpace(f.v) == "" {
			return fmt.Errorf("case %s: input.%s is required", c.ID, f.n)
		}
	}
	at, err := time.Parse(time.RFC3339Nano, in.EffectiveAt)
	if err != nil {
		return fmt.Errorf("case %s: input.effective_at must be an RFC3339 instant", c.ID)
	}
	c.at = at.UTC()
	switch c.Expect.Outcome {
	case OutcomeResolved:
		if c.Expect.RuleID == nil || *c.Expect.RuleID == "" {
			return fmt.Errorf("case %s: a RESOLVED expectation needs rule_id", c.ID)
		}
		if c.Expect.ParameterAmount != nil {
			if v := *c.Expect.ParameterAmount; !(len(v) > 1 && isoDurationRe.MatchString(v)) && !decimalRe.MatchString(v) {
				return fmt.Errorf("case %s: expect.parameter_amount must be a non-empty value (an amount, an ISO retention duration such as P6Y, or a mapping entry count)", c.ID)
			}
		}
	case OutcomeNoRule, OutcomeUnsupported, OutcomeAmbiguous:
		if c.Expect.RuleID != nil {
			return fmt.Errorf("case %s: only a RESOLVED expectation may carry rule_id", c.ID)
		}
		if c.Expect.ParameterAmount != nil {
			return fmt.Errorf("case %s: only a RESOLVED expectation may carry parameter_amount", c.ID)
		}
	default:
		return fmt.Errorf("case %s: expect.outcome must be RESOLVED, NO_RULE, UNSUPPORTED_JURISDICTION or AMBIGUOUS", c.ID)
	}
	return nil
}

// validateObligationCase checks a due-date case.
func validateObligationCase(c *TestCase) error {
	in := c.Input
	if in.RuleDomain != "" || in.RuleCode != "" || in.EffectiveAt != "" || in.TaxableAmount != "" || c.Expect.RuleID != nil || c.Expect.ContentDigest != nil || c.Expect.TaxAmount != nil || c.Expect.ParameterAmount != nil {
		return fmt.Errorf("case %s: an obligation case must not carry rule_domain, rule_code, effective_at, expect.rule_id or expect.content_digest", c.ID)
	}
	if !obligationCodeRe.MatchString(in.ObligationCode) {
		return fmt.Errorf("case %s: obligation_code %q must be UPPER_SNAKE", c.ID, in.ObligationCode)
	}
	for n, v := range map[string]string{"period_end": in.PeriodEnd, "event_date": in.EventDate, "registration_date": in.RegistrationDate, "anniversary_date": in.AnniversaryDate} {
		if v != "" {
			if _, ok := parseDate(v); !ok {
				return fmt.Errorf("case %s: input.%s must be YYYY-MM-DD", c.ID, n)
			}
		}
	}
	switch c.Expect.Outcome {
	case DueCalculated:
		if c.Expect.DueDate == nil {
			return fmt.Errorf("case %s: a DUE_DATE_CALCULATED expectation needs due_date", c.ID)
		}
		if _, ok := parseDate(*c.Expect.DueDate); !ok {
			return fmt.Errorf("case %s: expect.due_date must be YYYY-MM-DD", c.ID)
		}
	case DueNoRule, DueUnsupported, DueInvalidFacts, DueExtensionNotAllowed, DueNoCalendar, DueAmbiguous:
		if c.Expect.DueDate != nil {
			return fmt.Errorf("case %s: only a DUE_DATE_CALCULATED expectation may carry due_date", c.ID)
		}
	default:
		return fmt.Errorf("case %s: expect.outcome %q is not a due-date outcome", c.ID, c.Expect.Outcome)
	}
	return nil
}

// CalculateDue computes a due date from the artifact alone: the obligation
// rule effective on its own anchor date, the calendar version in force, and
// the facts. A jurisdiction outside the pack scope is UNSUPPORTED, never guessed.
func (a *ArtifactDoc) CalculateDue(jurisdictionCode, obligationCode string, f DueFacts) DueResult {
	chain, ok := JurisdictionChain(a.Jurisdictions, jurisdictionCode)
	if !ok {
		return DueResult{Outcome: DueUnsupported, Explanation: []string{DueUnsupported + ": the jurisdiction is not in the pack scope"}}
	}
	rule, bad := SelectObligationRule(a.Obligations, chain, obligationCode, f)
	if rule == nil {
		return bad
	}
	return CalculateDueDate(*rule, a.Calendars, f)
}

func dueFactsOf(in CaseInput) DueFacts {
	return DueFacts{PeriodEnd: in.PeriodEnd, EventDate: in.EventDate, RegistrationDate: in.RegistrationDate, AnniversaryDate: in.AnniversaryDate,
		AnchorYear: in.AnchorYear, ExtensionDays: in.ExtensionDays, ExtensionEvidence: in.ExtensionEvidenceRef}
}

// obligationCaseOK compares a computed due result with an expectation.
func obligationCaseOK(c TestCase, got DueResult) bool {
	if got.Outcome != c.Expect.Outcome {
		return false
	}
	if c.Expect.Outcome == DueCalculated && (c.Expect.DueDate == nil || got.DueDate != *c.Expect.DueDate) {
		return false
	}
	if c.Expect.ObligationRuleID != nil && got.ObligationRuleID != *c.Expect.ObligationRuleID {
		return false
	}
	return true
}

func describeDue(o string, due string, rule string) string {
	s := o
	if due != "" {
		s += ":" + due
	}
	if rule != "" {
		s += "@" + rule
	}
	return s
}

// obligationCoverageGaps checks the structural coverage of obligation modules
// from the results of the obligation cases that were actually run.
func obligationCoverageGaps(art *ArtifactDoc, b TestBundle, results map[string]DueResult) []CoverageGap {
	var gaps []CoverageGap
	gap := func(code, subject, format string, a ...any) {
		gaps = append(gaps, CoverageGap{Code: code, Subject: subject, Message: fmt.Sprintf(format, a...)})
	}
	has := func(class, ruleID string, ok func(DueResult) bool) bool {
		for _, c := range b.Cases {
			if c.Class != class || c.Input.ObligationCode == "" {
				continue
			}
			if r, found := results[c.ID]; found && r.ObligationRuleID == ruleID && ok(r) {
				return true
			}
		}
		return false
	}
	rules := append([]ObligationRule(nil), art.Obligations...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].ObligationRuleID < rules[j].ObligationRuleID })
	for _, r := range rules {
		label := r.ObligationCode + " v" + itoa(r.RuleVersion)
		if !has(ClassGolden, r.ObligationRuleID, func(d DueResult) bool { return d.Outcome == DueCalculated }) {
			gap("JUR-T010", r.ObligationRuleID, "obligation %s has no GOLDEN case that calculates a due date from it", label)
		}
		if r.BusinessDayAdjustment != AdjustNone &&
			!has(ClassBoundary, r.ObligationRuleID, func(d DueResult) bool { return d.Outcome == DueCalculated && d.Adjusted }) {
			gap("JUR-T011", r.ObligationRuleID, "obligation %s adjusts to a business day: a BOUNDARY case must produce a due date that was moved off a weekend or holiday (JUR-NEG-26)", label)
		}
		if !has(ClassNegative, r.ObligationRuleID, func(d DueResult) bool { return d.Outcome == DueExtensionNotAllowed }) {
			gap("JUR-T012", r.ObligationRuleID, "obligation %s needs a NEGATIVE case in which an extension that is not permitted is refused (extension_days above the cap, or any extension where none is allowed)", label)
		}
	}
	return gaps
}

// nullToNil turns a JSON null (an absent parameter set) into no bytes.
func nullToNil(b json.RawMessage) json.RawMessage {
	if string(b) == "null" {
		return nil
	}
	return b
}

// parameterAmount returns the value a golden case must pin for a parameter-only rule
// (amount, ISO retention duration, mapping entry count), or "" when the rule has none.
func (a *ArtifactDoc) parameterAmount(ruleID string) string {
	for _, r := range a.Rules {
		if r.RuleID != ruleID || len(r.Parameters) == 0 {
			continue
		}
		if p, err := ParseRuleParameters(r.Parameters); err == nil && ParameterOnlyFamily(p.Family) {
			return PinnedParameterValue(p)
		}
	}
	return ""
}

// hasParameterAmountCase reports whether a GOLDEN rule case resolves ruleID and states the amount.
func (b TestBundle) hasParameterAmountCase(ruleID, amount string) bool {
	for _, c := range b.Cases {
		if c.Class == ClassGolden && c.Input.ObligationCode == "" && c.Input.TaxableAmount == "" && c.Expect.Outcome == OutcomeResolved &&
			c.Expect.RuleID != nil && *c.Expect.RuleID == ruleID && c.Expect.ParameterAmount != nil && *c.Expect.ParameterAmount == amount {
			return true
		}
	}
	return false
}

// RuleCodes returns the distinct rule codes of a rule domain in the artifact, sorted.
func (a *ArtifactDoc) RuleCodes(ruleDomain string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range a.Rules {
		if r.RuleDomain == ruleDomain && !seen[r.RuleCode] {
			seen[r.RuleCode] = true
			out = append(out, r.RuleCode)
		}
	}
	sort.Strings(out)
	return out
}

// ParameterAmountForTest exposes a rule's fixed statutory amount to test code
// outside this package that builds bundles from an artifact.
func (a *ArtifactDoc) ParameterAmountForTest(ruleID string) string { return a.parameterAmount(ruleID) }
