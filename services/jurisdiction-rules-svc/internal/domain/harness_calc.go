package domain

import (
	"fmt"
	"sort"
	"time"
)

// ZS-JUR-001 Wave 4 (tax half): calculation cases in the certification harness.
//
// A calculation case resolves a rule exactly like a rule case (jurisdiction,
// rule domain, rule code, effective instant) and then applies the rule's
// typed parameters to a taxable amount. Everything runs against the compiled
// artifact only.

// CalcOutcome is the result of resolving a rule and calculating with it.
type CalcOutcome struct {
	Outcome string
	RuleID  string
	Tax     *TaxResult
}

// Calculate resolves the applicable rule from the artifact and applies its
// parameters to amount. A rule without parameters is NO_PARAMETERS: the
// calculation never guesses a rate.
func (a *ArtifactDoc) Calculate(jurisdictionCode, ruleDomain, ruleCode string, at time.Time, amount string) CalcOutcome {
	res := a.Resolve(jurisdictionCode, ruleDomain, ruleCode, at)
	if res.Outcome != OutcomeResolved {
		return CalcOutcome{Outcome: res.Outcome}
	}
	for _, r := range a.Rules {
		if r.RuleID != res.RuleID {
			continue
		}
		if len(r.Parameters) == 0 {
			return CalcOutcome{Outcome: CalcNoParameters, RuleID: r.RuleID}
		}
		p, err := ParseRuleParameters(r.Parameters)
		if err != nil {
			t := TaxResult{Outcome: CalcInvalidFacts, Message: err.Error(), Steps: []string{err.Error()}}
			return CalcOutcome{Outcome: CalcInvalidFacts, RuleID: r.RuleID, Tax: &t}
		}
		t := CalculateTax(p, amount)
		return CalcOutcome{Outcome: t.Outcome, RuleID: r.RuleID, Tax: &t}
	}
	return CalcOutcome{Outcome: OutcomeNoRule}
}

// validateCalcCase checks a calculation case.
func validateCalcCase(c *TestCase) error {
	in := c.Input
	if in.PeriodEnd != "" || in.EventDate != "" || in.RegistrationDate != "" || in.AnniversaryDate != "" || in.AnchorYear != 0 ||
		in.ExtensionDays != 0 || in.ExtensionEvidenceRef != "" || c.Expect.DueDate != nil || c.Expect.ObligationRuleID != nil || c.Expect.ContentDigest != nil || c.Expect.ParameterAmount != nil {
		return fmt.Errorf("case %s: a calculation case must not carry obligation fields or expect.content_digest", c.ID)
	}
	for _, f := range []struct{ n, v string }{{"rule_domain", in.RuleDomain}, {"rule_code", in.RuleCode}} {
		if f.v == "" {
			return fmt.Errorf("case %s: input.%s is required", c.ID, f.n)
		}
	}
	at, err := time.Parse(time.RFC3339Nano, in.EffectiveAt)
	if err != nil {
		return fmt.Errorf("case %s: input.effective_at must be an RFC3339 instant", c.ID)
	}
	c.at = at.UTC()
	switch c.Expect.Outcome {
	case CalcCalculated:
		if c.Expect.TaxAmount == nil {
			return fmt.Errorf("case %s: a CALCULATED expectation needs tax_amount", c.ID)
		}
		if _, ok := parseDecimal(*c.Expect.TaxAmount); !ok {
			return fmt.Errorf("case %s: expect.tax_amount must be a decimal string", c.ID)
		}
	case OutcomeNoRule, OutcomeUnsupported, OutcomeAmbiguous, CalcNoParameters, CalcInvalidFacts:
		if c.Expect.TaxAmount != nil {
			return fmt.Errorf("case %s: only a CALCULATED expectation may carry tax_amount", c.ID)
		}
	default:
		return fmt.Errorf("case %s: expect.outcome %q is not a calculation outcome", c.ID, c.Expect.Outcome)
	}
	return nil
}

func calcCaseOK(c TestCase, got CalcOutcome) bool {
	if got.Outcome != c.Expect.Outcome {
		return false
	}
	if c.Expect.Outcome == CalcCalculated && (got.Tax == nil || got.Tax.TaxAmount != *c.Expect.TaxAmount) {
		return false
	}
	if c.Expect.RuleID != nil && got.RuleID != *c.Expect.RuleID {
		return false
	}
	return true
}

func describeCalc(o, tax, rule string) string {
	s := o
	if tax != "" {
		s += ":" + tax
	}
	if rule != "" {
		s += "@" + rule
	}
	return s
}

// calcCoverageGaps checks the structural coverage of parameterized rules from
// the calculation cases that were actually run.
func calcCoverageGaps(art *ArtifactDoc, b TestBundle, results map[string]CalcOutcome) []CoverageGap {
	var gaps []CoverageGap
	gap := func(code, subject, format string, a ...any) {
		gaps = append(gaps, CoverageGap{Code: code, Subject: subject, Message: fmt.Sprintf(format, a...)})
	}
	has := func(class, ruleID string, ok func(TestCase, CalcOutcome) bool) bool {
		for _, c := range b.Cases {
			if c.Class != class || c.Input.TaxableAmount == "" || c.Input.ObligationCode != "" {
				continue
			}
			if r, found := results[c.ID]; found && r.RuleID == ruleID && ok(c, r) {
				return true
			}
		}
		return false
	}
	rules := append([]ArtifactRule(nil), art.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	for _, r := range rules {
		if len(r.Parameters) == 0 {
			continue
		}
		p, err := ParseRuleParameters(r.Parameters)
		if err != nil {
			continue // the compiler already refused invalid parameters
		}
		if ParameterOnlyFamily(p.Family) {
			continue // a fixed statutory amount is pinned by a golden rule case (JUR-T030), not calculated with
		}
		label := r.RuleDomain + "/" + r.RuleCode
		if !has(ClassGolden, r.RuleID, func(_ TestCase, o CalcOutcome) bool { return o.Outcome == CalcCalculated }) {
			gap("JUR-T020", r.RuleID, "rule %s has typed parameters but no GOLDEN calculation case that calculates with it", label)
		}
		for _, band := range p.Bands {
			if band.UpTo == nil {
				continue
			}
			limit, _ := parseDecimal(*band.UpTo)
			lim := *band.UpTo
			if !has(ClassBoundary, r.RuleID, func(c TestCase, o CalcOutcome) bool {
				amt, ok := parseDecimal(c.Input.TaxableAmount)
				return ok && amt.Cmp(limit) == 0 && o.Outcome == CalcCalculated
			}) {
				gap("JUR-T021", r.RuleID, "rule %s needs a BOUNDARY calculation case with taxable_amount exactly %s (a band limit)", label, lim)
			}
		}
		if !has(ClassBoundary, r.RuleID, func(_ TestCase, o CalcOutcome) bool {
			return o.Outcome == CalcCalculated && o.Tax != nil && o.Tax.Rounded
		}) {
			gap("JUR-T022", r.RuleID, "rule %s needs a BOUNDARY calculation case whose result is actually rounded (to exercise %s at scale %d)", label, p.Rounding.Mode, p.Rounding.Scale)
		}
		if !has(ClassNegative, r.RuleID, func(_ TestCase, o CalcOutcome) bool { return o.Outcome == CalcInvalidFacts }) {
			gap("JUR-T023", r.RuleID, "rule %s needs a NEGATIVE calculation case in which an unsupported amount (for example a negative one) is refused", label)
		}
	}
	return gaps
}
