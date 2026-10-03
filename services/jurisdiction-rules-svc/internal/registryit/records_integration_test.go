//go:build integration

// ZS-JUR-001 Wave 6 (retention s18, mappings s19) end to end: retention and
// accounting mapping rules are authored as typed-parameter rules, packaged,
// certified, released and answered from the verified artifact.
package registryit_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

var py2026Jan = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	retPayroll6 = `{"family":"RETENTION","record_class":"PAYROLL_RECORDS","trigger":"EMPLOYMENT_END","minimum":{"years":6,"months":0,"days":0},"format_requirement":"READABLE_ORIGINAL_OR_CERTIFIED_COPY","legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`
	retPayroll7 = `{"family":"RETENTION","record_class":"PAYROLL_RECORDS","trigger":"EMPLOYMENT_END","minimum":{"years":7,"months":0,"days":0},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`
	retFilings  = `{"family":"RETENTION","record_class":"TAX_FILINGS","trigger":"FILING_DATE","minimum":{"years":0,"months":6,"days":0},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`
	mapVAT      = `{"family":"MAPPING","mapping_type":"TAX_CODE_TO_ACCOUNT_CLASS","entries":[{"from":"VAT_STD","to":"OUTPUT_TAX_PAYABLE","note":"standard rated sales"},{"from":"VAT_IN","to":"INPUT_TAX_RECOVERABLE"},{"from":"VAT_EXEMPT","to":"EXEMPT_SALES"}]}`
)

func newRecordsScenario(t *testing.T) *payrollScenario {
	t.Helper()
	return newRulesScenario(t, []payrollSpec{
		{"PAYROLL_RECORDS", pyStart2025, &py2026Jan, retPayroll6, "RECORDS_RETENTION"},
		{"PAYROLL_RECORDS", py2026Jan, nil, retPayroll7, "RECORDS_RETENTION"},
		{"TAX_FILINGS", pyStart2025, nil, retFilings, "RECORDS_RETENTION"},
		{"VAT_TO_ACCOUNT", pyStart2025, nil, mapVAT, "ACCOUNTING_MAPPING"},
	})
}

func (s *payrollScenario) retention(t *testing.T, body map[string]any, want int) map[string]any {
	t.Helper()
	base := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "record_class": "PAYROLL_RECORDS", "trigger_date": "2026-03-10"}
	for k, v := range body {
		base[k] = v
	}
	code, r := s.e.do("POST", "/v1/retention-rules:resolve", "records-mgr", base)
	require.Equal(t, want, code, "%v", r)
	return r
}

func retOf(r map[string]any) map[string]any { m, _ := r["retention"].(map[string]any); return m }

func (s *payrollScenario) mapping(t *testing.T, body map[string]any, want int) map[string]any {
	t.Helper()
	base := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "mapping_code": "VAT_TO_ACCOUNT", "effective_at": "2026-08-01T00:00:00Z"}
	for k, v := range body {
		base[k] = v
	}
	code, r := s.e.do("POST", "/v1/accounting-mappings:resolve", "acct-engine", base)
	require.Equal(t, want, code, "%v", r)
	return r
}

func TestRetention_IsAnsweredFromTheVerifiedPackAndNeverShortenedByATenant(t *testing.T) {
	s := newRecordsScenario(t)
	s.release(t)

	r := s.retention(t, nil, 200)
	ret := retOf(r["retention"].(map[string]any))
	require.Equal(t, "RETENTION_RESOLVED", ret["outcome"])
	assert.Equal(t, "P7Y", ret["statutory_minimum"], "the 2026 rule applies to a 2026 trigger date")
	assert.Equal(t, "2033-03-10", ret["retain_until"])
	assert.Equal(t, true, ret["legal_hold_override"])
	assert.Equal(t, "ELIGIBILITY_ONLY", ret["destruction_rule"])
	id := r["decision_id"].(string)
	var kind, outcome string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, outcome FROM rule_decision_evidence WHERE decision_id=$1`, id).Scan(&kind, &outcome))
	assert.Equal(t, "RETENTION", kind)
	assert.Equal(t, "RETENTION_RESOLVED", outcome)

	// The rule in force on the trigger date decides: 2025 gets six years.
	r = s.retention(t, map[string]any{"trigger_date": "2025-06-30"}, 200)
	assert.Equal(t, "2031-06-30", retOf(r["retention"].(map[string]any))["retain_until"])
	assert.Equal(t, "READABLE_ORIGINAL_OR_CERTIFIED_COPY", retOf(r["retention"].(map[string]any))["format_requirement"])

	// Month arithmetic clamps to the end of the month.
	r = s.retention(t, map[string]any{"record_class": "TAX_FILINGS", "trigger_date": "2026-08-31"}, 200)
	assert.Equal(t, "2027-02-28", retOf(r["retention"].(map[string]any))["retain_until"])

	// JUR-NEG-12: a tenant may extend but never shorten.
	r = s.retention(t, map[string]any{"tenant_minimum": map[string]any{"years": 5}}, 422)
	assert.Equal(t, "TENANT_POLICY_TOO_SHORT", retOf(r["retention"].(map[string]any))["outcome"])
	r = s.retention(t, map[string]any{"tenant_minimum": map[string]any{"years": 10}}, 200)
	assert.Equal(t, "2036-03-10", retOf(r["retention"].(map[string]any))["effective_retain_until"])
	assert.Equal(t, "2033-03-10", retOf(r["retention"].(map[string]any))["retain_until"], "the statutory date is still reported")

	// Refusals and non-answers.
	r = s.retention(t, map[string]any{"trigger": "CREATION"}, 422)
	assert.Equal(t, "TRIGGER_MISMATCH", retOf(r["retention"].(map[string]any))["outcome"])
	r = s.retention(t, map[string]any{"record_class": "UNKNOWN_CLASS"}, 200)
	assert.Equal(t, "NO_RULE", r["retention"].(map[string]any)["outcome"])
	r = s.retention(t, map[string]any{"jurisdiction": "ATLANTIS"}, 422)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", r["retention"].(map[string]any)["outcome"])
	s.retention(t, map[string]any{"trigger_date": "10/03/2026"}, 400)
	s.retention(t, map[string]any{"trigger": "SOMETIME"}, 400)
	s.retention(t, map[string]any{"tenant_minimum": map[string]any{"years": 0}}, 400)

	// Idempotency.
	body := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "record_class": "PAYROLL_RECORDS", "trigger_date": "2026-03-10"}
	code, a := s.e.doWithHeaders("POST", "/v1/retention-rules:resolve", "records-mgr", body, map[string]string{"Idempotency-Key": "ret-1"})
	mustStatus(t, 200, code, a)
	code, b := s.e.doWithHeaders("POST", "/v1/retention-rules:resolve", "records-mgr", body, map[string]string{"Idempotency-Key": "ret-1"})
	mustStatus(t, 200, code, b)
	assert.Equal(t, a["decision_id"], b["decision_id"])
	body["trigger_date"] = "2026-03-11"
	code, c := s.e.doWithHeaders("POST", "/v1/retention-rules:resolve", "records-mgr", body, map[string]string{"Idempotency-Key": "ret-1"})
	mustStatus(t, 409, code, c)

	// A retention rule is a parameter, never a formula to calculate with.
	code, c = s.e.do("POST", "/v1/regulatory-calculations:execute", "payroll-engine", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "rule_domain": "RECORDS_RETENTION",
		"rule_code": "TAX_FILINGS", "effective_at": "2026-08-01T00:00:00Z", "taxable_amount": "1", "currency": "GBP"})
	mustStatus(t, 422, code, c)
	assert.Equal(t, "PARAMETER_ONLY", calcBody(c)["outcome"])
}

func TestMapping_ClassifiesWithoutGuessing(t *testing.T) {
	s := newRecordsScenario(t)
	s.release(t)

	r := s.mapping(t, map[string]any{"from": "VAT_STD"}, 200)
	m := r["mapping"].(map[string]any)["mapping"].(map[string]any)
	require.Equal(t, "MAPPING_RESOLVED", m["outcome"])
	assert.Equal(t, "OUTPUT_TAX_PAYABLE", m["to"])
	assert.Equal(t, "standard rated sales", m["note"])
	id := r["decision_id"].(string)
	var kind string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind FROM rule_decision_evidence WHERE decision_id=$1`, id).Scan(&kind))
	assert.Equal(t, "MAPPING", kind)

	r = s.mapping(t, nil, 200)
	assert.Len(t, r["mapping"].(map[string]any)["mapping"].(map[string]any)["entries"], 3)
	r = s.mapping(t, map[string]any{"from": "VAT_ZERO"}, 422)
	assert.Equal(t, "MAPPING_ENTRY_NOT_FOUND", r["mapping"].(map[string]any)["mapping"].(map[string]any)["outcome"], "an unmapped code is never guessed")
	r = s.mapping(t, map[string]any{"mapping_code": "NOPE"}, 200)
	assert.Equal(t, "NO_RULE", r["mapping"].(map[string]any)["outcome"])
	s.mapping(t, map[string]any{"effective_at": "yesterday"}, 400)
}

func TestRecordsParameters_AreValidatedAtTheEdgeAndByTheCompiler(t *testing.T) {
	s := newRecordsScenario(t)
	rid := s.rules["VAT_TO_ACCOUNT@2025-04-06"]
	require.NotEmpty(t, rid)
	// Draft rules are editable; invalid shapes are refused with 400.
	bad := []string{
		`{"family":"RETENTION","record_class":"X_Y","trigger":"CREATION","minimum":{"years":1,"months":0,"days":0},"legal_hold_override":false,"destruction_rule":"ELIGIBILITY_ONLY"}`,
		`{"family":"RETENTION","record_class":"X_Y","trigger":"CREATION","minimum":{"years":1,"months":0,"days":0},"legal_hold_override":true,"destruction_rule":"DELETE"}`,
		`{"family":"RETENTION","record_class":"X_Y","trigger":"CREATION","minimum":{"years":0,"months":0,"days":0},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`,
		`{"family":"MAPPING","mapping_type":"REPORTING_TAXONOMY","entries":[{"from":"A","to":"B"},{"from":"A","to":"C"}]}`,
		`{"family":"MAPPING","mapping_type":"WHATEVER","entries":[{"from":"A","to":"B"}]}`,
		`{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2},"trigger":"CREATION"}`,
	}
	for _, b := range bad {
		code, r := s.e.do("PUT", "/v1/admin/rules/"+rid+"/parameters", "rule-author", map[string]any{"parameters": rawMap(t, b)})
		assert.Equal(t, 400, code, "%s: %v", b, r)
	}

	// The compiler refuses a retention rule filed under the wrong domain (JUR-C092).
	s.packRef = uniqueRef("jur.p")
	code, r := s.e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRef, "pack_name": "P", "owner": "P"})
	mustStatus(t, 201, code, r)
	wrong, _, err := st.CreateRule(ctx, domainCreate(s, "PAYROLL", "WRONG_DOMAIN"))
	require.NoError(t, err)
	code, r = s.e.do("PUT", "/v1/admin/rules/"+wrong.JurisdictionRuleID+"/provenance", "author", map[string]any{
		"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = s.e.do("PUT", "/v1/admin/rules/"+wrong.JurisdictionRuleID+"/parameters", "rule-author", map[string]any{"parameters": rawMap(t, retFilings)})
	mustStatus(t, 200, code, r)
	code, r = s.e.do("POST", "/v1/admin/packs/"+s.packRef+"/versions", "author", manifest(s.packRef, "2026.08.1", s.jur.JurisdictionCode,
		map[string]any{"rule_modules": []string{wrong.JurisdictionRuleID}, "effective_from": "2025-01-01"}))
	mustStatus(t, 201, code, r)
	s.e.do("POST", s.path("/submit-review"), "author", nil)
	code, r = s.e.do("POST", s.path("/compile"), "compiler", nil)
	mustStatus(t, 422, code, r)
	assert.True(t, strings.Contains(mustString(r["report"]), "JUR-C092"), "%v", r["report"])
}

func rawMap(t *testing.T, s string) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(s), &v))
	return v
}

func domainCreate(s *payrollScenario, dom, code string) domain.CreateRuleParams {
	return domain.CreateRuleParams{JurisdictionID: s.jur.JurisdictionID, RuleDomain: dom, RuleCode: code, RuleName: code,
		EffectiveFrom: pyStart2025, RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"}
}
