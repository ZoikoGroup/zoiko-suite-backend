package domain

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, err := ParseRate(s)
	require.NoError(t, err)
	return r
}

func TestParseRate(t *testing.T) {
	for _, ok := range []string{"1", "0.5", "1.085", "149.253731343283582", "0.000000000000000001", "12345678901234567890.123456789012345678", " 1.1 "} {
		_, err := ParseRate(ok)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"", "0", "0.0", "-1", "1e5", "1,5", "abc", ".5", "1.", "1.0000000000000000001", "123456789012345678901", "01.5", "+1"} {
		_, err := ParseRate(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestFormatRateIsExactAndRoundsHalfUp(t *testing.T) {
	for in, want := range map[string]string{
		"1.085": "1.085", "1.0850": "1.085", "2": "2", "0.5": "0.5", "100.00": "100",
	} {
		assert.Equal(t, want, FormatRate(rat(t, in)), in)
	}
	for _, tc := range []struct {
		n, d int64
		want string
	}{
		{1, 3, "0.333333333333333333"},
		{2, 3, "0.666666666666666667"},
		{1, 8, "0.125"},
		{10, 11, "0.909090909090909091"},
		{25, 22, "1.136363636363636364"},
		{1, 1000000000000000000, "0.000000000000000001"},
		{1, 3000000000000000000, "0"},
	} {
		assert.Equal(t, tc.want, FormatRate(big.NewRat(tc.n, tc.d)), "%d/%d", tc.n, tc.d)
	}
	// Half up at the 18th decimal: 0.0000000000000000005 is below half a unit of the 18th place, 0.5e-18 rounds up.
	assert.Equal(t, "0.000000000000000001", FormatRate(new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Mul(big.NewInt(2), pow10(18)))))
	assert.Equal(t, "0", FormatRate(new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Mul(big.NewInt(3), pow10(18)))))
}

func pow10(n int64) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil) }

func TestCanonicalRateAndFraction(t *testing.T) {
	assert.Equal(t, "1.085", CanonicalRate("1.085000000000000000"))
	assert.Equal(t, "2", CanonicalRate("2.000000"))
	assert.Equal(t, "10", CanonicalRate("10"))
	assert.Equal(t, "0.5", CanonicalRate("0.5000"))
	assert.Equal(t, "22/25", FractionString(new(big.Rat).Mul(rat(t, "1.1"), rat(t, "0.8"))))
	assert.Equal(t, "2", FractionString(big.NewRat(4, 2)))
}

func TestInversionAndCrossArithmeticAreExact(t *testing.T) {
	assert.Equal(t, "10/11", FractionString(InvertRate(rat(t, "1.1"))))
	assert.Equal(t, "5/4", FractionString(InvertRate(rat(t, "0.8"))))
	// There and back again is the identity, with no rounding error: this is why no float is used.
	r := rat(t, "149.253731343283582")
	assert.Equal(t, 0, InvertRate(InvertRate(r)).Cmp(r))
	// A cross rate in floats would be 0.8800000000000001.
	assert.Equal(t, "0.88", FormatRate(new(big.Rat).Mul(rat(t, "1.1"), rat(t, "0.8"))))
}

func TestOutlierExceeds(t *testing.T) {
	prev := rat(t, "1.00")
	assert.False(t, OutlierExceeds(prev, rat(t, "1.10")), "exactly 10% is not past the threshold")
	assert.False(t, OutlierExceeds(prev, rat(t, "0.90")))
	assert.True(t, OutlierExceeds(prev, rat(t, "1.1000001")))
	assert.True(t, OutlierExceeds(prev, rat(t, "0.8999999")), "a fall counts as much as a rise")
	assert.True(t, OutlierExceeds(prev, rat(t, "2")))
	assert.False(t, OutlierExceeds(prev, rat(t, "1")))
}

func validIngest() IngestFXObservationRequest {
	return IngestFXObservationRequest{SourceID: "s", BaseCurrency: "EUR", QuoteCurrency: "USD", RateType: FXSpot, Rate: "1.085",
		MarketDate: "2026-06-12", ObservedAt: time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC), PayloadRef: "s3://x", PayloadHash: strings.Repeat("0", 64)}
}

func TestIngestRequestValidate(t *testing.T) {
	now := time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)
	require.NoError(t, validIngest().Validate(now))
	mut := func(f func(*IngestFXObservationRequest)) IngestFXObservationRequest {
		r := validIngest()
		f(&r)
		return r
	}
	for name, r := range map[string]IngestFXObservationRequest{
		"no source":        mut(func(r *IngestFXObservationRequest) { r.SourceID = "" }),
		"lower case":       mut(func(r *IngestFXObservationRequest) { r.BaseCurrency = "eur" }),
		"four letters":     mut(func(r *IngestFXObservationRequest) { r.QuoteCurrency = "USDX" }),
		"same currency":    mut(func(r *IngestFXObservationRequest) { r.QuoteCurrency = "EUR" }),
		"no rate type":     mut(func(r *IngestFXObservationRequest) { r.RateType = "" }),
		"unknown type":     mut(func(r *IngestFXObservationRequest) { r.RateType = "FORWARD" }),
		"no payload":       mut(func(r *IngestFXObservationRequest) { r.PayloadRef = " " }),
		"bad hash":         mut(func(r *IngestFXObservationRequest) { r.PayloadHash = "XYZ" }),
		"no observed time": mut(func(r *IngestFXObservationRequest) { r.ObservedAt = time.Time{} }),
		"future observed":  mut(func(r *IngestFXObservationRequest) { r.ObservedAt = now.Add(time.Hour) }),
		"zero rate":        mut(func(r *IngestFXObservationRequest) { r.Rate = "0" }),
		"float rate":       mut(func(r *IngestFXObservationRequest) { r.Rate = "1e-3" }),
		"bad date":         mut(func(r *IngestFXObservationRequest) { r.MarketDate = "12/06/2026" }),
		"future date":      mut(func(r *IngestFXObservationRequest) { r.MarketDate = "2026-07-01" }),
	} {
		assert.Error(t, r.Validate(now), name)
	}
	assert.NoError(t, mut(func(r *IngestFXObservationRequest) { r.PayloadHash = "" }).Validate(now), "the hash is optional, the reference is not")

	a, b := validIngest(), validIngest()
	b.Rate, b.CorrelationID = "1.0850", "c"
	assert.Equal(t, a.Fingerprint(), b.Fingerprint(), "1.085 and 1.0850 are one rate")
	b.Rate = "1.086"
	assert.NotEqual(t, a.Fingerprint(), b.Fingerprint())

	sup := SupersedeFXObservationRequest{IngestFXObservationRequest: validIngest()}
	assert.Error(t, sup.Validate(now), "a replacement states why")
	sup.Reason = "re-issued"
	assert.NoError(t, sup.Validate(now))
}

func TestSourceSetAndCloseRequestsValidate(t *testing.T) {
	assert.NoError(t, CreateFXSourceRequest{SourceCode: "ECB", Name: "ECB", SourceClass: FXSourceCentral, Timezone: "Europe/Berlin"}.Validate())
	for name, r := range map[string]CreateFXSourceRequest{
		"no code":   {Name: "n", SourceClass: FXSourceProvider},
		"no name":   {SourceCode: "c", SourceClass: FXSourceProvider},
		"bad class": {SourceCode: "c", Name: "n", SourceClass: "OTHER"},
		"bad zone":  {SourceCode: "c", Name: "n", SourceClass: FXSourceProvider, Timezone: "Mars/Olympus"},
		"long code": {SourceCode: strings.Repeat("x", 41), Name: "n", SourceClass: FXSourceProvider},
	} {
		assert.Error(t, r.Validate(), name)
	}
	assert.Error(t, WithdrawFXSourceRequest{}.Validate())
	assert.NoError(t, WithdrawFXSourceRequest{Reason: "r"}.Validate())

	ok := ApproveFXRateSetRequest{RateType: FXClosing, MarketDate: "2026-06-12", ObservationIDs: []string{"a", "b"}, EvidenceRef: "e"}
	require.NoError(t, ok.Validate())
	mut := func(f func(*ApproveFXRateSetRequest)) ApproveFXRateSetRequest { r := ok; f(&r); return r }
	for name, r := range map[string]ApproveFXRateSetRequest{
		"no type":     mut(func(r *ApproveFXRateSetRequest) { r.RateType = "" }),
		"no evidence": mut(func(r *ApproveFXRateSetRequest) { r.EvidenceRef = "" }),
		"no members":  mut(func(r *ApproveFXRateSetRequest) { r.ObservationIDs = nil }),
		"twice":       mut(func(r *ApproveFXRateSetRequest) { r.ObservationIDs = []string{"a", "a"} }),
		"bad date":    mut(func(r *ApproveFXRateSetRequest) { r.MarketDate = "x" }),
		"long name":   mut(func(r *ApproveFXRateSetRequest) { r.SetName = strings.Repeat("n", 41) }),
	} {
		assert.Error(t, r.Validate(), name)
	}
	assert.Error(t, CloseFXRateSetRequest{}.Validate())
	assert.NoError(t, CloseFXRateSetRequest{EvidenceRef: "e"}.Validate())
	assert.True(t, FXClosing.Valid() && FXTax.Valid() && !FXRateType("").Valid())
	assert.True(t, FXSourceManualExc.Valid() && !FXSourceClass("X").Valid())
}
