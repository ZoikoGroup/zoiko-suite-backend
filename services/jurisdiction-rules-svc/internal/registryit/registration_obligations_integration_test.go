//go:build integration

// ZS-JUR-001 s15 and s16 end to end: the registrations an entity holds decide which obligations apply, and each due date is derived
// from the verified released pack (never entered). Tracking each obligation stays with obligations-svc.
package registryit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

const regPath = "/v1/registration-obligations:resolve"

// regTypeRule is a VAT registration that brings the VAT_RETURN obligation the due scenario defines, a second obligation the pack does
// NOT define, and names a threshold monitor the pack does not define: the compiler warns about both gaps (JUR-W031).
const regTypeRule = `{"family":"REGISTRATION_TYPE","registration":{"kind":"VAT","authority":"Example Revenue","obligations":["VAT_RETURN","MISSING_OBLIGATION"],
	"threshold_monitor":"NO_SUCH_MONITOR","identifier":{"pattern":"^GB[0-9]{9}$","normalize":"NONE","examples_valid":["GB123456789"],"examples_invalid":["GB12345"]}}}`

// registrationPack releases the due scenario's calendar and obligation rule together with a registration type, and returns the compile report.
func registrationPack(t *testing.T, s *dueScenario) (ruleID, compileReport string) {
	t.Helper()
	e := s.e
	from := pyStart2025.AddDate(0, 0, 0)
	rule, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: s.jur.JurisdictionID, RuleDomain: "REGISTRATION_TYPE", RuleCode: "VAT_NUMBER", RuleName: "VAT number",
		EffectiveFrom: from, RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	ruleID = rule.JurisdictionRuleID
	code, r := e.do("PUT", "/v1/admin/rules/"+ruleID+"/provenance", "author", map[string]any{"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = e.do("PUT", "/v1/admin/rules/"+ruleID+"/parameters", "rule-author", map[string]any{"parameters": rawMap(t, regTypeRule)})
	mustStatus(t, 200, code, r)

	s.packRefD = uniqueRef("jur.r")
	code, r = e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRefD, "pack_name": "Registrations", "owner": "Tax Operations"})
	mustStatus(t, 201, code, r)
	const version = "2026.08.1"
	p := func(suffix string) string { return "/v1/admin/packs/" + s.packRefD + "/versions/" + version + suffix }
	code, r = e.do("POST", "/v1/admin/packs/"+s.packRefD+"/versions", "author", manifest(s.packRefD, version, s.jur.JurisdictionCode, map[string]any{
		"rule_modules": []string{ruleID}, "calendar_modules": []string{s.calV1}, "obligation_modules": []string{s.oblID}}))
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/submit-review"), "author", nil)
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", p("/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)
	compileReport = mustString(r["report"])
	code, r = e.do("POST", p("/sign"), "signer", nil)
	mustStatus(t, 200, code, r)

	jc := s.jur.JurisdictionCode
	oblCase := func(id, class, jur string, in, expect map[string]any) map[string]any {
		base := map[string]any{"jurisdiction": jur, "obligation_code": "VAT_RETURN"}
		for k, v := range in {
			base[k] = v
		}
		return map[string]any{"id": id, "class": class, "input": base, "expect": expect}
	}
	ruleCase := func(id, class, at string, expect map[string]any) map[string]any {
		return map[string]any{"id": id, "class": class, "expect": expect,
			"input": map[string]any{"jurisdiction": jc, "rule_domain": "REGISTRATION_TYPE", "rule_code": "VAT_NUMBER", "effective_at": at}}
	}
	bundle := map[string]any{"bundle_version": "1", "cases": []map[string]any{
		oblCase("g1", "GOLDEN", jc, map[string]any{"period_end": "2026-08-31"}, map[string]any{"outcome": "DUE_DATE_CALCULATED", "due_date": "2026-10-07", "obligation_rule_id": s.oblID}),
		oblCase("b1", "BOUNDARY", jc, map[string]any{"period_end": "2026-09-30"}, map[string]any{"outcome": "DUE_DATE_CALCULATED", "due_date": "2026-11-10"}),
		oblCase("n1", "NEGATIVE", jc, map[string]any{"period_end": "2026-09-30", "extension_days": 15, "extension_evidence_ref": "E-1"}, map[string]any{"outcome": "EXTENSION_NOT_PERMITTED"}),
		oblCase("n2", "NEGATIVE", "ATLANTIS", map[string]any{"period_end": "2026-09-30"}, map[string]any{"outcome": "UNSUPPORTED_JURISDICTION"}),
		ruleCase("r1", "GOLDEN", "2026-03-10T00:00:00Z", map[string]any{"outcome": "RESOLVED", "rule_id": ruleID}),
		ruleCase("r2", "BOUNDARY", from.Format("2006-01-02T15:04:05Z"), map[string]any{"outcome": "RESOLVED", "rule_id": ruleID}),
		ruleCase("r3", "BOUNDARY", "2025-04-05T23:59:59.999999999Z", map[string]any{"outcome": "NO_RULE"}),
	}}
	code, r = e.do("POST", p("/test-bundle"), "tester", bundle)
	require.Contains(t, []int{200, 201}, code, "%v", r)
	code, r = e.do("POST", p("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	require.Equal(t, true, r["passed"], "%v", r["result"])
	code, r = e.do("POST", p("/reviews"), "reviewer-1", map[string]any{"role": "TAX", "decision": "APPROVE"})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/publish"), "releaser", nil)
	mustStatus(t, 200, code, r)
	return ruleID, compileReport
}

func (s *dueScenario) applicability(t *testing.T, body map[string]any, want int, headers map[string]string) map[string]any {
	t.Helper()
	base := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "as_of": "2026-10-15"}
	for k, v := range body {
		base[k] = v
	}
	code, r := s.e.doWithHeaders("POST", regPath, "tax-engine", base, headers)
	require.Equal(t, want, code, "%v", r)
	return r
}

func regItems(r map[string]any) []any {
	a, _ := r["applicability"].(map[string]any)
	items, _ := a["registrations"].([]any)
	return items
}

func obligationsOf(item any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, o := range item.(map[string]any)["obligations"].([]any) {
		m := o.(map[string]any)
		out[m["obligation_code"].(string)] = m
	}
	return out
}

func TestRegistrationObligations_ApplicabilityFollowsTheRegistrationsAndDueDatesAreDerived(t *testing.T) {
	s := newDueScenario(t)
	_, report := registrationPack(t, s)

	// The compiler warned, without refusing, about the two references the pack cannot answer (JUR-W031), and not about the one it can.
	assert.Contains(t, report, "JUR-W031")
	assert.Contains(t, report, "MISSING_OBLIGATION")
	assert.Contains(t, report, "NO_SUCH_MONITOR")
	assert.NotContains(t, report, "names obligation VAT_RETURN")

	reg := func(date string, extra map[string]any) map[string]any {
		m := map[string]any{"registration_code": "VAT_NUMBER", "registration_date": date}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	facts := map[string]any{"period_end": "2026-09-30"}

	// A registration in force brings its obligations, each derived: VAT_RETURN is due 10 Nov (7 Nov, a Saturday, then the 9 Nov closure), and the
	// obligation the pack does not define is reported as such, never guessed.
	r := s.applicability(t, map[string]any{"registrations": []any{reg("2026-02-01", nil)}, "facts": facts}, 200, nil)
	items := regItems(r)
	require.Len(t, items, 1)
	it := items[0].(map[string]any)
	assert.Equal(t, true, it["in_force"])
	assert.Equal(t, "APPLIES", it["outcome"])
	assert.Equal(t, "VAT", it["kind"])
	ob := obligationsOf(items[0])
	assert.Equal(t, "DUE_DATE_CALCULATED", ob["VAT_RETURN"]["outcome"])
	assert.Equal(t, "2026-11-10", ob["VAT_RETURN"]["due_date"])
	assert.NotNil(t, ob["VAT_RETURN"]["due"], "the derivation is explained: pack release, rule version, calendar version, steps")
	assert.Equal(t, "NO_OBLIGATION_RULE", ob["MISSING_OBLIGATION"]["outcome"])

	// A registration that has not started, or has ended, brings nothing.
	for name, regn := range map[string]map[string]any{
		"not started": reg("2026-12-01", nil),
		"ended":       reg("2026-02-01", map[string]any{"deregistration_date": "2026-10-01"}),
	} {
		items = regItems(s.applicability(t, map[string]any{"registrations": []any{regn}, "facts": facts}, 200, nil))
		it = items[0].(map[string]any)
		assert.Equal(t, false, it["in_force"], name)
		assert.Equal(t, "NOT_IN_FORCE", it["outcome"], name)
		assert.Empty(t, it["obligations"], name)
	}
	// The end day is exclusive: on the deregistration date itself the registration no longer applies; the day before, it does.
	last := regItems(s.applicability(t, map[string]any{"as_of": "2026-09-30", "registrations": []any{reg("2026-02-01", map[string]any{"deregistration_date": "2026-10-01"})}, "facts": facts}, 200, nil))
	assert.Equal(t, true, last[0].(map[string]any)["in_force"])
	gone := regItems(s.applicability(t, map[string]any{"as_of": "2026-10-01", "registrations": []any{reg("2026-02-01", map[string]any{"deregistration_date": "2026-10-01"})}, "facts": facts}, 200, nil))
	assert.Equal(t, false, gone[0].(map[string]any)["in_force"])

	// An unknown registration type is explicit: nothing is assumed about the obligations it would bring.
	unk := regItems(s.applicability(t, map[string]any{"registrations": []any{map[string]any{"registration_code": "UNKNOWN_TYPE", "registration_date": "2026-02-01"}}, "facts": facts}, 200, nil))
	assert.Equal(t, "NO_RULE", unk[0].(map[string]any)["outcome"])
	assert.Empty(t, unk[0].(map[string]any)["obligations"])

	// Missing period facts: the obligation is reported with its reason, not given a made-up date.
	nofacts := regItems(s.applicability(t, map[string]any{"registrations": []any{reg("2026-02-01", nil)}}, 200, nil))
	nf := obligationsOf(nofacts[0])["VAT_RETURN"]
	assert.Contains(t, []string{"INVALID_FACTS", "NO_OBLIGATION_RULE"}, nf["outcome"], "the engine refuses either way; which one depends on whether the pack is in force on the registration date")
	assert.Nil(t, nf["due_date"], "no date is invented")
	assert.Contains(t, mustString(nf["reason"]), "no period fact", "and the caller is told the cause")

	// Several registrations are answered together, in order.
	two := regItems(s.applicability(t, map[string]any{"registrations": []any{reg("2026-02-01", nil), reg("2026-12-01", nil)}, "facts": facts}, 200, nil))
	require.Len(t, two, 2)
	assert.Equal(t, true, two[0].(map[string]any)["in_force"])
	assert.Equal(t, false, two[1].(map[string]any)["in_force"])
}

func TestRegistrationObligations_EvidenceIsIdempotentAndRequestsAreValidated(t *testing.T) {
	s := newDueScenario(t)
	registrationPack(t, s)
	body := map[string]any{"registrations": []any{map[string]any{"registration_code": "VAT_NUMBER", "registration_date": "2026-02-01"}}, "facts": map[string]any{"period_end": "2026-09-30"}}

	r := s.applicability(t, body, 200, map[string]string{"Idempotency-Key": "app-1"})
	var kind, outcome string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, outcome FROM rule_decision_evidence WHERE decision_id=$1`, r["decision_id"]).Scan(&kind, &outcome))
	assert.Equal(t, "APPLICABILITY", kind)
	assert.Equal(t, "APPLICABILITY_RESOLVED", outcome)
	again := s.applicability(t, body, 200, map[string]string{"Idempotency-Key": "app-1"})
	assert.Equal(t, r["decision_id"], again["decision_id"])
	assert.Equal(t, true, again["replayed"])
	body["as_of"] = "2026-10-16"
	s.applicability(t, body, 409, map[string]string{"Idempotency-Key": "app-1"})

	one := func(m map[string]any) []any { return []any{m} }
	good := map[string]any{"registration_code": "VAT_NUMBER", "registration_date": "2026-02-01"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range good {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	s.applicability(t, map[string]any{"registrations": []any{}}, 400, nil)
	s.applicability(t, map[string]any{"registrations": make([]any, 26)}, 400, nil)
	s.applicability(t, map[string]any{"registrations": one(with("registration_date", "Feb 1"))}, 400, nil)
	s.applicability(t, map[string]any{"registrations": one(with("registration_code", ""))}, 400, nil)
	s.applicability(t, map[string]any{"registrations": one(with("deregistration_date", "2026-02-01"))}, 400, nil) // not after the registration date
	s.applicability(t, map[string]any{"registrations": one(good), "as_of": "soon"}, 400, nil)
	s.applicability(t, map[string]any{"registrations": one(good), "jurisdiction": ""}, 400, nil)
}
