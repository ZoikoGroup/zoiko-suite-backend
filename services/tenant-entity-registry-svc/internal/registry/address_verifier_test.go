package registry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func ver(country, postal string) domain.AddressVersion {
	return domain.AddressVersion{VersionNo: 1, Lines: []string{"1 Main St"}, Locality: "X", PostalCode: postal, Country: country, Normalized: "n"}
}

func TestLocalVerifier(t *testing.T) {
	v := LocalAddressVerifier{}
	cases := []struct {
		name, country, postal string
		result                domain.VerificationResult
		finding               string
	}{
		{"UK postcode with spaces", "GB", "SW1A 1AA", domain.VerifyPassed, ""},
		{"UK postcode lower case", "GB", "ls1 1aa", domain.VerifyPassed, ""},
		{"US zip+4", "US", "12345-6789", domain.VerifyPassed, ""},
		{"US wrong length", "US", "1234", domain.VerifyFailed, "POSTAL_CODE_FORMAT"},
		{"missing postal where the country has a rule", "DE", "", domain.VerifyFailed, "POSTAL_CODE_MISSING"},
		{"Canada", "CA", "K1A 0B1", domain.VerifyPassed, ""},
		{"unknown country", "ZZ", "12345", domain.VerifyFailed, "COUNTRY_UNKNOWN"},
		{"valid country with no rule is surfaced, not passed", "KE", "00100", domain.VerifyLowConfidence, "NO_POSTAL_RULE_FOR_COUNTRY"},
		{"country without postal codes", "AE", "", domain.VerifyPassed, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := v.Verify(ver(c.country, c.postal))
			assert.Equal(t, c.result, o.Result)
			if c.finding != "" {
				assert.Contains(t, o.Findings, c.finding)
			}
			assert.Equal(t, domain.VerifierLocalRules, o.Verifier)
			assert.NotNil(t, o.Findings)
			assert.Less(t, o.Confidence, 1.0, "a rule check never claims delivery validation")
			assert.Equal(t, o.Result == domain.VerifyPassed, o.Confidence >= VerifiedThreshold,
				"only a pass reaches the confidence that verifies")
		})
	}
}

func TestLocalVerifierIsDeterministicAndLeavesTheVersionAlone(t *testing.T) {
	in := ver("GB", "SW1A 1AA")
	a, b := LocalAddressVerifier{}.Verify(in), LocalAddressVerifier{}.Verify(in)
	assert.Equal(t, a, b)
	assert.Equal(t, "SW1A 1AA", in.PostalCode)
}

func TestISOCountryList(t *testing.T) {
	for _, c := range []string{"GB", "US", "IN", "DE", "AU", "SG", "AE", "KE"} {
		assert.True(t, ValidCountryCode(c), c)
	}
	for _, c := range []string{"ZZ", "XX", "UK", "", "gb"} {
		assert.False(t, ValidCountryCode(c), c)
	}
}

func TestExternalVerifierIsBlockedNotSubstituted(t *testing.T) {
	s := &Service{}
	_, err := s.addressVerifier(domain.VerifierExternal)
	require.ErrorIs(t, err, ErrServiceUnavailable)
	_, err = s.addressVerifier("MAGIC")
	require.ErrorIs(t, err, ErrInvalidInput)
	v, err := s.addressVerifier("")
	require.NoError(t, err)
	assert.Equal(t, domain.VerifierLocalRules, v.Name())

	// The service refuses before touching a store: a blocked provider must not fall back to the local rules.
	_, err = s.VerifyAddress(context.Background(), "x", domain.VerifyAddressRequest{Verifier: domain.VerifierExternal})
	require.ErrorIs(t, err, ErrServiceUnavailable)
}
