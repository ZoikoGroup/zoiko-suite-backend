//go:build integration

// The console summary (JR-FE-1) counts live data, against a real Postgres.
package registryit_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

func summaryEnv(auth *recordingAuthz) *env {
	r := newRouter()
	handler.RegisterRoutes(r, handler.New(st, auth, &spyPub{}, "platform-scope", zap.NewNop()).WithRegistry(st, &spyPub{}).WithSummary(st))
	return &env{srv: r, auth: auth}
}

func summaryNumbers(t *testing.T, e *env) map[string]float64 {
	t.Helper()
	code, s := e.do("GET", "/v1/jurisdiction-summary", "console", nil)
	require.Equal(t, 200, code, "%v", s)
	out := map[string]float64{}
	for _, part := range []string{"kpis", "pack_pipeline", "attention"} {
		for k, v := range s[part].(map[string]any) {
			out[part+"."+k] = v.(float64)
		}
	}
	return out
}

func TestJRFE1_SummaryCountsLiveData(t *testing.T) {
	auth := &recordingAuthz{}
	e := summaryEnv(auth)
	before := summaryNumbers(t, e)
	require.True(t, auth.pairs["jurisdiction_summary.view"])

	j := seedJurisdiction(t, "SUM")
	seedRule(t, j.JurisdictionID, "SUMR") // stays DRAFT
	_, _, err := st.TransitionRuleStatus(ctx, store.TransitionParams{RuleID: seedRule(t, j.JurisdictionID, "SUMA").JurisdictionRuleID,
		NewStatus: "ACTIVE", AllowedPriors: []string{"DRAFT"}, ActorID: "approver"})
	require.NoError(t, err)
	active, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: j.JurisdictionID, RuleDomain: "TAX", RuleCode: "SUMD",
		RuleName: "drifting", EffectiveFrom: time.Now().UTC().Add(-time.Hour), RulePayload: []byte(`{}`), RuleStatus: "ACTIVE", CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	_, _, _, err = st.RecordDrift(ctx, domain.RecordDriftParams{JurisdictionRuleID: active.JurisdictionRuleID, ToState: "UNDER_REVIEW", RecordedByPrincipalID: "monitor"})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO source_change_notices (jurisdiction_id, authority, change_type, title, location, created_by_principal_id)
		VALUES ($1, 'HMRC', 'CLARIFICATION', 'summary probe', 'https://example.invalid/'||$2, 'intake')`, j.JurisdictionID, uuid.NewString())
	require.NoError(t, err)

	after := summaryNumbers(t, e)
	require.Equal(t, before["kpis.active_jurisdictions"]+1, after["kpis.active_jurisdictions"])
	require.Equal(t, before["kpis.rules_draft"]+1, after["kpis.rules_draft"], "one rule stays DRAFT")
	require.Equal(t, before["kpis.rules_under_review"]+1, after["kpis.rules_under_review"])
	require.Equal(t, before["attention.drifted_rules"]+1, after["attention.drifted_rules"])
	require.Equal(t, after["attention.drifted_rules"]+after["attention.open_remediation_cases"], after["kpis.open_drift_and_remediation"])
	require.Equal(t, before["attention.open_source_change_notices"]+1, after["attention.open_source_change_notices"])
	require.Equal(t, after["kpis.pack_versions_released"], after["pack_pipeline.released"])

	denied := summaryEnv(&recordingAuthz{deny: "jurisdiction_summary.view"})
	code, _ := denied.do("GET", "/v1/jurisdiction-summary", "intruder", nil)
	require.Equal(t, 403, code)
}
