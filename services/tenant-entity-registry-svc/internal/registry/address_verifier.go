package registry

// The local, deterministic address verifier (ORG-08, plan ORG-6). It checks what can be checked from the fields
// alone: the country is a real ISO 3166-1 alpha-2 code, the required fields are present, and the postal code fits
// the country's published format where one is known. It does not geocode and does not confirm delivery, so it never
// reports full confidence, and it proves nothing about registered-office status.
//
// An external provider (postal validation, geocoding) is a different verifier behind the same interface. It is
// BLOCKED until the external data feed service exists, and the service says so rather than pretending.

import (
	"regexp"
	"strings"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// AddressVerifier observes one address version. It must be deterministic and must not change the address.
type AddressVerifier interface {
	Name() string
	Verify(v domain.AddressVersion) domain.AddressObservation
}

// Confidence a local rule check can reach. Below VerifiedThreshold a result is surfaced as LOW_CONFIDENCE and the
// version stays UNVERIFIED.
const (
	VerifiedThreshold   = 0.80
	localRuleConfidence = 0.90
	localNoRuleScore    = 0.50
)

// postalRules are the published postal code formats of countries with a fixed one, applied to the comparison form
// (upper case, no spaces or hyphens).
var postalRules = map[string]*regexp.Regexp{
	"US": regexp.MustCompile(`^\d{5}(\d{4})?$`),
	"CA": regexp.MustCompile(`^[ABCEGHJKLMNPRSTVXY]\d[ABCEGHJ-NPRSTV-Z]\d[ABCEGHJ-NPRSTV-Z]\d$`),
	"GB": regexp.MustCompile(`^[A-Z]{1,2}\d[A-Z\d]?\d[A-Z]{2}$`),
	"DE": regexp.MustCompile(`^\d{5}$`),
	"FR": regexp.MustCompile(`^\d{5}$`),
	"ES": regexp.MustCompile(`^\d{5}$`),
	"IT": regexp.MustCompile(`^\d{5}$`),
	"NL": regexp.MustCompile(`^\d{4}[A-Z]{2}$`),
	"BE": regexp.MustCompile(`^\d{4}$`),
	"CH": regexp.MustCompile(`^\d{4}$`),
	"AT": regexp.MustCompile(`^\d{4}$`),
	"AU": regexp.MustCompile(`^\d{4}$`),
	"IN": regexp.MustCompile(`^\d{6}$`),
	"JP": regexp.MustCompile(`^\d{7}$`),
	"SG": regexp.MustCompile(`^\d{6}$`),
	"BR": regexp.MustCompile(`^\d{8}$`),
	"SE": regexp.MustCompile(`^\d{5}$`),
	"PL": regexp.MustCompile(`^\d{5}$`),
}

// Countries with no postal code system, where a missing code is not a finding.
var noPostal = map[string]bool{"AE": true, "HK": true, "QA": true, "ZW": true, "AO": true, "BS": true}

// iso3166 is every officially assigned ISO 3166-1 alpha-2 code.
var iso3166 = func() map[string]bool {
	const codes = "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ " +
		"CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR " +
		"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP " +
		"KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT " +
		"MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW " +
		"SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ " +
		"UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW"
	m := map[string]bool{}
	for _, c := range strings.Fields(codes) {
		m[c] = true
	}
	return m
}()

// ValidCountryCode reports whether c is an assigned ISO 3166-1 alpha-2 code.
func ValidCountryCode(c string) bool { return iso3166[c] }

// LocalAddressVerifier is the deterministic rule verifier.
type LocalAddressVerifier struct{}

// Name implements AddressVerifier.
func (LocalAddressVerifier) Name() string { return domain.VerifierLocalRules }

// Verify implements AddressVerifier.
func (LocalAddressVerifier) Verify(v domain.AddressVersion) domain.AddressObservation {
	obs := domain.AddressObservation{VersionNo: v.VersionNo, Verifier: domain.VerifierLocalRules, Findings: []string{},
		NormalizedSeen: v.Normalized}
	fail := func(f string) { obs.Findings = append(obs.Findings, f) }

	if !ValidCountryCode(v.Country) {
		fail("COUNTRY_UNKNOWN")
	}
	postal := normalizedPostal(v.PostalCode)
	rule, hasRule := postalRules[v.Country]
	switch {
	case hasRule && postal == "":
		fail("POSTAL_CODE_MISSING")
	case hasRule && !rule.MatchString(postal):
		fail("POSTAL_CODE_FORMAT")
	}
	if len(obs.Findings) > 0 {
		obs.Result, obs.Confidence = domain.VerifyFailed, 0
		return obs
	}
	if !hasRule && !noPostal[v.Country] {
		// A valid country with no rule to check against: nothing is wrong, and nothing is proven.
		obs.Result, obs.Confidence = domain.VerifyLowConfidence, localNoRuleScore
		obs.Findings = append(obs.Findings, "NO_POSTAL_RULE_FOR_COUNTRY")
		return obs
	}
	obs.Result, obs.Confidence = domain.VerifyPassed, localRuleConfidence
	return obs
}

func normalizedPostal(p string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(p) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
