package registry

// The local, deterministic LEI verifier (ORG-08, plan ORG-6). It checks what can be checked from the LEI
// alone: the 20-character alphanumeric format, the checksum (ISO 17442), and that the issuer is a valid LOU.
// It does not confirm the entity exists in the GLEIF database; an external GLEIF feed is a different verifier
// behind the same interface. It is BLOCKED until the external data feed service exists.

import (
	"regexp"
	"strings"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// LEIVerifier observes one LEI value. It must be deterministic and must not change the LEI.
type LEIVerifier interface {
	Name() string
	Verify(lei string) domain.LEIObservation
}

// Confidence a local rule check can reach.
const (
	leiVerifiedThreshold = 0.80
	leiRuleConfidence    = 0.90
	leiNoRuleScore       = 0.50
)

// leiRegex matches a 20-character alphanumeric string (A-Z, 0-9).
var leiRegex = regexp.MustCompile(`^[A-Z0-9]{20}$`)

// louPrefixes are valid Local Operating Unit (LOU) prefixes from GLEIF.
// These are the first 4 characters of the LEI.
var louPrefixes = map[string]bool{
	"3912": true, // Bloomberg
	"5493": true, // DTCC
	"5299": true, // WM Datenservice
	"2138": true, // London Stock Exchange
	"5586": true, // CMA
	"2221": true, // KDPW
	"8512": true, // CDCC
	"6354": true, // ASX
	"3358": true, // BNB
	"9845": true, // EBRD
	// ... more LOU prefixes exist; this is a subset for validation
}

// LocalLEIVerifier is the deterministic rule verifier for LEI.
type LocalLEIVerifier struct{}

// Name implements LEIVerifier.
func (LocalLEIVerifier) Name() string { return domain.VerifierLocalRules }

// Verify implements LEIVerifier.
// Performs: format check (20 alphanumeric), checksum (ISO 17442 Mod 97,10), LOU prefix validation.
func (LocalLEIVerifier) Verify(lei string) domain.LEIObservation {
	obs := domain.LEIObservation{LEI: lei, Verifier: domain.VerifierLocalRules, Findings: []string{}}

	fail := func(f string) {
		obs.Findings = append(obs.Findings, f)
	}

	// 1. Format check: exactly 20 alphanumeric characters
	if !leiRegex.MatchString(lei) {
		fail("LEI_FORMAT")
		obs.Result, obs.Confidence = domain.VerifyFailed, 0
		return obs
	}

	// 2. LOU prefix check: first 4 chars must be a known LOU
	lou := lei[:4]
	if !louPrefixes[lou] {
		fail("LOU_UNKNOWN")
		// Not a hard failure - some LOUs may be new
		obs.Result, obs.Confidence = domain.VerifyLowConfidence, leiNoRuleScore
		obs.Findings = append(obs.Findings, "LOU_PREFIX_NOT_IN_LOCAL_RULES")
		return obs
	}

	// 3. Checksum validation: ISO 17442 Mod 97,10
	if !validateLEIChecksum(lei) {
		fail("LEI_CHECKSUM")
		obs.Result, obs.Confidence = domain.VerifyFailed, 0
		return obs
	}

	obs.Result, obs.Confidence = domain.VerifyPassed, leiRuleConfidence
	return obs
}

// validateLEIChecksum implements the ISO 17442 Mod 97,10 checksum algorithm.
// LEI is 20 chars: 18 alphanumeric + 2 check digits.
// Algorithm: Convert letters to numbers (A=10, B=11, ..., Z=35), then mod 97.
func validateLEIChecksum(lei string) bool {
	var numeric strings.Builder
	for _, r := range lei {
		if r >= '0' && r <= '9' {
			numeric.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			// A=10, B=11, ..., Z=35
			val := int(r - 'A' + 10)
			numeric.WriteString(string(rune('0' + val/10)))
			numeric.WriteString(string(rune('0' + val%10)))
		} else {
			return false
		}
	}

	// Compute mod 97
	numStr := numeric.String()
	remainder := 0
	for _, r := range numStr {
		remainder = (remainder*10 + int(r-'0')) % 97
	}
	// Valid LEI has remainder 1
	return remainder == 1
}