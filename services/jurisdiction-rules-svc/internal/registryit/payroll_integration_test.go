//go:build integration

// ZS-JUR-001 Wave 6 (payroll) end to end: statutory parameters (allowances,
// withholding bands, employee and employer contributions) are authored as
// PAYROLL rules with typed parameters, packaged, tested, certified, released,
// and handed to the payroll product through the verified parameter interface.
// Payroll CALCULATION is not done here.
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

type payrollSpec struct {
	code   string
	from   time.Time
	to     *time.Time
	params string
	domain string // empty means PAYROLL
}

type payrollScenario struct {
	*fixture
	e       *env
	packRef string
	rules   map[string]string // "code@from" -> rule id
}

var (
	pyStart2025 = time.Date(2025, 4, 6, 0, 0, 0, 0, time.UTC)
	pyStart2026 = time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
)

const (
	pyAllowance25 = `{"family":"AMOUNT","class":"ALLOWANCE_OR_THRESHOLD","amount":"12570","unit":"PER_YEAR"}`
	pyAllowance26 = `{"family":"AMOUNT","class":"ALLOWANCE_OR_THRESHOLD","amount":"12800","unit":"PER_YEAR"}`
	pyIncomeTax   = `{"family":"TAX_BANDS","class":"INCOME_WITHHOLDING","bands":[{"up_to":"37700","rate":"0.2"},{"up_to":"125140","rate":"0.4"},{"up_to":null,"rate":"0.45"}],"rounding":{"mode":"HALF_UP","scale":2}}`
	pyEmployeeNI  = `{"family":"TAX_BANDS","class":"EMPLOYEE_CONTRIBUTION","bands":[{"up_to":"1048","rate":"0"},{"up_to":"4189","rate":"0.08"},{"up_to":null,"rate":"0.02"}],"rounding":{"mode":"HALF_UP","scale":2}}`
	pyEmployerNI  = `{"family":"TAX_RATE","class":"EMPLOYER_CONTRIBUTION","rate":"0.138","rounding":{"mode":"HALF_UP","scale":2}}`
)

func newPayrollScenario(t *testing.T) *payrollScenario {
	return newRulesScenario(t, []payrollSpec{
		{"PERSONAL_ALLOWANCE", pyStart2025, &pyStart2026, pyAllowance25, ""},
		{"PERSONAL_ALLOWANCE", pyStart2026, nil, pyAllowance26, ""},
		{"INCOME_TAX_BANDS", pyStart2025, nil, pyIncomeTax, ""},
		{"EMPLOYEE_NI", pyStart2025, nil, pyEmployeeNI, ""},
		{"EMPLOYER_NI", pyStart2025, nil, pyEmployerNI, ""},
	})
}

func newRulesScenario(t *testing.T, specs []payrollSpec) *payrollScenario {
	t.Helper()
	keyRef := "pay-key-" + strings.ToLower(uuid.NewString()[:6])
	signer, pub := newSigner(t, keyRef)
	e := newEnvResolver(signer, []string{"RELEASED"}, 0)
	f := newFixture(t, e)
	code, r := e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": pub})
	mustStatus(t, 201, code, r)
	s := &payrollScenario{fixture: f, e: e, rules: map[string]string{}}

	for _, sp := range specs {
		dom := sp.domain
		if dom == "" {
			dom = "PAYROLL"
		}
		rule, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: f.jur.JurisdictionID, RuleDomain: dom, RuleCode: sp.code,
			RuleName: sp.code, EffectiveFrom: sp.from, EffectiveTo: sp.to, RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
		require.NoError(t, err)
		code, r := e.do("PUT", "/v1/admin/rules/"+rule.JurisdictionRuleID+"/provenance", "author", map[string]any{
			"regime_id": f.regimeID, "interpretation_id": f.interpID, "source_ids": []string{f.sourceID}})
		mustStatus(t, 200, code, r)
		var p any
		require.NoError(t, json.Unmarshal([]byte(sp.params), &p))
		code, r = e.do("PUT", "/v1/admin/rules/"+rule.JurisdictionRuleID+"/parameters", "rule-author", map[string]any{"parameters": p})
		mustStatus(t, 200, code, r)
		s.rules[sp.code+"@"+sp.from.Format("2006-01-02")] = rule.JurisdictionRuleID
	}
	return s
}

func (s *payrollScenario) path(suffix string) string {
	return "/v1/admin/packs/" + s.packRef + "/versions/2026.08.1" + suffix
}

// autoBundle derives complete rule, calculation and fixed-amount coverage from the artifact.
func (s *payrollScenario) autoBundle(t *testing.T) map[string]any {
	t.Helper()
	code, got := s.e.do("GET", "/v1/packs/"+s.packRef+"/versions/2026.08.1/artifact", "", nil)
	mustStatus(t, 200, code, got)
	raw, err := json.Marshal(got["artifact"].(map[string]any)["artifact"])
	require.NoError(t, err)
	doc, err := domain.ParseArtifact(raw)
	require.NoError(t, err)
	domOf := map[string]string{}
	for _, ru := range doc.Rules {
		domOf[ru.RuleCode] = ru.RuleDomain
	}
	codeByID := map[string]string{}
	for _, j := range doc.Jurisdictions {
		codeByID[j.ID] = j.Code
	}
	rfc := func(tt time.Time) string { return tt.UTC().Format(time.RFC3339Nano) }
	var cases []map[string]any
	n := 0
	add := func(class string, in map[string]any, expect map[string]any) {
		n++
		cases = append(cases, map[string]any{"id": "c" + strings.Repeat("x", n/26) + string(rune('a'+n%26)), "class": class, "input": in, "expect": expect})
	}
	ruleIn := func(jc, rcode string, at time.Time) map[string]any {
		return map[string]any{"jurisdiction": jc, "rule_domain": domOf[rcode], "rule_code": rcode, "effective_at": rfc(at)}
	}
	expectFor := func(jc, rcode string, at time.Time) map[string]any {
		r := doc.Resolve(jc, domOf[rcode], rcode, at)
		if r.Outcome == domain.OutcomeResolved {
			return map[string]any{"outcome": r.Outcome, "rule_id": r.RuleID}
		}
		return map[string]any{"outcome": r.Outcome}
	}
	for _, r := range doc.Rules {
		jc := codeByID[r.JurisdictionID]
		inside := r.EffectiveFrom.Add(time.Hour)
		g := expectFor(jc, r.RuleCode, inside)
		if amt := doc.ParameterAmountForTest(r.RuleID); amt != "" {
			g["parameter_amount"] = amt
		}
		add("GOLDEN", ruleIn(jc, r.RuleCode, inside), g)
		bt := []time.Time{r.EffectiveFrom, r.EffectiveFrom.Add(-time.Nanosecond)}
		if r.EffectiveTo != nil {
			bt = append(bt, *r.EffectiveTo, r.EffectiveTo.Add(-time.Nanosecond))
		}
		for _, tt := range bt {
			add("BOUNDARY", ruleIn(jc, r.RuleCode, tt), expectFor(jc, r.RuleCode, tt))
		}
		if len(r.Parameters) == 0 {
			continue
		}
		p, perr := domain.ParseRuleParameters(r.Parameters)
		require.NoError(t, perr)
		if domain.ParameterOnlyFamily(p.Family) {
			continue
		}
		calc := func(class, amount string) {
			in := ruleIn(jc, r.RuleCode, inside)
			in["taxable_amount"] = amount
			res := domain.CalculateTax(p, amount)
			e := map[string]any{"outcome": res.Outcome, "rule_id": r.RuleID}
			if res.Outcome == domain.CalcCalculated {
				e["tax_amount"] = res.TaxAmount
			}
			add(class, in, e)
		}
		calc("GOLDEN", "5000")
		for _, b := range p.Bands {
			if b.UpTo != nil {
				calc("BOUNDARY", *b.UpTo)
			}
		}
		for _, cand := range []string{"0.01", "1000.01", "1234.57"} {
			if domain.CalculateTax(p, cand).Rounded {
				calc("BOUNDARY", cand)
				break
			}
		}
		calc("NEGATIVE", "-5")
	}
	for code := range domOf {
		add("NEGATIVE", ruleIn("ATLANTIS", code, pyStart2026.Add(time.Hour)), map[string]any{"outcome": domain.OutcomeUnsupported})
		break
	}
	return map[string]any{"bundle_version": "1", "cases": cases}
}

func (s *payrollScenario) release(t *testing.T) {
	t.Helper()
	e := s.e
	s.packRef = uniqueRef("jur.p")
	code, r := e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRef, "pack_name": "Payroll statutory", "owner": "Payroll Compliance"})
	mustStatus(t, 201, code, r)
	var ids []string
	for _, id := range s.rules {
		ids = append(ids, id)
	}
	code, r = e.do("POST", "/v1/admin/packs/"+s.packRef+"/versions", "author", manifest(s.packRef, "2026.08.1", s.jur.JurisdictionCode, map[string]any{"rule_modules": ids, "effective_from": "2025-01-01"}))
	mustStatus(t, 201, code, r)
	for _, step := range []string{"/submit-review", "/compile", "/sign"} {
		code, r = e.do("POST", s.path(step), "author", nil)
		require.Contains(t, []int{200, 201}, code, "%s: %v", step, r)
	}
	code, r = e.do("POST", s.path("/test-bundle"), "tester", s.autoBundle(t))
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", s.path("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	require.Equal(t, true, r["passed"], "%v", r["result"])
	code, r = e.do("POST", s.path("/reviews"), "reviewer-pay", map[string]any{"role": "PAYROLL", "decision": "APPROVE"})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", s.path("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", s.path("/publish"), "releaser", nil)
	mustStatus(t, 200, code, r)
}

func (s *payrollScenario) params(t *testing.T, body map[string]any, want int) map[string]any {
	t.Helper()
	base := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "effective_at": "2026-09-01T00:00:00Z"}
	for k, v := range body {
		base[k] = v
	}
	code, r := s.e.do("POST", "/v1/payroll-statutory-parameters:resolve", "payroll-engine", base)
	require.Equal(t, want, code, "%v", r)
	return r
}

func setOf(r map[string]any) map[string]any { c, _ := r["parameter_set"].(map[string]any); return c }

func itemsByCode(t *testing.T, r map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, it := range setOf(r)["items"].([]any) {
		m := it.(map[string]any)
		out[m["rule_code"].(string)] = m
	}
	return out
}

func TestPayrollParameters_AreHandedToPayrollAsAVerifiedSourcedSet(t *testing.T) {
	s := newPayrollScenario(t)
	s.release(t)

	r := s.params(t, nil, 200)
	set := setOf(r)
	require.Equal(t, "PARAMETERS_RESOLVED", set["outcome"])
	items := itemsByCode(t, r)
	require.Len(t, items, 4)
	assert.Equal(t, "12800", items["PERSONAL_ALLOWANCE"]["parameters"].(map[string]any)["amount"], "the 2026 allowance applies on a September 2026 pay date")
	assert.Equal(t, "INCOME_WITHHOLDING", items["INCOME_TAX_BANDS"]["class"])
	assert.Equal(t, "EMPLOYEE_CONTRIBUTION", items["EMPLOYEE_NI"]["class"])
	assert.Equal(t, "EMPLOYER_CONTRIBUTION", items["EMPLOYER_NI"]["class"])
	assert.Equal(t, "0.138", items["EMPLOYER_NI"]["parameters"].(map[string]any)["rate"])
	for code, it := range items {
		assert.NotEmpty(t, it["sources"], code)
		assert.Equal(t, s.packRef, it["pack"].(map[string]any)["pack_ref"], code)
		assert.Regexp(t, `^sha256:`, it["content_digest"], code)
	}
	// Ordered by class then code, so two identical requests render identically.
	classes := []string{}
	for _, it := range set["items"].([]any) {
		classes = append(classes, it.(map[string]any)["class"].(string))
	}
	assert.Equal(t, []string{"ALLOWANCE_OR_THRESHOLD", "EMPLOYEE_CONTRIBUTION", "EMPLOYER_CONTRIBUTION", "INCOME_WITHHOLDING"}, classes)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, set["bundle_digest"])

	// Evidence: what was handed over, anchored on the whole-set digest and the single pack release.
	id := r["decision_id"].(string)
	var kind, outcome, ruleDigest string
	var artDigest *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, outcome, rule_content_digest, artifact_digest FROM rule_decision_evidence WHERE decision_id=$1`, id).
		Scan(&kind, &outcome, &ruleDigest, &artDigest))
	assert.Equal(t, "PARAMETER_SET", kind)
	assert.Equal(t, "PARAMETERS_RESOLVED", outcome)
	assert.Equal(t, set["bundle_digest"], ruleDigest)
	require.NotNil(t, artDigest, "a set from one pack release is anchored on that release too")
	code, got := s.e.do("GET", "/v1/rule-decisions/"+id, "auditor", nil)
	mustStatus(t, 200, code, got)
	_, err := pool.Exec(ctx, `INSERT INTO rule_decision_evidence (requested_by, request_digest, request, effective_at, outcome, response, decision_kind)
		VALUES ('x','sha256:`+strings.Repeat("0", 64)+`','{}'::jsonb,NOW(),'PARAMETERS_RESOLVED','{}'::jsonb,'PARAMETER_SET')`)
	require.Error(t, err, "a resolved parameter set cannot be stored without its set digest")

	// Reproducible by pay date: a 2025 pay date gets the 2025 allowance.
	r = s.params(t, map[string]any{"effective_at": "2025-09-01T00:00:00Z"}, 200)
	assert.Equal(t, "12570", itemsByCode(t, r)["PERSONAL_ALLOWANCE"]["parameters"].(map[string]any)["amount"])
	// The boundary instant itself belongs to the NEW tax year.
	r = s.params(t, map[string]any{"effective_at": "2026-04-06T00:00:00Z"}, 200)
	assert.Equal(t, "12800", itemsByCode(t, r)["PERSONAL_ALLOWANCE"]["parameters"].(map[string]any)["amount"])
	r = s.params(t, map[string]any{"effective_at": "2026-04-05T23:59:59.999999999Z"}, 200)
	assert.Equal(t, "12570", itemsByCode(t, r)["PERSONAL_ALLOWANCE"]["parameters"].(map[string]any)["amount"])

	// Class filter; unknown class is a client error.
	r = s.params(t, map[string]any{"classes": []string{"EMPLOYER_CONTRIBUTION"}}, 200)
	require.Len(t, itemsByCode(t, r), 1)
	s.params(t, map[string]any{"classes": []string{"BONUS"}}, 400)

	// Explicit non-answers.
	r = s.params(t, map[string]any{"effective_at": "2024-01-01T00:00:00Z"}, 200)
	assert.Equal(t, "NO_RULE", setOf(r)["outcome"], "before the first tax year in the pack: nothing is invented")
	r = s.params(t, map[string]any{"jurisdiction": "ATLANTIS"}, 422)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", setOf(r)["outcome"])

	// Pinned replay returns the same set.
	r = s.params(t, map[string]any{"pack_ref": s.packRef, "pack_version": "2026.08.1"}, 200)
	assert.Equal(t, set["bundle_digest"], setOf(r)["bundle_digest"])

	// Hygiene and idempotency.
	code, _ = s.e.do("POST", "/v1/payroll-statutory-parameters:resolve", "payroll-engine", map[string]any{"jurisdiction": "GB"})
	assert.Equal(t, 400, code)
	code, _ = s.e.do("POST", "/v1/payroll-statutory-parameters:resolve", "", map[string]any{})
	assert.Equal(t, 401, code)
	s.e.auth.deny = "payroll_parameters.resolve"
	code, _ = s.e.do("POST", "/v1/payroll-statutory-parameters:resolve", "payroll-engine", map[string]any{"jurisdiction": "x", "effective_at": "2026-01-01T00:00:00Z"})
	assert.Equal(t, 403, code)
	s.e.auth.deny = ""
	body := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "effective_at": "2026-09-01T00:00:00Z"}
	code, a := s.e.doWithHeaders("POST", "/v1/payroll-statutory-parameters:resolve", "payroll-engine", body, map[string]string{"Idempotency-Key": "pay-1"})
	mustStatus(t, 200, code, a)
	code, b := s.e.doWithHeaders("POST", "/v1/payroll-statutory-parameters:resolve", "payroll-engine", body, map[string]string{"Idempotency-Key": "pay-1"})
	mustStatus(t, 200, code, b)
	assert.Equal(t, a["decision_id"], b["decision_id"])
	body["effective_at"] = "2025-09-01T00:00:00Z"
	code, c2 := s.e.doWithHeaders("POST", "/v1/payroll-statutory-parameters:resolve", "payroll-engine", body, map[string]string{"Idempotency-Key": "pay-1"})
	mustStatus(t, 409, code, c2)
}

func TestPayrollParameters_AFixedAmountIsNotCalculatedWith(t *testing.T) {
	s := newPayrollScenario(t)
	s.release(t)
	code, r := s.e.do("POST", "/v1/regulatory-calculations:execute", "payroll-engine", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "rule_domain": "PAYROLL",
		"rule_code": "PERSONAL_ALLOWANCE", "effective_at": "2026-09-01T00:00:00Z", "taxable_amount": "50000", "currency": "GBP"})
	mustStatus(t, 422, code, r)
	assert.Equal(t, "PARAMETER_ONLY", calcBody(r)["outcome"], "an allowance is a parameter, not a calculation")
	// The rate rules still calculate with exact arithmetic: (37700 x 20%) + (12300 x 40%) = 12460.
	code, r = s.e.do("POST", "/v1/regulatory-calculations:execute", "payroll-engine", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "rule_domain": "PAYROLL",
		"rule_code": "INCOME_TAX_BANDS", "effective_at": "2026-09-01T00:00:00Z", "taxable_amount": "50000", "currency": "GBP"})
	mustStatus(t, 200, code, r)
	assert.Equal(t, "12460.00", calcBody(r)["tax"].(map[string]any)["tax_amount"])
}

func TestPayrollParameters_AClasslessPayrollRuleCannotBePackaged(t *testing.T) {
	s := newPayrollScenario(t)
	rule, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: s.jur.JurisdictionID, RuleDomain: "PAYROLL", RuleCode: "NO_CLASS",
		RuleName: "n", EffectiveFrom: pyStart2025, RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	code, r := s.e.do("PUT", "/v1/admin/rules/"+rule.JurisdictionRuleID+"/provenance", "author", map[string]any{
		"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = s.e.do("PUT", "/v1/admin/rules/"+rule.JurisdictionRuleID+"/parameters", "rule-author", map[string]any{
		"parameters": map[string]any{"family": "AMOUNT", "amount": "100", "unit": "PER_WEEK"}})
	mustStatus(t, 200, code, r) // valid in general: the class is only mandatory for payroll rules
	s.packRef = uniqueRef("jur.p")
	code, r = s.e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRef, "pack_name": "P", "owner": "P"})
	mustStatus(t, 201, code, r)
	code, r = s.e.do("POST", "/v1/admin/packs/"+s.packRef+"/versions", "author", manifest(s.packRef, "2026.08.1", s.jur.JurisdictionCode, map[string]any{"rule_modules": []string{rule.JurisdictionRuleID}, "effective_from": "2025-01-01"}))
	mustStatus(t, 201, code, r)
	s.e.do("POST", s.path("/submit-review"), "author", nil)
	code, r = s.e.do("POST", s.path("/compile"), "compiler", nil)
	mustStatus(t, 422, code, r)
	assert.Contains(t, mustString(r["report"]), "JUR-C091")
}
