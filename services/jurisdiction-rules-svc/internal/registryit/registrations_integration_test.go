//go:build integration

// ZS-JUR-001 s16 end to end: registration identifier formats, licences, schemes and
// threshold monitors are authored as typed-parameter rules, packaged, certified,
// released and answered from the verified artifact. Nothing about an entity's actual
// registrations is stored, and an identifier never reaches the evidence ledger.
package registryit_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	regVAT = `{"family":"REGISTRATION_TYPE","registration":{"kind":"VAT","authority":"Example Revenue","evidence_required":["AUTHORITY_CERTIFICATE"],"threshold_monitor":"VAT_REG_THRESHOLD",
		"identifier":{"pattern":"^GB[0-9]{9}$","normalize":"UPPER_STRIP_SEPARATORS","examples_valid":["GB123456789","gb 123-456-789"],"examples_invalid":["GB12345","XX123456789"]}}}`
	licTrading = `{"family":"LICENCE_TYPE","licence":{"authority":"Example Council","term":{"years":1},"renewal_notice_days":60,"grace_days":30}}`
	schemeFlat = `{"family":"SCHEME","scheme":{"authority":"Example Revenue","eligibility":{"metric":"ANNUAL_TURNOVER","operator":"LTE","threshold":"150000","currency":"GBP"},"lock_in":{"years":1},"cancellation_notice_days":30}}`
	monV1      = `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"90000","currency":"GBP","breach":{"effect":"REGISTRATION_REQUIRED","effective":{"kind":"END_OF_MONTH_FOLLOWING"}}}}`
	monV2      = `{"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"85000","currency":"GBP","breach":{"effect":"REGISTRATION_REQUIRED","effective":{"kind":"END_OF_MONTH_FOLLOWING"}}}}`
)

func newRegistrationScenario(t *testing.T) *payrollScenario {
	t.Helper()
	return newRulesScenario(t, []payrollSpec{
		{"VAT_NUMBER", pyStart2025, nil, regVAT, "REGISTRATION_TYPE"},
		{"TRADING_LICENCE", pyStart2025, nil, licTrading, "LICENCE_TYPE"},
		{"SMALL_BUSINESS_SCHEME", pyStart2025, nil, schemeFlat, "SCHEME_ELECTION"},
		{"VAT_REG_THRESHOLD", pyStart2025, &py2026Jan, monV1, "THRESHOLD_MONITOR"},
		{"VAT_REG_THRESHOLD", py2026Jan, nil, monV2, "THRESHOLD_MONITOR"},
	})
}

// regCall posts to an s16 endpoint with the scenario's jurisdiction filled in.
func (s *payrollScenario) regCall(t *testing.T, path string, body map[string]any, want int) map[string]any {
	t.Helper()
	base := map[string]any{"jurisdiction": s.jur.JurisdictionCode}
	for k, v := range body {
		base[k] = v
	}
	code, r := s.e.do("POST", path, "tax-ops", base)
	require.Equal(t, want, code, "%v", r)
	return r
}

// verdict returns the domain result nested under the response key, for example r["licence"]["licence"].
func verdict(r map[string]any, key string) map[string]any {
	m, _ := r[key].(map[string]any)
	v, _ := m[key].(map[string]any)
	return v
}

func outcomeOf(r map[string]any, key string) string {
	m, _ := r[key].(map[string]any)
	o, _ := m["outcome"].(string)
	return o
}

func TestRegistrationIdentifier_FormatIsCheckedAndTheIdentifierIsNeverStored(t *testing.T) {
	s := newRegistrationScenario(t)
	s.release(t)
	const path = "/v1/registration-identifiers:validate"
	call := func(id string, want int) map[string]any {
		return s.regCall(t, path, map[string]any{"registration_code": "VAT_NUMBER", "identifier": id, "as_of": "2026-03-10"}, want)
	}

	assert.Equal(t, "IDENTIFIER_VALID", verdict(call("gb 123-456-789", 200), "registration")["outcome"], "separators and case are normalised")
	// The identifier under test is deliberately not one of the rule's own published examples, so any trace of it in evidence is a leak.
	r := call("GB 555-666-777", 200)
	v := verdict(r, "registration")
	require.Equal(t, "IDENTIFIER_VALID", v["outcome"])
	assert.Equal(t, "VAT", v["kind"])
	assert.Equal(t, []any{"AUTHORITY_CERTIFICATE"}, v["evidence_required"])
	assert.Equal(t, "VAT_REG_THRESHOLD", v["threshold_monitor"])
	assert.Contains(t, strings.Join(toStrings(v["explanation"]), " "), "format check only")

	assert.Equal(t, "IDENTIFIER_INVALID", outcomeOf(call("GB12345", 200), "registration"))
	assert.Equal(t, "IDENTIFIER_INVALID", outcomeOf(call("XX123456789", 200), "registration"))

	// Evidence anchors the verdict on the pack release and rule, and holds a digest, never the identifier.
	id := r["decision_id"].(string)
	var kind, outcome, reqText, respText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, outcome, request::text, response::text FROM rule_decision_evidence WHERE decision_id=$1`, id).
		Scan(&kind, &outcome, &reqText, &respText))
	assert.Equal(t, "REGISTRATION", kind)
	assert.Equal(t, "IDENTIFIER_VALID", outcome)
	assert.Contains(t, reqText, "sha256:")
	for _, forbidden := range []string{"555666777", "555-666-777"} {
		assert.NotContains(t, reqText, forbidden, "the request evidence must not hold the identifier")
		assert.NotContains(t, respText, forbidden, "the response evidence must not hold the identifier")
	}

	// Refusals and non-answers.
	assert.Equal(t, "NO_RULE", outcomeOf(s.regCall(t, path, map[string]any{"registration_code": "UNKNOWN_TYPE", "identifier": "x", "as_of": "2026-03-10"}, 200), "registration"))
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", outcomeOf(s.regCall(t, path, map[string]any{"jurisdiction": "ATLANTIS", "registration_code": "VAT_NUMBER", "identifier": "x", "as_of": "2026-03-10"}, 422), "registration"))
	s.regCall(t, path, map[string]any{"registration_code": "VAT_NUMBER", "identifier": "GB123456789", "as_of": "10/03/2026"}, 400)
	s.regCall(t, path, map[string]any{"registration_code": "VAT_NUMBER", "identifier": strings.Repeat("9", 257), "as_of": "2026-03-10"}, 400)
	s.regCall(t, path, map[string]any{"registration_code": "VAT_NUMBER", "as_of": "2026-03-10"}, 400)

	// Idempotency is keyed on the digest, so a replay is the same decision and a different identifier is a conflict.
	body := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "registration_code": "VAT_NUMBER", "identifier": "GB123456789", "as_of": "2026-03-10"}
	code, a := s.e.doWithHeaders("POST", path, "tax-ops", body, map[string]string{"Idempotency-Key": "reg-1"})
	mustStatus(t, 200, code, a)
	code, b := s.e.doWithHeaders("POST", path, "tax-ops", body, map[string]string{"Idempotency-Key": "reg-1"})
	mustStatus(t, 200, code, b)
	assert.Equal(t, a["decision_id"], b["decision_id"])
	body["identifier"] = "GB987654321"
	code, c := s.e.doWithHeaders("POST", path, "tax-ops", body, map[string]string{"Idempotency-Key": "reg-1"})
	mustStatus(t, 409, code, c)
}

func toStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestLicence_StatusIsDerivedFromTheTypeInForce(t *testing.T) {
	s := newRegistrationScenario(t)
	s.release(t)
	const path = "/v1/licences:evaluate"
	call := func(extra map[string]any, want int) map[string]any {
		base := map[string]any{"licence_code": "TRADING_LICENCE", "issue_date": "2026-01-15", "as_of": "2026-06-01"}
		for k, v := range extra {
			base[k] = v
		}
		return s.regCall(t, path, base, want)
	}

	r := call(nil, 200)
	v := verdict(r, "licence")
	require.Equal(t, "LICENCE_EVALUATED", v["outcome"])
	assert.Equal(t, "ACTIVE", v["status"])
	assert.Equal(t, "2027-01-14", v["valid_until"])
	assert.Equal(t, "2026-11-15", v["renewal_window_opens"])
	assert.Equal(t, "2027-02-13", v["grace_ends_on"])

	for asOf, want := range map[string]string{"2026-01-14": "NOT_YET_VALID", "2026-11-15": "RENEWAL_WINDOW", "2027-01-15": "IN_GRACE", "2027-02-14": "EXPIRED"} {
		assert.Equal(t, want, verdict(call(map[string]any{"as_of": asOf}, 200), "licence")["status"], asOf)
	}

	// A stated expiry beyond the maximum term is refused, not trusted; an expiry before issue is unusable.
	assert.Equal(t, "LICENCE_TERM_EXCEEDED", outcomeOf(call(map[string]any{"expiry_date": "2027-01-15"}, 422), "licence"))
	assert.Equal(t, "INVALID_FACTS", outcomeOf(call(map[string]any{"expiry_date": "2026-01-01"}, 422), "licence"))
	assert.Equal(t, "2026-09-30", verdict(call(map[string]any{"expiry_date": "2026-09-30"}, 200), "licence")["valid_until"])
	call(map[string]any{"issue_date": "15 Jan"}, 400)
	call(map[string]any{"expiry_date": "never"}, 400)

	var kind string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind FROM rule_decision_evidence WHERE decision_id=$1`, r["decision_id"]).Scan(&kind))
	assert.Equal(t, "LICENCE", kind)
}

func TestScheme_EligibilityAndLockInDeferral(t *testing.T) {
	s := newRegistrationScenario(t)
	s.release(t)
	const path = "/v1/scheme-elections:evaluate"
	call := func(extra map[string]any, want int) map[string]any {
		base := map[string]any{"scheme_code": "SMALL_BUSINESS_SCHEME", "metric_value": "120000", "start_date": "2026-04-01", "as_of": "2026-03-20"}
		for k, v := range extra {
			base[k] = v
		}
		return s.regCall(t, path, base, want)
	}

	v := verdict(call(nil, 200), "scheme")
	require.Equal(t, "SCHEME_ELIGIBLE", v["outcome"])
	assert.Equal(t, "2027-04-01", v["lock_in_ends"])
	assert.Equal(t, "ANNUAL_TURNOVER LTE 150000", v["comparison"])

	assert.Equal(t, "SCHEME_ELIGIBLE", outcomeOf(call(map[string]any{"metric_value": "150000"}, 200), "scheme"), "LTE includes the threshold")
	ne := call(map[string]any{"metric_value": "150000.01"}, 200)
	assert.Equal(t, "SCHEME_NOT_ELIGIBLE", outcomeOf(ne, "scheme"))

	early := verdict(call(map[string]any{"cancel_date": "2026-06-01"}, 200), "scheme")
	assert.Equal(t, true, early["deferred_by_lock_in"])
	assert.Equal(t, "2027-04-01", early["earliest_cancellation_effective"])
	late := verdict(call(map[string]any{"cancel_date": "2027-05-01"}, 200), "scheme")
	assert.Nil(t, late["deferred_by_lock_in"])
	assert.Equal(t, "2027-05-31", late["earliest_cancellation_effective"])

	assert.Equal(t, "INVALID_FACTS", outcomeOf(call(map[string]any{"metric_value": "1e5"}, 422), "scheme"))
	assert.Equal(t, "INVALID_FACTS", outcomeOf(call(map[string]any{"metric_value": "-5"}, 422), "scheme"))
	call(map[string]any{"start_date": "soon"}, 400)
	call(map[string]any{"cancel_date": "later"}, 400)

	var kind, outcome string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, outcome FROM rule_decision_evidence WHERE decision_id=$1`, ne["decision_id"]).Scan(&kind, &outcome))
	assert.Equal(t, "SCHEME", kind)
	assert.Equal(t, "SCHEME_NOT_ELIGIBLE", outcome, "a not-eligible verdict is evidence too")
}

func TestThresholdMonitor_BreachDateEffectiveDateAndTheRuleInForce(t *testing.T) {
	s := newRegistrationScenario(t)
	s.release(t)
	const path = "/v1/threshold-monitors:evaluate"
	data := []map[string]any{
		{"date": "2026-01-31", "amount": "30000"}, {"date": "2026-02-28", "amount": "30000"},
		{"date": "2026-03-31", "amount": "30000"}, {"date": "2026-04-30", "amount": "1"},
	}
	call := func(extra map[string]any, want int) map[string]any {
		base := map[string]any{"monitor_code": "VAT_REG_THRESHOLD", "as_of": "2026-06-15", "observations": data}
		for k, v := range extra {
			base[k] = v
		}
		return s.regCall(t, path, base, want)
	}

	// 2026 rule: threshold 85000, so the third month (90000 in total) already breaches it.
	r := call(nil, 200)
	v := verdict(r, "threshold")
	require.Equal(t, "THRESHOLD_EVALUATED", v["outcome"])
	assert.Equal(t, true, v["breached"])
	assert.Equal(t, "2026-03-31", v["breach_date"])
	assert.Equal(t, "90000", v["total_at_breach"])
	assert.Equal(t, "2026-04-30", v["effective_date"], "end of the month following the breach")
	assert.Equal(t, "2026-04-30", v["registration_effective_date"], "the value the obligation engine takes as registration_date")
	assert.Equal(t, "85000", v["threshold"])

	// The rule in force on as_of decides: for 2025 the old threshold of 90000 applies, and 90000 is not GREATER than 90000.
	old := verdict(call(map[string]any{"as_of": "2025-12-31", "observations": []map[string]any{
		{"date": "2025-10-31", "amount": "45000"}, {"date": "2025-11-30", "amount": "45000"}}}, 200), "threshold")
	assert.Equal(t, false, old["breached"])
	assert.Equal(t, "90000", old["threshold"])
	assert.Equal(t, "90000", old["total"])
	assert.Empty(t, old["registration_effective_date"])

	// Observations after as_of are ignored: re-running for an earlier date reproduces the earlier answer.
	early := verdict(call(map[string]any{"as_of": "2026-02-28"}, 200), "threshold")
	assert.Equal(t, false, early["breached"])
	assert.EqualValues(t, 2, early["observations_after_as_of_ignored"])
	assert.Equal(t, "25000", early["headroom"])

	// Unusable facts are refused, never guessed.
	assert.Equal(t, "INVALID_FACTS", outcomeOf(call(map[string]any{"observations": []map[string]any{{"date": "2026-01-01", "amount": "-5"}}}, 422), "threshold"))
	assert.Equal(t, "INVALID_FACTS", outcomeOf(call(map[string]any{"observations": []map[string]any{{"date": "2026-01-01", "amount": "1e3"}}}, 422), "threshold"))
	assert.Equal(t, "NO_RULE", outcomeOf(call(map[string]any{"monitor_code": "NOPE"}, 200), "threshold"))
	call(map[string]any{"as_of": "June"}, 400)

	// Evidence and idempotency.
	var kind, outcome string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, outcome FROM rule_decision_evidence WHERE decision_id=$1`, r["decision_id"]).Scan(&kind, &outcome))
	assert.Equal(t, "THRESHOLD", kind)
	assert.Equal(t, "THRESHOLD_EVALUATED", outcome)
	body := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "monitor_code": "VAT_REG_THRESHOLD", "as_of": "2026-06-15", "observations": data}
	code, a := s.e.doWithHeaders("POST", path, "tax-ops", body, map[string]string{"Idempotency-Key": "thr-1"})
	mustStatus(t, 200, code, a)
	code, b := s.e.doWithHeaders("POST", path, "tax-ops", body, map[string]string{"Idempotency-Key": "thr-1"})
	mustStatus(t, 200, code, b)
	assert.Equal(t, a["decision_id"], b["decision_id"])
	body["as_of"] = "2026-06-16"
	code, c := s.e.doWithHeaders("POST", path, "tax-ops", body, map[string]string{"Idempotency-Key": "thr-1"})
	mustStatus(t, 409, code, c)

	// A parameter-only family is never calculated with.
	code, c = s.e.do("POST", "/v1/regulatory-calculations:execute", "payroll-engine", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "rule_domain": "THRESHOLD_MONITOR",
		"rule_code": "VAT_REG_THRESHOLD", "effective_at": "2026-08-01T00:00:00Z", "taxable_amount": "1", "currency": "GBP"})
	mustStatus(t, 422, code, c)
	assert.Equal(t, "PARAMETER_ONLY", calcBody(c)["outcome"])
}

func TestRegistrationEvidence_DatabaseRefusesAVerdictWithoutItsBasis(t *testing.T) {
	newRegistrationScenario(t)
	for _, c := range [][2]string{{"REGISTRATION", "IDENTIFIER_VALID"}, {"LICENCE", "LICENCE_EVALUATED"}, {"SCHEME", "SCHEME_ELIGIBLE"}, {"THRESHOLD", "THRESHOLD_EVALUATED"}} {
		_, err := pool.Exec(ctx, `INSERT INTO rule_decision_evidence (requested_by, request_digest, request, effective_at, outcome, response, decision_kind)
			VALUES ('x','sha256:`+strings.Repeat("0", 64)+`','{}'::jsonb,NOW(),$1,'{}'::jsonb,$2)`, c[1], c[0])
		require.Error(t, err, "%s verdict cannot be stored without its pack and rule basis", c[1])
	}
	_, err := pool.Exec(ctx, `INSERT INTO rule_decision_evidence (requested_by, request_digest, request, effective_at, outcome, response, decision_kind)
		VALUES ('x','sha256:`+strings.Repeat("0", 64)+`','{}'::jsonb,NOW(),'IDENTIFIER_VALID','{}'::jsonb,'NOT_A_KIND')`)
	require.Error(t, err, "an unknown decision kind is refused")
}

func TestRegistrationParameters_AreValidatedAtTheEdgeAndByTheCompiler(t *testing.T) {
	s := newRegistrationScenario(t)
	rid := s.rules["VAT_REG_THRESHOLD@2025-04-06"]
	require.NotEmpty(t, rid)
	bad := []string{
		// threshold must be a decimal string, not a JSON number (JUR-NEG-25)
		`{"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":90000,"breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE"}}}}`,
		// a monitor detects a threshold being reached
		`{"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":{"kind":"ROLLING","months":12},"operator":"LT","threshold":"5","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"IMMEDIATE"}}}}`,
		// next period start needs a static period
		`{"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":{"kind":"ROLLING","months":12},"operator":"GT","threshold":"5","breach":{"effect":"NOTIFY_ONLY","effective":{"kind":"NEXT_PERIOD_START"}}}}`,
		// a pattern proved wrong by its own example
		`{"family":"REGISTRATION_TYPE","registration":{"kind":"VAT","authority":"A","identifier":{"pattern":"^GB[0-9]{9}$","normalize":"NONE","examples_valid":["GB1"],"examples_invalid":["XX"]}}}`,
		// an unanchored pattern
		`{"family":"REGISTRATION_TYPE","registration":{"kind":"VAT","authority":"A","identifier":{"pattern":"GB[0-9]{9}","normalize":"NONE","examples_valid":["GB123456789"],"examples_invalid":["XX"]}}}`,
		// another family's field on a registration family
		`{"family":"LICENCE_TYPE","rate":"0.2","licence":{"authority":"A","renewal_notice_days":0,"grace_days":0}}`,
		// registration fields on a tax rate
		`{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2},"monitor":{"metric":"TAXABLE_TURNOVER"}}`,
	}
	for _, b := range bad {
		code, r := s.e.do("PUT", "/v1/admin/rules/"+rid+"/parameters", "rule-author", map[string]any{"parameters": rawMap(t, b)})
		assert.Equal(t, 400, code, "%s: %v", b, r)
	}

	// The compiler refuses a monitor filed under the wrong domain (JUR-C092) and a reserved-domain rule with no parameters (JUR-C093).
	s.packRef = uniqueRef("jur.p")
	code, r := s.e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRef, "pack_name": "P", "owner": "P"})
	mustStatus(t, 201, code, r)
	wrong, _, err := st.CreateRule(ctx, domainCreate(s, "PAYROLL", "WRONG_DOMAIN"))
	require.NoError(t, err)
	code, r = s.e.do("PUT", "/v1/admin/rules/"+wrong.JurisdictionRuleID+"/provenance", "author", map[string]any{
		"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = s.e.do("PUT", "/v1/admin/rules/"+wrong.JurisdictionRuleID+"/parameters", "rule-author", map[string]any{"parameters": rawMap(t, monV1)})
	mustStatus(t, 200, code, r)
	code, r = s.e.do("POST", "/v1/admin/packs/"+s.packRef+"/versions", "author", manifest(s.packRef, "2026.08.1", s.jur.JurisdictionCode,
		map[string]any{"rule_modules": []string{wrong.JurisdictionRuleID}, "effective_from": "2025-01-01"}))
	mustStatus(t, 201, code, r)
	s.e.do("POST", s.path("/submit-review"), "author", nil)
	code, r = s.e.do("POST", s.path("/compile"), "compiler", nil)
	mustStatus(t, 422, code, r)
	assert.True(t, strings.Contains(mustString(r["report"]), "JUR-C092"), "%v", r["report"])
}
