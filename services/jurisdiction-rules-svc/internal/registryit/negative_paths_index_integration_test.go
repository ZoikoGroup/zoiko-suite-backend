//go:build integration

// Specification negative paths proved end to end against a real Postgres (rule
// R10: every one has a test named after its id). The tests that prove the other
// ids carry their id in their own name (TestJURNEGnn_..., TestREF10NPnn_...,
// TestPDC02NPnn_...), so each runs once; this file holds JUR-NEG-16, which had
// no test. The full index with the status of every id is in progress.md section 4.
package registryit_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// JUR-NEG-16: a subdivision rule that collides with its country's rule (same rule, overlapping
// period, in another released pack) is refused at release; the reason names both jurisdictions.
func TestJURNEG16_SubdivisionRuleCollidingWithItsCountryIsRefused(t *testing.T) {
	s := newRulesScenario(t, []payrollSpec{{"PERSONAL_ALLOWANCE", pyStart2025, nil, pyAllowance25, ""}})
	national := s.rules["PERSONAL_ALLOWANCE@2025-04-06"]
	_, code, r := s.releaseOnly(t, []string{national})
	mustStatus(t, 200, code, r)

	// A subdivision of the same country, with its own version of the rule over an overlapping period.
	parent := s.jur.JurisdictionID
	sub, _, err := st.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionCode: s.jur.JurisdictionCode + "-SUB", JurisdictionName: "Subdivision", JurisdictionType: "STATE_PROVINCE",
		ParentJurisdictionID: &parent, AuthorityType: "STATE", EffectiveFrom: pyStart2025.AddDate(-1, 0, 0), ActiveFlag: true, CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	rule, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: sub.JurisdictionID, RuleDomain: "PAYROLL", RuleCode: "PERSONAL_ALLOWANCE",
		RuleName: "subdivision allowance", EffectiveFrom: pyStart2026, RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	code, r = s.e.do("PUT", "/v1/admin/rules/"+rule.JurisdictionRuleID+"/provenance", "author", map[string]any{
		"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	var p any
	require.NoError(t, json.Unmarshal([]byte(pyAllowance26), &p))
	code, r = s.e.do("PUT", "/v1/admin/rules/"+rule.JurisdictionRuleID+"/parameters", "rule-author", map[string]any{"parameters": p})
	mustStatus(t, 200, code, r)

	refSub, code, r := s.releaseOnlyIn(t, []string{rule.JurisdictionRuleID}, sub.JurisdictionCode)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "operation_blocked", r["code"])
	reasons := blockReasons(r)
	assert.Contains(t, reasons, "cross_pack_conflict")
	assert.Contains(t, reasons, "PAYROLL/PERSONAL_ALLOWANCE")
	assert.True(t, strings.Contains(reasons, s.jur.JurisdictionCode), "the country is named: %s", reasons)
	assert.Equal(t, "CERTIFIED", versionStatus(t, refSub, "2026.08.1"), "certification does not release it")
}
