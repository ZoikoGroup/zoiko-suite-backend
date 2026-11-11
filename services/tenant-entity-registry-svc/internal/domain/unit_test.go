package domain_test

import (
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func TestParseFactor_ExactDecimalsAndFractions(t *testing.T) {
	for _, in := range []string{"1000", "1.609344", "1/3", "1609344/1000000", "0.0000000000000001"} {
		r, err := domain.ParseFactor(in)
		require.NoError(t, err, in)
		assert.Equal(t, 1, r.Sign(), in)
	}
	r, err := domain.ParseFactor("1.609344")
	require.NoError(t, err)
	frac, _ := domain.ParseFactor("1609344/1000000")
	assert.Zero(t, r.Cmp(frac), "a decimal and the same fraction are one factor, exactly")
	third, _ := domain.ParseFactor("1/3")
	assert.Equal(t, "1/3", domain.FractionString(third), "a fraction is kept, never turned into a float")
	twoSixths, _ := domain.ParseFactor("2/6")
	assert.Equal(t, "1/3", domain.FractionString(twoSixths), "a factor is stored reduced")
}

func TestParseFactor_Refusals(t *testing.T) {
	for _, in := range []string{"", "0", "0.0", "-1", "-1/2", "1/0", "abc", "1e3", "1.", ".5", "1/2/3", "1/-2", "1,5", "+", " ",
		strings.Repeat("9", domain.MaxFactorDigits+1), "1/" + strings.Repeat("9", domain.MaxFactorDigits+1)} {
		_, err := domain.ParseFactor(in)
		assert.Error(t, err, "%q", in)
	}
}

func TestParseQuantity(t *testing.T) {
	for _, in := range []string{"0", "12", "-3.5", "0.000001", "+4"} {
		_, err := domain.ParseQuantity(in)
		assert.NoError(t, err, in)
	}
	for _, in := range []string{"", "1/3", "1e3", "abc", ".5", strings.Repeat("1", domain.MaxQuantityDigits+1)} {
		_, err := domain.ParseQuantity(in)
		assert.Error(t, err, "%q", in)
	}
}

func TestRoundHalfUp(t *testing.T) {
	rat := func(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }
	for _, tc := range []struct {
		in       string
		decimals int
		want     string
	}{
		{"5/2", 0, "3"},
		{"-5/2", 0, "-3"},
		{"1/3", 6, "0.333333"},
		{"2/3", 6, "0.666667"},
		{"1005/1000", 2, "1.01"},
		{"-1005/1000", 2, "-1.01"},
		{"2/5", 0, "0"},
		{"-4/10000000", 6, "0.000000"},
		{"3", 2, "3.00"},
		{"1/8", 2, "0.13"},
		{"1/1000000", 6, "0.000001"},
		{"123456789/1000", 3, "123456.789"},
	} {
		assert.Equal(t, tc.want, domain.RoundHalfUp(rat(tc.in), tc.decimals), "%s to %d", tc.in, tc.decimals)
	}
}

func TestConvertExact_NoFloatEverTouchesAQuantity(t *testing.T) {
	km, _ := domain.ParseFactor("1000")
	mile, _ := domain.ParseFactor("1.609344")
	q, _ := domain.ParseQuantity("0.1")
	// 0.1 mile to km is exactly 0.1609344, which no float holds.
	out := domain.ConvertExact(q, mile, km)
	assert.Equal(t, "0.0001609344", domain.RoundHalfUp(out, 10))
	// A third, then back: exact arithmetic returns precisely what went in.
	third, _ := domain.ParseFactor("1/3")
	one, _ := domain.ParseFactor("1")
	there := domain.ConvertExact(big.NewRat(1, 1), third, one)
	back := domain.ConvertExact(there, one, third)
	assert.Zero(t, back.Cmp(big.NewRat(1, 1)))
}

func TestCreateQuantityDimensionRequest_Validate(t *testing.T) {
	good := domain.CreateQuantityDimensionRequest{DimensionCode: "LENGTH", Name: "Length", BaseUnit: domain.UnitFields{UnitCode: "M", Name: "Metre", Symbol: "m"}}
	require.NoError(t, good.Validate())
	for name, mut := range map[string]func(*domain.CreateQuantityDimensionRequest){
		"lower code":  func(r *domain.CreateQuantityDimensionRequest) { r.DimensionCode = "length" },
		"short code":  func(r *domain.CreateQuantityDimensionRequest) { r.DimensionCode = "L" },
		"no name":     func(r *domain.CreateQuantityDimensionRequest) { r.Name = " " },
		"long desc":   func(r *domain.CreateQuantityDimensionRequest) { r.Description = strings.Repeat("x", 1001) },
		"bad unit":    func(r *domain.CreateQuantityDimensionRequest) { r.BaseUnit.UnitCode = "m" },
		"no symbol":   func(r *domain.CreateQuantityDimensionRequest) { r.BaseUnit.Symbol = "" },
		"decimals 19": func(r *domain.CreateQuantityDimensionRequest) { d := 19; r.BaseUnit.Decimals = &d },
		"decimals -1": func(r *domain.CreateQuantityDimensionRequest) { d := -1; r.BaseUnit.Decimals = &d },
	} {
		r := good
		mut(&r)
		assert.Error(t, r.Validate(), name)
	}
	assert.Equal(t, domain.DefaultUnitDecimals, good.BaseUnit.DecimalsOrDefault())
	d := 2
	assert.Equal(t, 2, domain.UnitFields{Decimals: &d}.DecimalsOrDefault())
	zero := 0
	assert.Equal(t, 0, domain.UnitFields{Decimals: &zero}.DecimalsOrDefault(), "zero decimals is a real choice, not unset")
}

func TestCreateUnitRequest_Validate(t *testing.T) {
	good := domain.CreateUnitRequest{DimensionCode: "LENGTH", UnitFields: domain.UnitFields{UnitCode: "KM", Name: "Kilometre", Symbol: "km"}}
	require.NoError(t, good.Validate())
	bad := good
	bad.DimensionCode = "x"
	assert.Error(t, bad.Validate())
	bad = good
	bad.UnitCode = "K M"
	assert.Error(t, bad.Validate())
}

func TestProposeConversionRequest_Validate(t *testing.T) {
	good := domain.ProposeConversionRequest{Factor: "1000", SourceClass: domain.ConvStandard, SourceRef: "ISO 80000", EvidenceRef: "e"}
	require.NoError(t, good.Validate())
	for name, mut := range map[string]func(*domain.ProposeConversionRequest){
		"zero factor":     func(r *domain.ProposeConversionRequest) { r.Factor = "0" },
		"unknown source":  func(r *domain.ProposeConversionRequest) { r.SourceClass = "GUESS" },
		"no source ref":   func(r *domain.ProposeConversionRequest) { r.SourceRef = " " },
		"no evidence":     func(r *domain.ProposeConversionRequest) { r.EvidenceRef = "" },
		"override why":    func(r *domain.ProposeConversionRequest) { r.SourceClass = domain.ConvOverride },
		"bad valid_from":  func(r *domain.ProposeConversionRequest) { r.ValidFrom = "tomorrow" },
		"long source ref": func(r *domain.ProposeConversionRequest) { r.SourceRef = strings.Repeat("x", 301) },
	} {
		r := good
		mut(&r)
		assert.Error(t, r.Validate(), name)
	}
	over := good
	over.SourceClass, over.Reason = domain.ConvOverride, "supplier packs differ"
	assert.NoError(t, over.Validate())
	assert.False(t, domain.NonStandard(domain.ConvStandard))
	assert.True(t, domain.NonStandard(domain.ConvOrganisation))
	assert.True(t, domain.NonStandard(domain.ConvOverride))
}

func TestUnitCommandRequests_Validate(t *testing.T) {
	assert.Error(t, domain.ApproveConversionRequest{}.Validate())
	assert.NoError(t, domain.ApproveConversionRequest{EvidenceRef: "e"}.Validate())
	assert.Error(t, domain.RetireUnitRequest{EvidenceRef: "e"}.Validate())
	assert.Error(t, domain.RetireUnitRequest{Reason: "r"}.Validate())
	assert.NoError(t, domain.RetireUnitRequest{Reason: "r", EvidenceRef: "e"}.Validate())
	assert.NoError(t, domain.ConvertQuantityQuery{Quantity: "1", From: "KM", To: "M"}.Validate())
	assert.Error(t, domain.ConvertQuantityQuery{Quantity: "x", From: "KM", To: "M"}.Validate())
	assert.Error(t, domain.ConvertQuantityQuery{Quantity: "1", From: "km", To: "M"}.Validate())
	assert.Error(t, domain.ConvertQuantityQuery{Quantity: "1", From: "KM"}.Validate())
}
