package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const allowanceParams = `{"family":"AMOUNT","class":"ALLOWANCE_OR_THRESHOLD","amount":"12570","unit":"PER_YEAR"}`
const employeeNIParams = `{"family":"TAX_BANDS","class":"EMPLOYEE_CONTRIBUTION","bands":[{"up_to":"1048","rate":"0"},{"up_to":"4189","rate":"0.08"},{"up_to":null,"rate":"0.02"}],"rounding":{"mode":"HALF_UP","scale":2}}`

func TestParameters_AmountFamilyAndClasses(t *testing.T) {
	p, err := ParseRuleParameters([]byte(allowanceParams))
	require.NoError(t, err)
	assert.Equal(t, FamilyAmount, p.Family)
	assert.Equal(t, "12570", *p.Amount)
	_, err = ParseRuleParameters([]byte(employeeNIParams))
	require.NoError(t, err, "a class may accompany a rate family")
	// A band that stops charging above a cap expresses a contribution cap without a new family.
	capped := `{"family":"TAX_BANDS","class":"EMPLOYEE_CONTRIBUTION","bands":[{"up_to":"500","rate":"0"},{"up_to":"4000","rate":"0.1"},{"up_to":null,"rate":"0"}],"rounding":{"mode":"HALF_UP","scale":2}}`
	cp, err := ParseRuleParameters([]byte(capped))
	require.NoError(t, err)
	assert.Equal(t, "350.00", CalculateTax(cp, "4000").TaxAmount, "(4000-500) x 10%")
	assert.Equal(t, "350.00", CalculateTax(cp, "9000").TaxAmount, "earnings above the cap add nothing")

	for _, c := range ParameterClasses {
		assert.True(t, ValidParameterClass(c))
	}
	assert.False(t, ValidParameterClass("OTHER"))

	for name, raw := range map[string]string{
		"amount as a number":    `{"family":"AMOUNT","amount":12570,"unit":"PER_YEAR"}`,
		"negative amount":       `{"family":"AMOUNT","amount":"-1","unit":"PER_YEAR"}`,
		"missing amount":        `{"family":"AMOUNT","unit":"PER_YEAR"}`,
		"missing unit":          `{"family":"AMOUNT","amount":"1"}`,
		"unknown unit":          `{"family":"AMOUNT","amount":"1","unit":"PER_FORTNIGHT"}`,
		"amount with rounding":  `{"family":"AMOUNT","amount":"1","unit":"PER_YEAR","rounding":{"mode":"HALF_UP","scale":2}}`,
		"amount with a rate":    `{"family":"AMOUNT","amount":"1","unit":"PER_YEAR","rate":"0.1"}`,
		"unknown class":         `{"family":"AMOUNT","class":"BONUS","amount":"1","unit":"PER_YEAR"}`,
		"amount on a rate rule": `{"family":"TAX_RATE","rate":"0.2","amount":"1","rounding":{"mode":"HALF_UP","scale":2}}`,
		"unit on a rate rule":   `{"family":"TAX_RATE","rate":"0.2","unit":"PER_YEAR","rounding":{"mode":"HALF_UP","scale":2}}`,
	} {
		_, err := ParseRuleParameters([]byte(raw))
		assert.ErrorIs(t, err, ErrParametersInvalid, name)
	}
}

func TestCalculateTax_AFixedAmountIsAParameterNotACalculation(t *testing.T) {
	p, _ := ParseRuleParameters([]byte(allowanceParams))
	r := CalculateTax(p, "50000")
	assert.Equal(t, CalcParameterOnly, r.Outcome)
	assert.Empty(t, r.TaxAmount)
}

func payrollInput(params string) CompileInput {
	in := baseInput()
	in.Rules[0].RuleDomain = PayrollDomain
	in.Rules[0].RuleCode = "PERSONAL_ALLOWANCE"
	in.Rules[0].Parameters = json.RawMessage(params)
	return in
}

func TestCompile_PayrollRulesMustDeclareTheirStatutoryClass(t *testing.T) {
	ok := Compile(payrollInput(allowanceParams))
	require.False(t, ok.Report.HasErrors(), "%+v", ok.Report.Findings)

	noClass := Compile(payrollInput(`{"family":"AMOUNT","amount":"12570","unit":"PER_YEAR"}`))
	assert.Contains(t, codes(noClass.Report, SeverityError), "JUR-C091")
	// The same parameters on a non-payroll rule need no class.
	in := baseInput()
	in.Rules[0].Parameters = json.RawMessage(`{"family":"AMOUNT","amount":"1","unit":"PER_YEAR"}`)
	assert.False(t, Compile(in).Report.HasErrors())
}

func TestHarness_FixedAmountRulesNeedAGoldenCasePinningTheValue(t *testing.T) {
	out := Compile(payrollInput(allowanceParams))
	require.False(t, out.Report.HasErrors())
	doc, err := ParseArtifact(out.ArtifactJSON)
	require.NoError(t, err)
	assert.Equal(t, []string{"PERSONAL_ALLOWANCE"}, doc.RuleCodes(PayrollDomain))
	assert.Empty(t, doc.RuleCodes("TAX"))

	mk := func(parameterAmount any) []map[string]any {
		g := map[string]any{"outcome": OutcomeResolved, "rule_id": "rule-1"}
		if parameterAmount != nil {
			g["parameter_amount"] = parameterAmount
		}
		rc := func(id, class, jur string, at time.Time, e map[string]any) map[string]any {
			return map[string]any{"id": id, "class": class, "input": map[string]any{"jurisdiction": jur, "rule_domain": PayrollDomain, "rule_code": "PERSONAL_ALLOWANCE", "effective_at": rfc(at)}, "expect": e}
		}
		from := tm(2026, 1, 1)
		return []map[string]any{
			rc("g", ClassGolden, "GB", tm(2026, 9, 1), g),
			rc("b1", ClassBoundary, "GB", from, map[string]any{"outcome": OutcomeResolved, "rule_id": "rule-1"}),
			rc("b2", ClassBoundary, "GB", from.Add(-time.Nanosecond), map[string]any{"outcome": OutcomeNoRule}),
			rc("n", ClassNegative, "FR", tm(2026, 9, 1), map[string]any{"outcome": OutcomeUnsupported}),
		}
	}
	run := func(cases []map[string]any) TestRunResult {
		b, err := ParseTestBundle(bundleOf(cases...))
		require.NoError(t, err)
		return RunTestBundle(doc, b)
	}
	good := run(mk("12570"))
	assert.True(t, good.Passed, "%+v %+v", good.CoverageGaps, good.Results)

	// Without the pinned amount the value is untested: a coverage gap, even though every case passes.
	unpinned := run(mk(nil))
	assert.False(t, unpinned.Passed)
	assert.Equal(t, 0, unpinned.Failed)
	var t030 bool
	for _, g := range unpinned.CoverageGaps {
		t030 = t030 || g.Code == "JUR-T030"
	}
	assert.True(t, t030)

	// A wrong pinned amount fails the case.
	wrong := run(mk("12571"))
	assert.False(t, wrong.Passed)
	assert.Equal(t, 1, wrong.Failed)

	// parameter_amount is only for RESOLVED rule cases.
	_, err = ParseTestBundle(bundleOf(map[string]any{"id": "x", "class": ClassGolden, "input": map[string]any{"jurisdiction": "GB", "rule_domain": "TAX", "rule_code": "STD", "effective_at": rfc(tm(2026, 9, 1))},
		"expect": map[string]any{"outcome": OutcomeNoRule, "parameter_amount": "1"}}))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	_, err = ParseTestBundle(bundleOf(map[string]any{"id": "x", "class": ClassGolden, "input": map[string]any{"jurisdiction": "GB", "rule_domain": "TAX", "rule_code": "STD", "effective_at": rfc(tm(2026, 9, 1))},
		"expect": map[string]any{"outcome": OutcomeResolved, "rule_id": "r", "parameter_amount": "1,0"}}))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	_ = strings.TrimSpace
}
