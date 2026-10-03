package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tm(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// twoRuleArtifact: GB has STD v1 [2026-01-01, 2026-06-01) superseded by v2 from 2026-06-01.
func twoRuleArtifact() *ArtifactDoc {
	jun := tm(2026, 6, 1)
	return &ArtifactDoc{
		PackRef: "jur.gb.tax.core", PackVersion: "1.0",
		Jurisdictions: []ArtifactScopeEntry{{ID: "j-gb", Code: "GB"}},
		Rules: []ArtifactRule{
			{RuleID: "r1", JurisdictionID: "j-gb", RuleDomain: "TAX", RuleCode: "STD", EffectiveFrom: tm(2026, 1, 1), EffectiveTo: &jun, ContentDigest: "sha256:r1"},
			{RuleID: "r2", JurisdictionID: "j-gb", RuleDomain: "TAX", RuleCode: "STD", EffectiveFrom: jun, SupersedesRuleID: ptr("r1"), ContentDigest: "sha256:r2"},
		},
	}
}

func TestResolve_IsEffectiveDated_AndHalfOpen(t *testing.T) {
	a := twoRuleArtifact()
	cases := []struct {
		at   time.Time
		want string
		id   string
	}{
		{tm(2025, 12, 31), OutcomeNoRule, ""},                         // before anything exists
		{tm(2026, 1, 1), OutcomeResolved, "r1"},                       // start is inclusive
		{tm(2026, 3, 1), OutcomeResolved, "r1"},                       // JUR-NEG-01/07: the old rule serves its own period
		{tm(2026, 6, 1).Add(-time.Nanosecond), OutcomeResolved, "r1"}, // last instant of r1
		{tm(2026, 6, 1), OutcomeResolved, "r2"},                       // end is exclusive; r2 starts exactly here
		{tm(2030, 1, 1), OutcomeResolved, "r2"},                       // open ended
	}
	for _, c := range cases {
		got := a.Resolve("GB", "TAX", "STD", c.at)
		assert.Equal(t, c.want, got.Outcome, c.at.String())
		assert.Equal(t, c.id, got.RuleID, c.at.String())
	}
	assert.Equal(t, OutcomeNoRule, a.Resolve("GB", "TAX", "OTHER", tm(2026, 3, 1)).Outcome)
	assert.Equal(t, OutcomeNoRule, a.Resolve("GB", "PAYROLL", "STD", tm(2026, 3, 1)).Outcome)
}

func TestResolve_UnsupportedJurisdictionIsExplicit_NotGuessed(t *testing.T) {
	got := twoRuleArtifact().Resolve("FR", "TAX", "STD", tm(2026, 3, 1))
	assert.Equal(t, OutcomeUnsupported, got.Outcome, "JUR-NEG-22")
	assert.Empty(t, got.RuleID)
}

func TestResolve_HierarchyAndDeclaredPrecedenceOnly(t *testing.T) {
	mk := func(nat, st *int, stSupersedesNat bool) *ArtifactDoc {
		a := &ArtifactDoc{
			Jurisdictions: []ArtifactScopeEntry{{ID: "j-us", Code: "US"}, {ID: "j-ca", Code: "US-CA", ParentID: ptr("j-us")}},
			Rules: []ArtifactRule{
				{RuleID: "nat", JurisdictionID: "j-us", RuleDomain: "TAX", RuleCode: "SALES", EffectiveFrom: tm(2026, 1, 1), Precedence: nat},
				{RuleID: "st", JurisdictionID: "j-ca", RuleDomain: "TAX", RuleCode: "SALES", EffectiveFrom: tm(2026, 1, 1), Precedence: st},
			},
		}
		if stSupersedesNat {
			a.Rules[1].SupersedesRuleID = ptr("nat")
		}
		return a
	}
	at := tm(2026, 3, 1)

	// The state asks: both apply (state inherits the national rule). Declared precedence decides.
	assert.Equal(t, "st", mk(ptr(1), ptr(2), false).Resolve("US-CA", "TAX", "SALES", at).RuleID)
	assert.Equal(t, "nat", mk(ptr(5), ptr(2), false).Resolve("US-CA", "TAX", "SALES", at).RuleID, "national can outrank when declared (s10 national invariant)")
	// No implicit "most specific wins": undeclared overlap is ambiguous.
	amb := mk(nil, nil, false).Resolve("US-CA", "TAX", "SALES", at)
	assert.Equal(t, OutcomeAmbiguous, amb.Outcome)
	assert.Equal(t, []string{"nat", "st"}, amb.Candidates)
	assert.Equal(t, OutcomeAmbiguous, mk(ptr(1), ptr(1), false).Resolve("US-CA", "TAX", "SALES", at).Outcome, "equal precedence is not a declaration")
	// An explicit supersedes link is a declaration.
	assert.Equal(t, "st", mk(nil, nil, true).Resolve("US-CA", "TAX", "SALES", at).RuleID)
	// The national jurisdiction never sees a state rule.
	assert.Equal(t, "nat", mk(nil, nil, false).Resolve("US", "TAX", "SALES", at).RuleID)
}

func TestParseArtifact_RoundTripsACompiledArtifact(t *testing.T) {
	in := baseInput()
	in.Jurisdictions = []Named{{ID: "j-gb", Code: "GB"}, {ID: "j-eng", Code: "GB-ENG", ParentID: ptr("j-gb")}}
	out := Compile(in)
	require.False(t, out.Report.HasErrors(), "%+v", out.Report.Findings)
	doc, err := ParseArtifact(out.ArtifactJSON)
	require.NoError(t, err)
	assert.Equal(t, "jur.gb.tax.core", doc.PackRef)
	require.Len(t, doc.Rules, 1)
	got := doc.Resolve("GB-ENG", "TAX", "STD", tm(2026, 9, 1))
	assert.Equal(t, "rule-1", got.RuleID, "an England query inherits the GB rule through the artifact's own parent links")
	assert.Regexp(t, `^sha256:`, got.ContentDigest)
	_, err = ParseArtifact([]byte(`{"format":"other"}`))
	assert.Error(t, err)
	_, err = ParseArtifact([]byte(`nope`))
	assert.Error(t, err)
}

// ── bundle ──────────────────────────────────────────────────────────────────

func caseJSON(id, class, juris, at string, expect map[string]any) map[string]any {
	return map[string]any{"id": id, "class": class, "input": map[string]any{"jurisdiction": juris, "rule_domain": "TAX", "rule_code": "STD", "effective_at": at}, "expect": expect}
}

func resolved(id string) map[string]any {
	return map[string]any{"outcome": OutcomeResolved, "rule_id": id}
}

func bundleOf(cases ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"bundle_version": "1", "cases": cases})
	return b
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// completeBundle satisfies every coverage requirement of twoRuleArtifact.
func completeBundle() []byte {
	jun := tm(2026, 6, 1)
	return bundleOf(
		caseJSON("g1", ClassGolden, "GB", rfc(tm(2026, 3, 1)), resolved("r1")),
		caseJSON("g2", ClassGolden, "GB", rfc(tm(2026, 9, 1)), resolved("r2")),
		caseJSON("b1", ClassBoundary, "GB", rfc(tm(2026, 1, 1)), resolved("r1")),
		caseJSON("b2", ClassBoundary, "GB", rfc(tm(2026, 1, 1).Add(-time.Nanosecond)), map[string]any{"outcome": OutcomeNoRule}),
		caseJSON("b3", ClassBoundary, "GB", rfc(jun), resolved("r2")),
		caseJSON("b4", ClassBoundary, "GB", rfc(jun.Add(-time.Nanosecond)), resolved("r1")),
		caseJSON("h1", ClassHistorical, "GB", rfc(tm(2026, 2, 1)), resolved("r1")),
		caseJSON("n1", ClassNegative, "FR", rfc(tm(2026, 3, 1)), map[string]any{"outcome": OutcomeUnsupported}),
	)
}

func TestParseTestBundle_RejectsMalformed(t *testing.T) {
	good := caseJSON("g1", ClassGolden, "GB", rfc(tm(2026, 3, 1)), resolved("r1"))
	mut := func(f func(map[string]any)) []byte {
		c := map[string]any{}
		b, _ := json.Marshal(good)
		_ = json.Unmarshal(b, &c)
		f(c)
		return bundleOf(c)
	}
	_, err := ParseTestBundle(bundleOf(good))
	require.NoError(t, err)

	bad := map[string][]byte{
		"not json":           []byte(`x`),
		"wrong version":      []byte(`{"bundle_version":"2","cases":[]}`),
		"no cases":           []byte(`{"bundle_version":"1","cases":[]}`),
		"unknown field":      []byte(`{"bundle_version":"1","cases":[],"extra":1}`),
		"bad class":          mut(func(c map[string]any) { c["class"] = "PERFORMANCE" }),
		"bad id":             mut(func(c map[string]any) { c["id"] = "has space" }),
		"bad instant":        mut(func(c map[string]any) { c["input"].(map[string]any)["effective_at"] = "2026-03-01" }),
		"blank jurisdiction": mut(func(c map[string]any) { c["input"].(map[string]any)["jurisdiction"] = " " }),
		"resolved no rule":   mut(func(c map[string]any) { c["expect"] = map[string]any{"outcome": OutcomeResolved} }),
		"no_rule with rule":  mut(func(c map[string]any) { c["expect"] = map[string]any{"outcome": OutcomeNoRule, "rule_id": "r1"} }),
		"bad outcome":        mut(func(c map[string]any) { c["expect"] = map[string]any{"outcome": "MAYBE"} }),
		"duplicate ids":      bundleOf(good, good),
	}
	for name, raw := range bad {
		_, err := ParseTestBundle(raw)
		require.Error(t, err, name)
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
	}
	big := make([]map[string]any, maxBundleCases+1)
	for i := range big {
		big[i] = caseJSON(fmt.Sprintf("c%d", i), ClassGolden, "GB", rfc(tm(2026, 3, 1)), resolved("r1"))
	}
	_, err = ParseTestBundle(bundleOf(big...))
	assert.Error(t, err, "bounded size")
}

func TestRunTestBundle_PassesOnlyWithCompleteCoverage(t *testing.T) {
	b, err := ParseTestBundle(completeBundle())
	require.NoError(t, err)
	r := RunTestBundle(twoRuleArtifact(), b)
	assert.True(t, r.Passed, "%+v", r.CoverageGaps)
	assert.Equal(t, 8, r.Total)
	assert.Equal(t, 0, r.Failed)
	assert.True(t, r.CoverageComplete)
	assert.Equal(t, 2, r.ByClass[ClassGolden])
	assert.Equal(t, 4, r.ByClass[ClassBoundary])
	assert.Contains(t, r.ClassesNotImplemented, "REGRESSION_CORPUS", "the run states what it did not prove")
	assert.Equal(t, HarnessVersion, r.HarnessVersion)
}

func TestRunTestBundle_FailingCaseFailsTheRun(t *testing.T) {
	raw := strings.Replace(string(completeBundle()), `"rule_id":"r1"`, `"rule_id":"r2"`, 1) // g1 now expects the wrong rule
	b, err := ParseTestBundle([]byte(raw))
	require.NoError(t, err)
	r := RunTestBundle(twoRuleArtifact(), b)
	assert.False(t, r.Passed)
	assert.Equal(t, 1, r.Failed)
	var failed CaseResult
	for _, c := range r.Results {
		if !c.Passed {
			failed = c
		}
	}
	assert.Equal(t, "r2", strings.TrimPrefix(failed.Expected, "RESOLVED:"))
	assert.Equal(t, "RESOLVED:r1", failed.Actual)
}

func TestRunTestBundle_CoverageGapsNameWhatIsMissing(t *testing.T) {
	// One golden case only.
	b, err := ParseTestBundle(bundleOf(caseJSON("g1", ClassGolden, "GB", rfc(tm(2026, 3, 1)), resolved("r1"))))
	require.NoError(t, err)
	r := RunTestBundle(twoRuleArtifact(), b)
	assert.False(t, r.Passed, "all cases pass but coverage is incomplete")
	assert.Equal(t, 0, r.Failed)
	codes := map[string]int{}
	for _, g := range r.CoverageGaps {
		codes[g.Code]++
	}
	assert.Equal(t, 1, codes["JUR-T001"], "r2 has no golden case")
	assert.Equal(t, 6, codes["JUR-T002"], "r1 needs 4 instants (from, from-1ns, to, to-1ns), r2 needs 2; a single case can satisfy both rules when instants coincide")
	assert.Equal(t, 1, codes["JUR-T003"], "the supersession needs a historical case")
	assert.Equal(t, 1, codes["JUR-T004"], "no unsupported-jurisdiction negative case")
	// The message tells the author the exact instant to add.
	var found bool
	for _, g := range r.CoverageGaps {
		if g.Code == "JUR-T002" && strings.Contains(g.Message, rfc(tm(2026, 6, 1))) {
			found = true
		}
	}
	assert.True(t, found)
}
