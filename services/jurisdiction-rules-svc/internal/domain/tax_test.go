package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func flat(rate, mode string, scale int) RuleParameters {
	return RuleParameters{Family: FamilyTaxRate, Rate: &rate, Rounding: Rounding{Mode: mode, Scale: scale}}
}

func bands(mode string, scale int, spec ...string) RuleParameters {
	// spec: "limit:rate" pairs; "-" as limit means no upper limit.
	var bs []BandSpec
	for _, s := range spec {
		parts := strings.SplitN(s, ":", 2)
		b := BandSpec{Rate: parts[1]}
		if parts[0] != "-" {
			l := parts[0]
			b.UpTo = &l
		}
		bs = append(bs, b)
	}
	return RuleParameters{Family: FamilyTaxBands, Bands: bs, Rounding: Rounding{Mode: mode, Scale: scale}}
}

func TestCalculateTax_FlatRateIsExact(t *testing.T) {
	for _, c := range []struct{ amount, rate, want string }{
		{"100", "0.2", "20.00"},
		{"0", "0.2", "0.00"},
		{"0.30", "0.1", "0.03"},
		{"1234567890123.45", "0.2", "246913578024.69"}, // large amounts stay exact (binary floating point would not)
		{"999999999999999", "1", "999999999999999.00"},
		{"19.99", "0.0825", "1.65"}, // 1.649175 rounds half-up to 1.65
	} {
		r := CalculateTax(flat(c.rate, RoundHalfUp, 2), c.amount)
		require.Equal(t, CalcCalculated, r.Outcome, "%s: %v", c.amount, r.Steps)
		assert.Equal(t, c.want, r.TaxAmount, c.amount+" x "+c.rate)
	}
}

func TestCalculateTax_TheClassicFloatTraps(t *testing.T) {
	// 0.1 + 0.2 != 0.3 in binary floating point; here the tax on 0.30 at 10% and the sum of two
	// separately calculated slices agree exactly.
	a := CalculateTax(flat("0.1", RoundHalfUp, 6), "0.3")
	b := CalculateTax(bands(RoundHalfUp, 6, "0.1:0.1", "-:0.1"), "0.3")
	assert.Equal(t, "0.030000", a.TaxAmount)
	assert.Equal(t, a.TaxAmount, b.TaxAmount, "a flat rate and a band split at the same rate agree to the last digit")
	// 1.005 is stored as 1.00499999999999989... in float64, so a float implementation rounds it to 1.00;
	// exact arithmetic sees a true tie and rounds half up to 1.01.
	assert.Equal(t, "1.01", CalculateTax(flat("1", RoundHalfUp, 2), "1.005").TaxAmount)
}

func TestCalculateTax_RoundingModesAreExplicitAndApplyOnce(t *testing.T) {
	cases := []struct {
		amount, mode, want string
		scale              int
		rounded            bool
	}{
		{"0.25", RoundHalfUp, "0.3", 1, true},
		{"0.25", RoundHalfEven, "0.2", 1, true}, // tie to the even digit
		{"0.35", RoundHalfEven, "0.4", 1, true}, // tie to the even digit
		{"0.25", RoundUp, "0.3", 1, true},
		{"0.25", RoundDown, "0.2", 1, true},
		{"0.249", RoundHalfUp, "0.25", 2, true},
		{"0.2", RoundHalfUp, "0.2", 1, false}, // exact: nothing was rounded
		{"0.201", RoundUp, "0.21", 2, true},   // UP is a ceiling
		{"0.209", RoundDown, "0.20", 2, true}, // DOWN is a floor
		{"2.5", RoundHalfEven, "2", 0, true},
		{"3.5", RoundHalfEven, "4", 0, true},
	}
	for _, c := range cases {
		r := CalculateTax(flat("1", c.mode, c.scale), c.amount)
		require.Equal(t, CalcCalculated, r.Outcome)
		assert.Equal(t, c.want, r.TaxAmount, "%s %s", c.amount, c.mode)
		assert.Equal(t, c.rounded, r.Rounded, "%s %s", c.amount, c.mode)
	}

	// Rounding is applied once to the total, not per band: three slices of 0.004 each would
	// round to 0.00 individually but their sum 0.012 rounds to 0.01.
	b := bands(RoundHalfUp, 2, "1:0.004", "2:0.004", "-:0.004")
	r := CalculateTax(b, "3")
	assert.Equal(t, "0.01", r.TaxAmount)
	assert.Equal(t, "0.012", r.Unrounded)
}

func TestCalculateTax_MarginalBands_AndTheirBoundaries(t *testing.T) {
	p := bands(RoundHalfUp, 2, "12570:0", "50270:0.2", "-:0.4")
	for amount, want := range map[string]string{
		"0":        "0.00",
		"12570":    "0.00", // the limit itself belongs to the lower band
		"12570.01": "0.00", // 0.01 x 20% = 0.002 rounds to 0.00
		"12571":    "0.20",
		"50270":    "7540.00", // 37700 x 20%
		"50270.01": "7540.00", // 0.01 x 40% = 0.004
		"50271":    "7540.40",
		"60000":    "11432.00", // 7540 + 9730 x 40%
	} {
		r := CalculateTax(p, amount)
		require.Equal(t, CalcCalculated, r.Outcome, amount)
		assert.Equal(t, want, r.TaxAmount, amount)
	}
	r := CalculateTax(p, "60000")
	assert.Len(t, r.Steps, 4, "one explained step per band plus the rounding step: %v", r.Steps)
	assert.Contains(t, r.Steps[1], "50270")
}

func TestCalculateTax_RefusesAmountsItCannotCalculateExactly(t *testing.T) {
	for _, bad := range []string{"-1", "-0.01", "1e3", "1.", ".5", "01", "abc", "", " 1", "1,000", "1.1234567890123", "1234567890123456", "NaN", "+5"} {
		r := CalculateTax(flat("0.2", RoundHalfUp, 2), bad)
		assert.Equal(t, CalcInvalidFacts, r.Outcome, "%q", bad)
		assert.Empty(t, r.TaxAmount)
	}
	r := CalculateTax(RuleParameters{Family: "OTHER"}, "1")
	assert.Equal(t, CalcInvalidFacts, r.Outcome, "invalid parameters never calculate")
}

func TestParseRuleParameters_IsStrict(t *testing.T) {
	good := `{"family":"TAX_RATE","rate":"0.2000","rounding":{"mode":"HALF_UP","scale":2}}`
	p, err := ParseRuleParameters([]byte(good))
	require.NoError(t, err)
	assert.Equal(t, FamilyTaxRate, p.Family)
	_, err = ParseRuleParameters([]byte(`{"family":"TAX_BANDS","bands":[{"up_to":"100","rate":"0"},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_EVEN","scale":0}}`))
	require.NoError(t, err)

	for name, raw := range map[string]string{
		"not json":          `x`,
		"unknown field":     `{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2},"extra":1}`,
		"trailing":          good + `{}`,
		"rate as number":    `{"family":"TAX_RATE","rate":0.2,"rounding":{"mode":"HALF_UP","scale":2}}`,
		"rate above 1":      `{"family":"TAX_RATE","rate":"1.5","rounding":{"mode":"HALF_UP","scale":2}}`,
		"rate negative":     `{"family":"TAX_RATE","rate":"-0.1","rounding":{"mode":"HALF_UP","scale":2}}`,
		"rate exponent":     `{"family":"TAX_RATE","rate":"2e-1","rounding":{"mode":"HALF_UP","scale":2}}`,
		"missing rate":      `{"family":"TAX_RATE","rounding":{"mode":"HALF_UP","scale":2}}`,
		"flat with bands":   `{"family":"TAX_RATE","rate":"0.2","bands":[{"up_to":null,"rate":"0.1"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"bad family":        `{"family":"FORMULA","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2}}`,
		"lower-case mode":   `{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"half_up","scale":2}}`,
		"scale 7":           `{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":7}}`,
		"negative scale":    `{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":-1}}`,
		"no rounding":       `{"family":"TAX_RATE","rate":"0.2"}`,
		"one band":          `{"family":"TAX_BANDS","bands":[{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"last band bounded": `{"family":"TAX_BANDS","bands":[{"up_to":"100","rate":"0"},{"up_to":"200","rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"middle unbounded":  `{"family":"TAX_BANDS","bands":[{"up_to":null,"rate":"0"},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"not increasing":    `{"family":"TAX_BANDS","bands":[{"up_to":"200","rate":"0"},{"up_to":"100","rate":"0.1"},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"equal limits":      `{"family":"TAX_BANDS","bands":[{"up_to":"100","rate":"0"},{"up_to":"100","rate":"0.1"},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"zero limit":        `{"family":"TAX_BANDS","bands":[{"up_to":"0","rate":"0"},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"band flat rate":    `{"family":"TAX_BANDS","rate":"0.1","bands":[{"up_to":"100","rate":"0"},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
		"band rate number":  `{"family":"TAX_BANDS","bands":[{"up_to":"100","rate":0},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`,
	} {
		_, err := ParseRuleParameters([]byte(raw))
		assert.ErrorIs(t, err, ErrParametersInvalid, name)
	}
}
