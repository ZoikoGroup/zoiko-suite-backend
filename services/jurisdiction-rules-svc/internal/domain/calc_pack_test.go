package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bandedParams = `{"family":"TAX_BANDS","bands":[{"up_to":"1000","rate":"0.1"},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`

func paramPackInput() CompileInput {
	in := baseInput()
	in.Rules[0].Parameters = json.RawMessage(bandedParams)
	return in
}

func paramDoc(t *testing.T) *ArtifactDoc {
	t.Helper()
	out := Compile(paramPackInput())
	require.False(t, out.Report.HasErrors(), "%+v", out.Report.Findings)
	doc, err := ParseArtifact(out.ArtifactJSON)
	require.NoError(t, err)
	return doc
}

func TestCompile_ParametersTravelInTheArtifactAndAreDigested(t *testing.T) {
	out := Compile(paramPackInput())
	require.False(t, out.Report.HasErrors(), "%+v", out.Report.Findings)
	var art map[string]any
	require.NoError(t, json.Unmarshal(out.ArtifactJSON, &art))
	mod := art["rule_modules"].([]any)[0].(map[string]any)
	assert.Equal(t, "TAX_BANDS", mod["parameters"].(map[string]any)["family"])

	// A changed rate changes the artifact digest (and the rule's own content digest).
	in := paramPackInput()
	in.Rules[0].Parameters = json.RawMessage(strings.Replace(bandedParams, `"rate":"0.2"`, `"rate":"0.21"`, 1))
	assert.NotEqual(t, out.ArtifactDigest, Compile(in).ArtifactDigest)

	// A rule without parameters carries an explicit null, and the artifact differs from one with parameters.
	plain := Compile(baseInput())
	var pa map[string]any
	require.NoError(t, json.Unmarshal(plain.ArtifactJSON, &pa))
	assert.Nil(t, pa["rule_modules"].([]any)[0].(map[string]any)["parameters"])
	assert.NotEqual(t, plain.ArtifactDigest, out.ArtifactDigest)
}

func TestCompile_RejectsInvalidParameters(t *testing.T) {
	for name, params := range map[string]string{
		"rate as a number":   `{"family":"TAX_RATE","rate":0.2,"rounding":{"mode":"HALF_UP","scale":2}}`,
		"rate above 100%":    `{"family":"TAX_RATE","rate":"1.2","rounding":{"mode":"HALF_UP","scale":2}}`,
		"unknown family":     `{"family":"FORMULA","rounding":{"mode":"HALF_UP","scale":2}}`,
		"bounded last band":  `{"family":"TAX_BANDS","bands":[{"up_to":"1","rate":"0"},{"up_to":"2","rate":"0.1"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"unknown field":      `{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2},"formula":"x*0.2"}`,
		"float outside rate": `{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2.5}}`,
	} {
		in := paramPackInput()
		in.Rules[0].Parameters = json.RawMessage(params)
		out := Compile(in)
		assert.Contains(t, codes(out.Report, SeverityError), "JUR-C090", name)
		assert.Empty(t, out.ArtifactJSON, name)
	}
}

func TestArtifact_CalculatesWithTheResolvedRulesParameters(t *testing.T) {
	doc := paramDoc(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	c := doc.Calculate("GB", "TAX", "STD", at, "1500")
	require.Equal(t, CalcCalculated, c.Outcome, "%+v", c.Tax)
	assert.Equal(t, "200.00", c.Tax.TaxAmount, "1000 x 10% + 500 x 20%")
	assert.Equal(t, "rule-1", c.RuleID)

	assert.Equal(t, CalcInvalidFacts, doc.Calculate("GB", "TAX", "STD", at, "-5").Outcome)
	assert.Equal(t, OutcomeNoRule, doc.Calculate("GB", "TAX", "STD", time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), "100").Outcome)
	assert.Equal(t, OutcomeUnsupported, doc.Calculate("FR", "TAX", "STD", at, "100").Outcome)

	// A rule with no parameters is never given a guessed rate.
	plain, err := ParseArtifact(Compile(baseInput()).ArtifactJSON)
	require.NoError(t, err)
	c = plain.Calculate("GB", "TAX", "STD", at, "100")
	assert.Equal(t, CalcNoParameters, c.Outcome)
	assert.Nil(t, c.Tax)
}

// ── harness ─────────────────────────────────────────────────────────────────

func calcCase(id, class string, in map[string]any, expect map[string]any) map[string]any {
	base := map[string]any{"jurisdiction": "GB", "rule_domain": "TAX", "rule_code": "STD", "effective_at": rfc(tm(2026, 9, 1))}
	for k, v := range in {
		base[k] = v
	}
	return map[string]any{"id": id, "class": class, "input": base, "expect": expect}
}

func calcBundleParts() []map[string]any {
	from := tm(2026, 1, 1)
	return []map[string]any{
		// the rule-resolution coverage every rule needs
		caseJSON("rg", ClassGolden, "GB", rfc(tm(2026, 9, 1)), resolved("rule-1")),
		caseJSON("rb1", ClassBoundary, "GB", rfc(from), resolved("rule-1")),
		caseJSON("rb2", ClassBoundary, "GB", rfc(from.Add(-time.Nanosecond)), map[string]any{"outcome": OutcomeNoRule}),
		caseJSON("rn", ClassNegative, "FR", rfc(tm(2026, 9, 1)), map[string]any{"outcome": OutcomeUnsupported}),
		// the calculation coverage
		calcCase("cg", ClassGolden, map[string]any{"taxable_amount": "1500"}, map[string]any{"outcome": CalcCalculated, "tax_amount": "200.00", "rule_id": "rule-1"}),
		calcCase("cb1", ClassBoundary, map[string]any{"taxable_amount": "1000"}, map[string]any{"outcome": CalcCalculated, "tax_amount": "100.00"}),
		calcCase("cb2", ClassBoundary, map[string]any{"taxable_amount": "1000.01"}, map[string]any{"outcome": CalcCalculated, "tax_amount": "100.00"}), // 100.002 is rounded
		calcCase("cn", ClassNegative, map[string]any{"taxable_amount": "-5"}, map[string]any{"outcome": CalcInvalidFacts}),
	}
}

func TestHarness_CalculationCases_PassWithCompleteCoverage(t *testing.T) {
	b, err := ParseTestBundle(bundleOf(calcBundleParts()...))
	require.NoError(t, err)
	r := RunTestBundle(paramDoc(t), b)
	assert.True(t, r.Passed, "%+v %+v", r.CoverageGaps, r.Results)
	assert.Equal(t, 8, r.Total)
	assert.Equal(t, 0, r.Failed)
	assert.Equal(t, "CALCULATED:200.00@rule-1", r.Results[4].Actual)
}

func TestHarness_CalculationCoverageGapsAreSpecific(t *testing.T) {
	parts := calcBundleParts()
	// Keep the rule coverage, drop every calculation case except the golden one.
	only := append(parts[:4:4], parts[4])
	b, err := ParseTestBundle(bundleOf(only...))
	require.NoError(t, err)
	r := RunTestBundle(paramDoc(t), b)
	assert.False(t, r.Passed)
	got := map[string]int{}
	for _, g := range r.CoverageGaps {
		got[g.Code]++
	}
	assert.Equal(t, 0, got["JUR-T020"])
	assert.Equal(t, 1, got["JUR-T021"], "the 1000 band limit needs its own boundary case")
	assert.Equal(t, 1, got["JUR-T022"], "no case exercises the rounding")
	assert.Equal(t, 1, got["JUR-T023"], "no negative amount case")

	// A boundary case that is not exactly the limit does not satisfy the requirement.
	near := append(parts[:4:4], parts[4], calcCase("cb1", ClassBoundary, map[string]any{"taxable_amount": "999.99"}, map[string]any{"outcome": CalcCalculated, "tax_amount": "100.00"}))
	b, _ = ParseTestBundle(bundleOf(near...))
	r = RunTestBundle(paramDoc(t), b)
	var t021 bool
	for _, g := range r.CoverageGaps {
		t021 = t021 || g.Code == "JUR-T021"
	}
	assert.True(t, t021)
}

func TestHarness_AWrongExpectedTaxFailsTheCase(t *testing.T) {
	parts := calcBundleParts()
	parts[4] = calcCase("cg", ClassGolden, map[string]any{"taxable_amount": "1500"}, map[string]any{"outcome": CalcCalculated, "tax_amount": "199.99"})
	b, err := ParseTestBundle(bundleOf(parts...))
	require.NoError(t, err)
	r := RunTestBundle(paramDoc(t), b)
	assert.False(t, r.Passed)
	assert.Equal(t, 1, r.Failed)
	assert.Equal(t, "CALCULATED:199.99", r.Results[4].Expected)
	assert.Equal(t, "CALCULATED:200.00@rule-1", r.Results[4].Actual)
}

func TestParseTestBundle_CalculationCaseRules(t *testing.T) {
	ok := calcCase("c1", ClassGolden, map[string]any{"taxable_amount": "10"}, map[string]any{"outcome": CalcCalculated, "tax_amount": "1.00"})
	_, err := ParseTestBundle(bundleOf(ok))
	require.NoError(t, err)
	for name, c := range map[string]map[string]any{
		"calculated without tax": calcCase("c1", ClassGolden, map[string]any{"taxable_amount": "10"}, map[string]any{"outcome": CalcCalculated}),
		"tax on a failure":       calcCase("c1", ClassNegative, map[string]any{"taxable_amount": "-1"}, map[string]any{"outcome": CalcInvalidFacts, "tax_amount": "0"}),
		"bad tax":                calcCase("c1", ClassGolden, map[string]any{"taxable_amount": "10"}, map[string]any{"outcome": CalcCalculated, "tax_amount": "1,00"}),
		"resolution outcome":     calcCase("c1", ClassGolden, map[string]any{"taxable_amount": "10"}, map[string]any{"outcome": OutcomeResolved, "rule_id": "r"}),
		"missing rule code":      calcCase("c1", ClassGolden, map[string]any{"taxable_amount": "10", "rule_code": ""}, map[string]any{"outcome": CalcNoParameters}),
		"mixed with obligation":  calcCase("c1", ClassGolden, map[string]any{"taxable_amount": "10", "obligation_code": "VAT_RETURN"}, map[string]any{"outcome": CalcNoParameters}),
		"bad instant":            calcCase("c1", ClassGolden, map[string]any{"taxable_amount": "10", "effective_at": "2026-09-01"}, map[string]any{"outcome": CalcNoParameters}),
	} {
		_, err := ParseTestBundle(bundleOf(c))
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
	}
	// tax_amount without taxable_amount is a rule case with a stray field: refused, not ignored.
	stray := caseJSON("c1", ClassGolden, "GB", rfc(tm(2026, 9, 1)), map[string]any{"outcome": OutcomeNoRule, "tax_amount": "1.00"})
	_, err = ParseTestBundle(bundleOf(stray))
	assert.ErrorIs(t, err, ErrBundleInvalid)
}
