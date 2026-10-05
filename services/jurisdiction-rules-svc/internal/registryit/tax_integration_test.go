//go:build integration

// ZS-JUR-001 Wave 4 (tax half) end to end. DECISION under test: calculation
// parameters (rates, thresholds, rounding) live in released, immutable,
// sourced, tested, certified pack rule modules as exact decimal strings, and
// are executed from the verified artifact alone.
package registryit_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

const taxParams = `{"family":"TAX_BANDS","bands":[{"up_to":"1000","rate":"0.1"},{"up_to":null,"rate":"0.2"}],"rounding":{"mode":"HALF_UP","scale":2}}`

type taxScenario struct {
	*fixture
	e       *env
	keyRef  string
	packRef string
}

func newTaxScenario(t *testing.T) *taxScenario {
	t.Helper()
	keyRef := "tax-key-" + strings.ToLower(uuid.NewString()[:6])
	signer, pub := newSigner(t, keyRef)
	e := newEnvResolver(signer, []string{"RELEASED"}, 0)
	f := newFixture(t, e)
	code, r := e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": pub})
	mustStatus(t, 201, code, r)
	return &taxScenario{fixture: f, e: e, keyRef: keyRef}
}

func (s *taxScenario) setParams(t *testing.T, raw string, want int) map[string]any {
	t.Helper()
	var p any
	require.NoError(t, json.Unmarshal([]byte(raw), &p))
	code, r := s.e.do("PUT", "/v1/admin/rules/"+s.ruleID+"/parameters", "rule-author", map[string]any{"parameters": p})
	require.Equal(t, want, code, "%v", r)
	return r
}

// toReviewAndCompile creates the pack, drafts a version carrying the rule, and compiles and signs it.
func (s *taxScenario) toReviewAndCompile(t *testing.T) string {
	t.Helper()
	s.packRef = uniqueRef("jur.t")
	code, r := s.e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRef, "pack_name": "Tax", "owner": "Tax Engineering"})
	mustStatus(t, 201, code, r)
	code, r = s.e.do("POST", "/v1/admin/packs/"+s.packRef+"/versions", "author", manifest(s.packRef, "2026.08.1", s.jur.JurisdictionCode, map[string]any{"rule_modules": []string{s.ruleID}}))
	mustStatus(t, 201, code, r)
	code, r = s.e.do("POST", s.path("/submit-review"), "author", nil)
	mustStatus(t, 200, code, r)
	code, r = s.e.do("POST", s.path("/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)
	return r["artifact_digest"].(string)
}

func (s *taxScenario) path(suffix string) string {
	return "/v1/admin/packs/" + s.packRef + "/versions/2026.08.1" + suffix
}

// bundle builds the complete rule and calculation coverage from the compiled artifact.
func (s *taxScenario) bundle(t *testing.T, withCalc bool) map[string]any {
	t.Helper()
	code, got := s.e.do("GET", "/v1/packs/"+s.packRef+"/versions/2026.08.1/artifact", "", nil)
	mustStatus(t, 200, code, got)
	raw, err := json.Marshal(got["artifact"].(map[string]any)["artifact"])
	require.NoError(t, err)
	doc, err := domain.ParseArtifact(raw)
	require.NoError(t, err)
	rule := doc.Rules[0]
	jc := s.jur.JurisdictionCode
	rfc := func(tt time.Time) string { return tt.UTC().Format(time.RFC3339Nano) }
	inside := rule.EffectiveFrom.Add(time.Hour)
	rc := func(id, class, jur string, at time.Time, expect map[string]any) map[string]any {
		return map[string]any{"id": id, "class": class, "input": map[string]any{"jurisdiction": jur, "rule_domain": "TAX", "rule_code": rule.RuleCode, "effective_at": rfc(at)}, "expect": expect}
	}
	cc := func(id, class, amount string, expect map[string]any) map[string]any {
		e := map[string]any{"rule_id": rule.RuleID}
		for k, v := range expect {
			e[k] = v
		}
		return map[string]any{"id": id, "class": class, "input": map[string]any{"jurisdiction": jc, "rule_domain": "TAX", "rule_code": rule.RuleCode, "effective_at": rfc(inside), "taxable_amount": amount}, "expect": e}
	}
	res := map[string]any{"outcome": domain.OutcomeResolved, "rule_id": rule.RuleID}
	cases := []map[string]any{
		rc("rg", "GOLDEN", jc, inside, res),
		rc("rb1", "BOUNDARY", jc, rule.EffectiveFrom, res),
		rc("rb2", "BOUNDARY", jc, rule.EffectiveFrom.Add(-time.Nanosecond), map[string]any{"outcome": domain.OutcomeNoRule}),
		rc("rn", "NEGATIVE", "ATLANTIS", inside, map[string]any{"outcome": domain.OutcomeUnsupported}),
	}
	if withCalc {
		cases = append(cases,
			cc("cg", "GOLDEN", "1500", map[string]any{"outcome": "CALCULATED", "tax_amount": "200.00"}),
			cc("cb1", "BOUNDARY", "1000", map[string]any{"outcome": "CALCULATED", "tax_amount": "100.00"}),
			cc("cb2", "BOUNDARY", "1000.01", map[string]any{"outcome": "CALCULATED", "tax_amount": "100.00"}),
			cc("cn", "NEGATIVE", "-5", map[string]any{"outcome": "INVALID_FACTS"}))
	}
	return map[string]any{"bundle_version": "1", "cases": cases}
}

func (s *taxScenario) certifyAndRelease(t *testing.T) {
	t.Helper()
	e := s.e
	code, r := e.do("POST", s.path("/sign"), "signer", nil)
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", s.path("/test-bundle"), "tester", s.bundle(t, true))
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", s.path("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	require.Equal(t, true, r["passed"], "%v", r["result"])
	code, r = e.do("POST", s.path("/reviews"), "reviewer-tax", map[string]any{"role": "TAX", "decision": "APPROVE"})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", s.path("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", s.path("/publish"), "releaser", nil)
	mustStatus(t, 200, code, r)
}

func (s *taxScenario) execute(t *testing.T, body map[string]any, want int) map[string]any {
	t.Helper()
	base := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "rule_domain": "TAX", "rule_code": "STD",
		"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "taxable_amount": "1500", "currency": "GBP"}
	for k, v := range body {
		base[k] = v
	}
	code, r := s.e.do("POST", "/v1/regulatory-calculations:execute", "tax-engine", base)
	require.Equal(t, want, code, "%v", r)
	return r
}

func calcBody(r map[string]any) map[string]any { c, _ := r["calculation"].(map[string]any); return c }

func TestRuleParameters_AreValidatedFrozenAndDefendedInTheDatabase(t *testing.T) {
	s := newTaxScenario(t)
	e := s.e

	// Strict validation: rates must be decimal STRINGS (JUR-NEG-25), closed families only.
	for name, raw := range map[string]string{
		"rate as a number": `{"family":"TAX_RATE","rate":0.2,"rounding":{"mode":"HALF_UP","scale":2}}`,
		"formula family":   `{"family":"FORMULA","rounding":{"mode":"HALF_UP","scale":2}}`,
		"rate above 100%":  `{"family":"TAX_RATE","rate":"1.5","rounding":{"mode":"HALF_UP","scale":2}}`,
		"no rounding":      `{"family":"TAX_RATE","rate":"0.2"}`,
		"unknown field":    `{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2},"formula":"x"}`,
	} {
		r := s.setParams(t, raw, 400)
		assert.Equal(t, "invalid_parameters", r["error"], name)
	}
	code, r := e.do("PUT", "/v1/admin/rules/"+s.ruleID+"/parameters", "rule-author", map[string]any{})
	mustStatus(t, 400, code, r) // "parameters" is required; null clears
	code, _ = e.do("PUT", "/v1/admin/rules/"+s.ruleID+"/parameters", "", map[string]any{"parameters": nil})
	assert.Equal(t, 401, code)
	e.auth.deny = "jurisdiction_rule.set_parameters"
	code, _ = e.do("PUT", "/v1/admin/rules/"+s.ruleID+"/parameters", "rule-author", map[string]any{"parameters": nil})
	assert.Equal(t, 403, code)
	e.auth.deny = ""
	code, _ = e.do("PUT", "/v1/admin/rules/"+uuid.NewString()+"/parameters", "rule-author", map[string]any{"parameters": nil})
	assert.Equal(t, 404, code)

	// Set, read back (canonical), clear, set again.
	got := s.setParams(t, taxParams, 200)
	assert.Equal(t, "TAX_BANDS", got["parameters"].(map[string]any)["family"])
	code, got = e.do("GET", "/v1/rules/"+s.ruleID+"/parameters", "", nil)
	mustStatus(t, 200, code, got)
	assert.Equal(t, "1000", got["parameters"].(map[string]any)["bands"].([]any)[0].(map[string]any)["up_to"])
	code, got = e.do("PUT", "/v1/admin/rules/"+s.ruleID+"/parameters", "rule-author", map[string]any{"parameters": nil})
	mustStatus(t, 200, code, got)
	assert.Nil(t, got["parameters"])
	s.setParams(t, taxParams, 200)

	// Frozen with the rule: past DRAFT the API refuses, and so does the database.
	_, err := pool.Exec(ctx, `UPDATE jurisdiction_rules SET rule_status='ACTIVE' WHERE jurisdiction_rule_id=$1`, s.ruleID)
	require.NoError(t, err)
	code, r = e.do("PUT", "/v1/admin/rules/"+s.ruleID+"/parameters", "rule-author", map[string]any{"parameters": nil})
	mustStatus(t, 409, code, r)
	assert.Equal(t, "not_draft", r["error"])
	_, err = pool.Exec(ctx, `UPDATE jurisdiction_rules SET rule_parameters='{"family":"TAX_RATE"}'::jsonb WHERE jurisdiction_rule_id=$1`, s.ruleID)
	require.Error(t, err, "an ACTIVE rule's rate cannot be edited, not even directly")
	_, err = pool.Exec(ctx, `UPDATE jurisdiction_rules SET rule_parameters=NULL WHERE jurisdiction_rule_id=$1`, s.ruleID)
	require.Error(t, err)
}

func TestCompile_RefusesInvalidParametersEvenWhenWrittenStraightIntoSQL(t *testing.T) {
	s := newTaxScenario(t)
	// Bypass the API validation entirely: the compiler is the second line of defence.
	_, err := pool.Exec(ctx, `UPDATE jurisdiction_rules SET rule_parameters='{"family":"TAX_RATE","rate":0.2,"rounding":{"mode":"HALF_UP","scale":2}}'::jsonb WHERE jurisdiction_rule_id=$1`, s.ruleID)
	require.NoError(t, err)
	s.packRef = uniqueRef("jur.t")
	code, r := s.e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRef, "pack_name": "Tax", "owner": "T"})
	mustStatus(t, 201, code, r)
	code, r = s.e.do("POST", "/v1/admin/packs/"+s.packRef+"/versions", "author", manifest(s.packRef, "2026.08.1", s.jur.JurisdictionCode, map[string]any{"rule_modules": []string{s.ruleID}}))
	mustStatus(t, 201, code, r)
	s.e.do("POST", s.path("/submit-review"), "author", nil)
	code, r = s.e.do("POST", s.path("/compile"), "compiler", nil)
	mustStatus(t, 422, code, r)
	assert.Contains(t, mustString(r["report"]), "JUR-C090")
}

func TestTaxCalculation_FromAReleasedPack_IsExactExplainedAndRecorded(t *testing.T) {
	s := newTaxScenario(t)
	s.setParams(t, taxParams, 200)
	s.toReviewAndCompile(t)

	// Coverage is mandatory: rule cases alone do not certify a rule that carries parameters.
	code, r := s.e.do("POST", s.path("/test-bundle"), "tester", s.bundle(t, false))
	mustStatus(t, 201, code, r)
	code, r = s.e.do("POST", s.path("/sign"), "signer", nil)
	mustStatus(t, 200, code, r)
	code, r = s.e.do("POST", s.path("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	assert.Equal(t, false, r["passed"])
	gaps := mustString(r["result"].(map[string]any)["coverage_gaps"])
	for _, c := range []string{"JUR-T020", "JUR-T021", "JUR-T022", "JUR-T023"} {
		assert.Contains(t, gaps, c)
	}
	s.certifyAndRelease(t)

	// 1000 x 10% + 500 x 20% = 200.00 exactly.
	r = s.execute(t, nil, 200)
	c := calcBody(r)
	require.Equal(t, "CALCULATED", c["outcome"])
	tax := c["tax"].(map[string]any)
	assert.Equal(t, "200.00", tax["tax_amount"])
	assert.Equal(t, "TAX_BANDS", tax["family"])
	assert.Len(t, tax["steps"], 3)
	res := c["resolution"].(map[string]any)
	assert.Equal(t, "RESOLVED", res["outcome"])
	assert.Equal(t, s.packRef, res["pack"].(map[string]any)["pack_ref"])
	assert.Equal(t, s.ruleID, res["rule"].(map[string]any)["rule_id"])
	assert.NotEmpty(t, res["sources"])
	id := r["decision_id"].(string)

	// The classic traps stay exact, and rounding is applied once.
	r = s.execute(t, map[string]any{"taxable_amount": "1000.01"}, 200)
	assert.Equal(t, "100.00", calcBody(r)["tax"].(map[string]any)["tax_amount"])
	assert.Equal(t, true, calcBody(r)["tax"].(map[string]any)["rounded"])
	assert.Equal(t, "100.002", calcBody(r)["tax"].(map[string]any)["unrounded_tax"])
	r = s.execute(t, map[string]any{"taxable_amount": "1234567890123.45"}, 200)
	// 1234567890123.45: 1000 x 10% = 100, plus 1234567889123.45 x 20% = 246913577824.69, exactly.
	assert.Equal(t, "246913577924.69", calcBody(r)["tax"].(map[string]any)["tax_amount"])

	// Evidence: inputs, steps, result, rule and pack versions, immutably (s12).
	var kind, outcome, artDigest string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, outcome, artifact_digest FROM rule_decision_evidence WHERE decision_id=$1`, id).Scan(&kind, &outcome, &artDigest))
	assert.Equal(t, "CALCULATION", kind)
	assert.Equal(t, "CALCULATED", outcome)
	assert.Equal(t, res["pack"].(map[string]any)["artifact_digest"], artDigest)
	code, got := s.e.do("GET", "/v1/rule-decisions/"+id, "auditor", nil)
	mustStatus(t, 200, code, got)
	_, err := pool.Exec(ctx, `UPDATE rule_decision_evidence SET outcome='NO_RULE' WHERE decision_id=$1`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO rule_decision_evidence (requested_by, request_digest, request, effective_at, outcome, response, decision_kind)
		VALUES ('x','sha256:`+strings.Repeat("0", 64)+`','{}'::jsonb,NOW(),'CALCULATED','{}'::jsonb,'CALCULATION')`)
	require.Error(t, err, "a calculated tax cannot be stored without its pack and rule basis")

	// Explicit refusals, all recorded.
	r = s.execute(t, map[string]any{"taxable_amount": "-5"}, 422)
	assert.Equal(t, "INVALID_FACTS", calcBody(r)["outcome"])
	r = s.execute(t, map[string]any{"taxable_amount": "1e3"}, 422)
	assert.Equal(t, "INVALID_FACTS", calcBody(r)["outcome"], "exponent notation is refused: no float parsing anywhere")
	r = s.execute(t, map[string]any{"jurisdiction": "ATLANTIS"}, 422)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", calcBody(r)["outcome"])
	r = s.execute(t, map[string]any{"effective_at": time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)}, 200)
	assert.Equal(t, "NO_RULE", calcBody(r)["outcome"], "before the rule is effective nothing is invented")

	// Request hygiene.
	code, _ = s.e.do("POST", "/v1/regulatory-calculations:execute", "tax-engine", map[string]any{"jurisdiction": "GB"})
	assert.Equal(t, 400, code)
	bad := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "rule_domain": "TAX", "rule_code": "STD", "effective_at": time.Now().UTC().Format(time.RFC3339Nano), "taxable_amount": "1", "currency": "gbp"}
	code, _ = s.e.do("POST", "/v1/regulatory-calculations:execute", "tax-engine", bad)
	assert.Equal(t, 400, code, "currency must be a three-letter upper-case code")
	code, _ = s.e.do("POST", "/v1/regulatory-calculations:execute", "", bad)
	assert.Equal(t, 401, code)
	s.e.auth.deny = "regulatory_calculation.execute"
	bad["currency"] = "GBP"
	code, _ = s.e.do("POST", "/v1/regulatory-calculations:execute", "tax-engine", bad)
	assert.Equal(t, 403, code)
	s.e.auth.deny = ""

	// Idempotency binds a key to one question.
	do := func(key, amount string) (int, map[string]any) {
		body := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "rule_domain": "TAX", "rule_code": "STD",
			"effective_at": "2026-10-01T12:00:00Z", "taxable_amount": amount, "currency": "GBP"}
		return s.e.doWithHeaders("POST", "/v1/regulatory-calculations:execute", "tax-engine", body, map[string]string{"Idempotency-Key": key})
	}
	code, a := do("calc-1", "500")
	mustStatus(t, 200, code, a)
	code, b := do("calc-1", "500")
	mustStatus(t, 200, code, b)
	assert.Equal(t, a["decision_id"], b["decision_id"])
	assert.Equal(t, true, b["replayed"])
	code, c2 := do("calc-1", "501")
	mustStatus(t, 409, code, c2)
}

func TestTaxCalculation_AQuietlyAlteredRateCannotBeServed(t *testing.T) {
	s := newTaxScenario(t)
	s.setParams(t, taxParams, 200)
	s.toReviewAndCompile(t)
	s.certifyAndRelease(t)
	r := s.execute(t, nil, 200)
	require.Equal(t, "200.00", calcBody(r)["tax"].(map[string]any)["tax_amount"])

	// An attacker (or a bad migration) changes the 20% band to 2% in the stored artifact, with the guards off.
	withoutGuards(t, "pack_artifacts", func() {
		_, err := pool.Exec(ctx, `UPDATE pack_artifacts a SET artifact = replace(artifact, '"rate":"0.2"', '"rate":"0.02"') FROM jurisdiction_pack_versions v
			JOIN jurisdiction_packs p USING (pack_id) WHERE a.pack_version_id=v.pack_version_id AND p.pack_ref=$1`, s.packRef)
		require.NoError(t, err)
		var changed int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM pack_artifacts a JOIN jurisdiction_pack_versions v USING (pack_version_id)
			JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 AND a.artifact LIKE '%"rate":"0.02"%'`, s.packRef).Scan(&changed))
		require.Equal(t, 1, changed, "the tamper really changed the stored rate")
		r := s.execute(t, nil, 503)
		assert.Equal(t, "pack_unverified", r["error"], "the wrong tax is never calculated: there is no answer instead")
		assert.Contains(t, mustString(r["failures"]), domain.ReasonDigestMismatch)
		_, err = pool.Exec(ctx, `UPDATE pack_artifacts a SET artifact = replace(artifact, '"rate":"0.02"', '"rate":"0.2"') FROM jurisdiction_pack_versions v
			JOIN jurisdiction_packs p USING (pack_id) WHERE a.pack_version_id=v.pack_version_id AND p.pack_ref=$1`, s.packRef)
		require.NoError(t, err)
	})
	r = s.execute(t, nil, 200)
	assert.Equal(t, "200.00", calcBody(r)["tax"].(map[string]any)["tax_amount"])
}
