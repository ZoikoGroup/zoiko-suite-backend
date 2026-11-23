package domain

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func dayp(s string) *time.Time { t := day(s); return &t }

const vatType = `{"family":"REGISTRATION_TYPE","registration":{"kind":"VAT","authority":"Example Revenue","identifier":{"pattern":"^GB[0-9]{9}$",
 "normalize":"UPPER_STRIP_SEPARATORS","examples_valid":["GB123456789","gb 123-456-789"],"examples_invalid":["GB12345","XX123456789"]}}}`

func TestRegistrationFamilies_ParseAndValidate(t *testing.T) {
	good := map[string]string{
		"registration": vatType,
		"licence":      `{"family":"LICENCE_TYPE","licence":{"authority":"A","term":{"years":1},"renewal_notice_days":60,"grace_days":30}}`,
		"licence-open": `{"family":"LICENCE_TYPE","licence":{"authority":"A","renewal_notice_days":0,"grace_days":0}}`,
		"scheme":       `{"family":"SCHEME","scheme":{"authority":"A","eligibility":{"metric":"ANNUAL_TURNOVER","operator":"LTE","threshold":"150000"},"lock_in":{"years":1},"cancellation_notice_days":30}}`,
		"rolling":      `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"90000","breach":{"effect":"REGISTRATION_REQUIRED","effective":{"kind":"END_OF_MONTH_FOLLOWING"}}}}`,
		"static":       `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":{"kind":"STATIC","start_month":4,"start_day":1},"operator":"GTE","threshold":"100000","breach":{"effect":"REVIEW_REQUIRED","effective":{"kind":"NEXT_PERIOD_START"}}}}`,
	}
	for name, raw := range good {
		_, err := ParseRuleParameters([]byte(raw))
		assert.NoError(t, err, name)
	}

	bad := []struct{ name, raw, want string }{
		{"unknown field", `{"family":"SCHEME","scheme":{"authority":"A","eligibility":{"metric":"METRIC","operator":"LT","threshold":"1"},"cancellation_notice_days":0},"extra":1}`, "unknown field"},
		{"threshold as a JSON number", `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":90000,"breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE"}}}}`, "cannot unmarshal"},
		{"threshold zero", `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"0","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE"}}}}`, "positive"},
		{"monitor operator LT", `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"ROLLING","months":12},"operator":"LT","threshold":"5","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE"}}}}`, "GT or GTE"},
		{"rolling needs no start", `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"ROLLING","months":12,"start_month":4,"start_day":1},"operator":"GT","threshold":"5","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE"}}}}`, "ROLLING"},
		{"29 Feb start", `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"STATIC","start_month":2,"start_day":29},"operator":"GT","threshold":"5","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE"}}}}`, "29 February"},
		{"next period start on rolling", `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"5","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"NEXT_PERIOD_START"}}}}`, "STATIC period"},
		{"days on wrong kind", `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"5","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE","days":3}}}}`, "DAYS_AFTER"},
		{"unanchored pattern", strings.Replace(vatType, `"^GB[0-9]{9}$"`, `"GB[0-9]{9}"`, 1), "anchored"},
		{"pattern disagrees with a valid example", strings.Replace(vatType, `"GB123456789","gb 123-456-789"`, `"GB1234"`, 1), "declared valid"},
		{"pattern accepts an invalid example", strings.Replace(vatType, `"GB12345","XX123456789"`, `"GB123456789"`, 1), "declared invalid"},
		{"pattern does not compile", strings.Replace(vatType, `"^GB[0-9]{9}$"`, `"^GB[0-9{9}$"`, 1), "compile"},
		{"no examples", strings.Replace(vatType, `"examples_invalid":["GB12345","XX123456789"]`, `"examples_invalid":[]`, 1), "proved by its own examples"},
		{"licence term zero", `{"family":"LICENCE_TYPE","licence":{"authority":"A","term":{"years":0},"renewal_notice_days":0,"grace_days":0}}`, "term"},
		{"licence notice too long", `{"family":"LICENCE_TYPE","licence":{"authority":"A","renewal_notice_days":400,"grace_days":0}}`, "0 to 365"},
		{"scheme bad operator", `{"family":"SCHEME","scheme":{"authority":"A","eligibility":{"metric":"METRIC","operator":"EQ","threshold":"1"},"cancellation_notice_days":0}}`, "operator"},
		{"wrong payload for family", `{"family":"SCHEME","licence":{"authority":"A","renewal_notice_days":0,"grace_days":0}}`, "SCHEME needs scheme"},
		{"two payloads", `{"family":"SCHEME","scheme":{"authority":"A","eligibility":{"metric":"METRIC","operator":"LT","threshold":"1"},"cancellation_notice_days":0},"licence":{"authority":"A","renewal_notice_days":0,"grace_days":0}}`, "exactly"},
		{"another family's fields", `{"family":"LICENCE_TYPE","rate":"0.2","licence":{"authority":"A","renewal_notice_days":0,"grace_days":0}}`, "own payload"},
		{"registration fields on a tax rate", `{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2},"licence":{"authority":"A","renewal_notice_days":0,"grace_days":0}}`, "belong to"},
		{"registration fields on retention", `{"family":"RETENTION","record_class":"X1","trigger":"CREATION","minimum":{"years":1},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY","monitor":{"metric":"METRIC"}}`, "belong to"},
	}
	for _, c := range bad {
		_, err := ParseRuleParameters([]byte(c.raw))
		if assert.Error(t, err, c.name) {
			assert.Contains(t, err.Error(), c.want, c.name)
		}
	}
}

func TestRegistrationFamilies_AreTiedToTheirOwnDomainsAndPinnedForCertification(t *testing.T) {
	assert.Equal(t, RegistrationTypeDomain, FamilyDomain(FamilyRegistrationType))
	assert.Equal(t, LicenceTypeDomain, FamilyDomain(FamilyLicenceType))
	assert.Equal(t, SchemeDomain, FamilyDomain(FamilyScheme))
	assert.Equal(t, ThresholdDomain, FamilyDomain(FamilyThresholdMonitor))
	for _, d := range []string{RegistrationTypeDomain, LicenceTypeDomain, SchemeDomain, ThresholdDomain} {
		assert.True(t, ReservedDomain(d), d)
	}
	for _, f := range []string{FamilyRegistrationType, FamilyLicenceType, FamilyScheme, FamilyThresholdMonitor} {
		assert.True(t, ParameterOnlyFamily(f), "%s is never calculated with", f)
	}
	pin := func(raw string) string {
		p, err := ParseRuleParameters([]byte(raw))
		require.NoError(t, err)
		return PinnedParameterValue(p)
	}
	assert.Equal(t, "P1Y", pin(`{"family":"LICENCE_TYPE","licence":{"authority":"A","term":{"years":1},"renewal_notice_days":60,"grace_days":0}}`))
	assert.Equal(t, "60", pin(`{"family":"LICENCE_TYPE","licence":{"authority":"A","renewal_notice_days":60,"grace_days":0}}`))
	assert.Equal(t, "150000", pin(`{"family":"SCHEME","scheme":{"authority":"A","eligibility":{"metric":"METRIC","operator":"LTE","threshold":"150000"},"cancellation_notice_days":0}}`))
	assert.Equal(t, "90000", pin(`{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"90000","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE"}}}}`))
	assert.Equal(t, "", pin(vatType), "a registration type is pinned by its own examples, checked at validation")
}

func mustParams(t *testing.T, raw string) RuleParameters {
	t.Helper()
	p, err := ParseRuleParameters([]byte(raw))
	require.NoError(t, err)
	return p
}

func TestCheckIdentifier(t *testing.T) {
	p := mustParams(t, vatType)
	for _, id := range []string{"GB123456789", "gb 123-456-789", "  GB123.456.789 "} {
		r := CheckIdentifier(*p.Registration, id)
		assert.Equal(t, OutcomeIdentifierValid, r.Outcome, id)
		assert.Equal(t, IdentifierDigest("GB123456789"), r.NormalizedDigest, "the digest is of the normalised form")
	}
	for _, id := range []string{"GB12345", "XX123456789", "GB1234567890", "", strings.Repeat("GB123456789", 10)} {
		assert.Equal(t, OutcomeIdentifierInvalid, CheckIdentifier(*p.Registration, id).Outcome, id)
	}
	r := CheckIdentifier(*p.Registration, "GB123456789")
	assert.NotContains(t, fmt.Sprintf("%+v", r), "GB123456789", "the identifier itself is never echoed")
	assert.Contains(t, strings.Join(r.Explanation, " "), "format check only")
}

const licType = `{"family":"LICENCE_TYPE","licence":{"authority":"A","term":{"years":1},"renewal_notice_days":60,"grace_days":30}}`

func TestEvaluateLicence_StatusBoundaries(t *testing.T) {
	l := *mustParams(t, licType).Licence
	// issued 2026-01-15, one year: last valid day 2027-01-14; renewal opens 60 days earlier; grace runs 30 days after.
	cases := []struct{ asOf, status string }{
		{"2026-01-14", LicenceNotYetValid},
		{"2026-01-15", LicenceActive},
		{"2026-11-14", LicenceActive},
		{"2026-11-15", LicenceRenewalWindow},
		{"2027-01-14", LicenceRenewalWindow},
		{"2027-01-15", LicenceInGrace},
		{"2027-02-13", LicenceInGrace},
		{"2027-02-14", LicenceExpired},
	}
	for _, c := range cases {
		r := EvaluateLicence(l, day("2026-01-15"), nil, day(c.asOf))
		assert.Equal(t, OutcomeLicenceEvaluated, r.Outcome, c.asOf)
		assert.Equal(t, c.status, r.Status, c.asOf)
		assert.Equal(t, "2027-01-14", r.ValidUntil)
	}
	r := EvaluateLicence(l, day("2026-01-15"), nil, day("2027-01-14"))
	assert.Equal(t, "2026-11-15", r.RenewalWindowOpens)
	assert.Equal(t, "2027-02-13", r.GraceEndsOn)
	require.NotNil(t, r.DaysToExpiry)
	assert.Equal(t, 0, *r.DaysToExpiry)
}

func TestEvaluateLicence_StatedExpiryTermAndFacts(t *testing.T) {
	l := *mustParams(t, licType).Licence
	assert.Equal(t, "2027-01-14", EvaluateLicence(l, day("2026-01-15"), dayp("2027-01-14"), day("2026-06-01")).ValidUntil)
	assert.Equal(t, "2026-09-30", EvaluateLicence(l, day("2026-01-15"), dayp("2026-09-30"), day("2026-06-01")).ValidUntil, "a shorter licence is fine")
	assert.Equal(t, LicenceTermExceeded, EvaluateLicence(l, day("2026-01-15"), dayp("2027-01-15"), day("2026-06-01")).Outcome, "beyond the maximum term")
	assert.Equal(t, CalcInvalidFacts, EvaluateLicence(l, day("2026-01-15"), dayp("2026-01-15"), day("2026-06-01")).Outcome, "expiry must be after issue")

	open := *mustParams(t, `{"family":"LICENCE_TYPE","licence":{"authority":"A","renewal_notice_days":0,"grace_days":0}}`).Licence
	assert.Equal(t, CalcInvalidFacts, EvaluateLicence(open, day("2026-01-15"), nil, day("2026-06-01")).Outcome, "no fixed term and no stated expiry")
	r := EvaluateLicence(open, day("2026-01-15"), dayp("2030-01-01"), day("2029-12-31"))
	assert.Equal(t, LicenceActive, r.Status, "without a renewal notice there is no renewal window")
	assert.Empty(t, r.RenewalWindowOpens)

	// A leap-day issue clamps like the retention maths: 29 Feb 2028 + 1 year = 28 Feb 2029, last valid day 27 Feb 2029.
	assert.Equal(t, "2029-02-27", EvaluateLicence(l, day("2028-02-29"), nil, day("2028-03-01")).ValidUntil)
}

const schemeJSON = `{"family":"SCHEME","scheme":{"authority":"A","eligibility":{"metric":"ANNUAL_TURNOVER","operator":"LTE","threshold":"150000"},"lock_in":{"years":1},"cancellation_notice_days":30}}`

func TestEvaluateScheme(t *testing.T) {
	s := *mustParams(t, schemeJSON).Scheme
	assert.Equal(t, OutcomeSchemeEligible, EvaluateScheme(s, "150000", day("2026-04-01"), nil).Outcome, "LTE includes the threshold itself")
	assert.Equal(t, OutcomeSchemeEligible, EvaluateScheme(s, "149999.99", day("2026-04-01"), nil).Outcome)
	ne := EvaluateScheme(s, "150000.01", day("2026-04-01"), nil)
	assert.Equal(t, OutcomeSchemeNotEligible, ne.Outcome)
	assert.Contains(t, strings.Join(ne.Explanation, " "), "not eligible")
	for _, bad := range []string{"-1", "1e5", "abc", "", "1."} {
		assert.Equal(t, CalcInvalidFacts, EvaluateScheme(s, bad, day("2026-04-01"), nil).Outcome, bad)
	}

	// Lock-in: an election from 2026-04-01 is locked in until 2027-04-01.
	r := EvaluateScheme(s, "100000", day("2026-04-01"), nil)
	assert.Equal(t, "2027-04-01", r.LockInEnds)
	assert.Empty(t, r.EarliestCancellation)
	early := EvaluateScheme(s, "100000", day("2026-04-01"), dayp("2026-06-01"))
	assert.True(t, early.DeferredByLockIn)
	assert.Equal(t, "2027-04-01", early.EarliestCancellation, "notice would end in July 2026, inside the lock-in: deferred to its end")
	late := EvaluateScheme(s, "100000", day("2026-04-01"), dayp("2027-05-01"))
	assert.False(t, late.DeferredByLockIn)
	assert.Equal(t, "2027-05-31", late.EarliestCancellation, "30 days' notice")
	edge := EvaluateScheme(s, "100000", day("2026-04-01"), dayp("2027-03-02"))
	assert.False(t, edge.DeferredByLockIn, "2027-03-02 + 30 days is exactly 2027-04-01: not before the lock-in ends")
	assert.Equal(t, "2027-04-01", edge.EarliestCancellation)
}

func monitor(t *testing.T, period, op, thr, effective string) ThresholdMonitor {
	t.Helper()
	raw := fmt.Sprintf(`{"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":%s,"operator":"%s","threshold":"%s","breach":{"effect":"REGISTRATION_REQUIRED","effective":%s}}}`,
		period, op, thr, effective)
	return *mustParams(t, raw).Monitor
}

const rolling12 = `{"kind":"ROLLING","months":12}`

func obs(pairs ...string) []Observation {
	var o []Observation
	for i := 0; i < len(pairs); i += 2 {
		o = append(o, Observation{Date: pairs[i], Amount: pairs[i+1]})
	}
	return o
}

func TestEvaluateThreshold_RollingWindowAndEffectiveDates(t *testing.T) {
	data := obs("2026-01-31", "30000", "2026-02-28", "30000", "2026-03-31", "30000", "2026-04-30", "1")
	asOf := day("2026-06-15")

	m := monitor(t, rolling12, "GT", "90000", `{"kind":"END_OF_MONTH_FOLLOWING"}`)
	r := EvaluateThreshold(m, data, asOf)
	assert.Equal(t, OutcomeThresholdEvaluated, r.Outcome)
	assert.True(t, r.Breached)
	assert.Equal(t, "2026-04-30", r.BreachDate, "90000 is not GREATER than 90000; the next pound is")
	assert.Equal(t, "90001", r.TotalAtBreach)
	assert.Equal(t, "2026-05-31", r.EffectiveDate)
	assert.Equal(t, "2026-05-31", r.RegistrationEffectiveDate)
	assert.Equal(t, "2025-06-16", r.WindowFrom, "the rolling window is (as_of minus 12 months, as_of]")
	assert.Equal(t, "90001", r.Total)

	assert.Equal(t, "2026-03-31", EvaluateThreshold(monitor(t, rolling12, "GTE", "90000", `{"kind":"IMMEDIATE"}`), data, asOf).BreachDate, "GTE breaches at exactly the threshold")
	assert.Equal(t, "2026-04-30", EvaluateThreshold(monitor(t, rolling12, "GT", "90000", `{"kind":"IMMEDIATE"}`), data, asOf).EffectiveDate)
	assert.Equal(t, "2026-05-30", EvaluateThreshold(monitor(t, rolling12, "GT", "90000", `{"kind":"DAYS_AFTER","days":30}`), data, asOf).EffectiveDate)
	// End of month following a December breach crosses the year.
	dec := EvaluateThreshold(monitor(t, rolling12, "GT", "10", `{"kind":"END_OF_MONTH_FOLLOWING"}`), obs("2026-12-20", "11"), day("2026-12-31"))
	assert.Equal(t, "2027-01-31", dec.EffectiveDate)

	// Not breached: headroom is reported and there is no effective date.
	under := EvaluateThreshold(m, obs("2026-01-31", "30000"), asOf)
	assert.False(t, under.Breached)
	assert.Equal(t, "60000", under.Headroom)
	assert.Empty(t, under.EffectiveDate)
	assert.Empty(t, under.RegistrationEffectiveDate)
}

func TestEvaluateThreshold_WindowEdgesAndReproducibility(t *testing.T) {
	m := monitor(t, rolling12, "GT", "100", `{"kind":"IMMEDIATE"}`)
	// as_of 2026-06-15: an observation on 2025-06-15 is outside (the window starts 2025-06-16), one on 2025-06-16 is inside.
	out := EvaluateThreshold(m, obs("2025-06-15", "200"), day("2026-06-15"))
	assert.False(t, out.Breached)
	assert.Equal(t, "0", out.Total)
	in := EvaluateThreshold(m, obs("2025-06-16", "200"), day("2026-06-15"))
	assert.True(t, in.Breached)

	// Month-end clamp: 31 March minus 12 months is 31 March; with a 1-month window, 31 March minus 1 month is 28 Feb, never 3 March.
	one := monitor(t, `{"kind":"ROLLING","months":1}`, "GT", "10", `{"kind":"IMMEDIATE"}`)
	assert.Equal(t, "2027-03-01", EvaluateThreshold(one, nil, day("2027-03-31")).WindowFrom, "(28 Feb, 31 Mar] starts on 1 Mar")

	// Later observations are ignored, so a re-run for an earlier date reproduces the earlier answer.
	data := obs("2026-01-10", "60", "2026-02-10", "60")
	early := EvaluateThreshold(m, data, day("2026-01-31"))
	assert.False(t, early.Breached)
	assert.Equal(t, 1, early.IgnoredAfterAsOf)
	assert.True(t, EvaluateThreshold(m, data, day("2026-02-28")).Breached)
}

func TestEvaluateThreshold_StaticPeriod(t *testing.T) {
	static := `{"kind":"STATIC","start_month":4,"start_day":1}`
	m := monitor(t, static, "GT", "100000", `{"kind":"NEXT_PERIOD_START"}`)
	data := obs("2026-03-31", "60000", "2026-04-01", "60000")
	// The period containing 31 March 2026 started on 1 April 2025; 1 April 2026 starts the next one.
	before := EvaluateThreshold(m, data, day("2026-03-31"))
	assert.Equal(t, "2025-04-01", before.WindowFrom)
	assert.Equal(t, "60000", before.Total)
	after := EvaluateThreshold(m, data, day("2026-04-01"))
	assert.Equal(t, "2026-04-01", after.WindowFrom)
	assert.Equal(t, "60000", after.Total, "the new period starts fresh")
	assert.False(t, after.Breached)

	br := EvaluateThreshold(m, obs("2025-06-01", "70000", "2025-10-10", "40000"), day("2026-02-01"))
	assert.True(t, br.Breached)
	assert.Equal(t, "2025-10-10", br.BreachDate)
	assert.Equal(t, "2026-04-01", br.EffectiveDate, "NEXT_PERIOD_START is the first period start after the breach")
}

func TestEvaluateThreshold_ExactDecimalsAndInvalidFacts(t *testing.T) {
	// 0.1 + 0.2 is exactly 0.3 here: not GREATER than 0.3, but it does MEET it. Binary floating point would get this wrong.
	data := obs("2026-01-01", "0.1", "2026-01-02", "0.2")
	assert.False(t, EvaluateThreshold(monitor(t, rolling12, "GT", "0.3", `{"kind":"IMMEDIATE"}`), data, day("2026-01-31")).Breached)
	ge := EvaluateThreshold(monitor(t, rolling12, "GTE", "0.3", `{"kind":"IMMEDIATE"}`), data, day("2026-01-31"))
	assert.True(t, ge.Breached)
	assert.Equal(t, "0.3", ge.Total)

	m := monitor(t, rolling12, "GT", "10", `{"kind":"IMMEDIATE"}`)
	for name, o := range map[string][]Observation{
		"negative amount": obs("2026-01-01", "-5"),
		"exponent":        obs("2026-01-01", "1e3"),
		"bad date":        obs("2026/01/01", "5"),
		"empty amount":    obs("2026-01-01", ""),
	} {
		r := EvaluateThreshold(m, o, day("2026-01-31"))
		assert.Equal(t, CalcInvalidFacts, r.Outcome, name)
		assert.False(t, r.Breached, name)
	}
	many := make([]Observation, maxObservations+1)
	for i := range many {
		many[i] = Observation{Date: "2026-01-01", Amount: "1"}
	}
	assert.Equal(t, CalcInvalidFacts, EvaluateThreshold(m, many, day("2026-01-31")).Outcome)

	// An order-independent answer: the same observations in any order give the same breach date.
	a := EvaluateThreshold(m, obs("2026-01-05", "6", "2026-01-20", "6"), day("2026-01-31"))
	b := EvaluateThreshold(m, obs("2026-01-20", "6", "2026-01-05", "6"), day("2026-01-31"))
	assert.Equal(t, a.BreachDate, b.BreachDate)
	assert.Equal(t, "2026-01-20", a.BreachDate)
}

func TestMonitorNeverPromisesWhatItCannotKnow(t *testing.T) {
	// A NOTIFY_ONLY or REVIEW_REQUIRED breach is not a registration: no registration date is offered to the obligation engine.
	raw := `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"METRIC","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"5","breach":{"effect":"REVIEW_REQUIRED","effective":{"kind":"IMMEDIATE"}}}}`
	r := EvaluateThreshold(*mustParams(t, raw).Monitor, obs("2026-01-01", "6"), day("2026-01-31"))
	assert.True(t, r.Breached)
	assert.Equal(t, "REVIEW_REQUIRED", r.Effect)
	assert.Empty(t, r.RegistrationEffectiveDate)
}
