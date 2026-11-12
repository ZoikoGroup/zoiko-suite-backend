//go:build integration

// s26 rule lifecycle events: versioned on create; effective and expired when a
// rule in force crosses the start or end of its interval (migration 000035).
package registryit_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

func outboxEventsFor(t *testing.T, eventType, aggregateID string) []map[string]any {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT event_payload FROM event_outbox WHERE event_type=$1 AND partition_key=$2 ORDER BY created_at`, eventType, aggregateID)
	require.NoError(t, err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		require.NoError(t, rows.Scan(&raw))
		var env map[string]any
		require.NoError(t, json.Unmarshal(raw, &env))
		out = append(out, env)
	}
	return out
}

func resetEffectivityWatermark(t *testing.T) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO rule_effectivity_watch (watch_id, scanned_through) VALUES (1, NOW() - interval '1 minute')
		ON CONFLICT (watch_id) DO UPDATE SET scanned_through = EXCLUDED.scanned_through`)
	require.NoError(t, err)
}

func activeRule(t *testing.T, jurID, code string, from time.Time) *domain.JurisdictionRule {
	t.Helper()
	rule, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: jurID, RuleDomain: "TAX", RuleCode: code,
		RuleName: code, EffectiveFrom: from, RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "author"})
	require.NoError(t, err)
	_, _, err = st.TransitionRuleStatus(ctx, store.TransitionParams{RuleID: rule.JurisdictionRuleID, NewStatus: "ACTIVE",
		AllowedPriors: []string{"DRAFT"}, ActorID: "approver"})
	require.NoError(t, err)
	return rule
}

func TestS26_RuleVersionedOnCreate(t *testing.T) {
	e := outboxEnv()
	j := seedJurisdiction(t, "S26V")
	code, rule := e.do("POST", "/v1/admin/jurisdictions/"+j.JurisdictionID+"/rules", "author", map[string]any{
		"rule_domain": "TAX", "rule_code": "S26V", "rule_name": "versioned", "effective_from": time.Now().UTC().Add(48 * time.Hour)})
	mustStatus(t, 201, code, rule)
	id := rule["jurisdiction_rule_id"].(string)
	evs := outboxEventsFor(t, "regulatory-rule.versioned", id)
	require.Len(t, evs, 1)
	require.Equal(t, float64(1), evs[0]["aggregate_version"])
	require.Equal(t, "S26V", evs[0]["payload"].(map[string]any)["rule_code"])
}

func TestS26_EffectiveAndExpiredAreAnnouncedOnceWhenTheInstantPasses(t *testing.T) {
	pub := events.NewOutboxPublisher(zap.NewNop(), st, "zoiko.jurisdiction.events.test")
	w := events.NewEffectivityWatcher(zap.NewNop(), st, pub, time.Minute)
	resetEffectivityWatermark(t)

	j := seedJurisdiction(t, "S26E")
	rule := activeRule(t, j.JurisdictionID, "S26E", time.Now().UTC().Add(-20*time.Second))

	n, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)
	evs := outboxEventsFor(t, "regulatory-rule.effective", rule.JurisdictionRuleID)
	require.Len(t, evs, 1, "the start of the interval is announced")
	require.Equal(t, "ACTIVE", evs[0]["payload"].(map[string]any)["rule_status"])

	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	require.Len(t, outboxEventsFor(t, "regulatory-rule.effective", rule.JurisdictionRuleID), 1, "announced once")

	// Superseded now (its end is now): the end of the interval is announced.
	_, _, err = st.TransitionRuleStatus(ctx, store.TransitionParams{RuleID: rule.JurisdictionRuleID, NewStatus: "SUPERSEDED",
		AllowedPriors: []string{"ACTIVE"}, EndDate: true, ActorID: "approver"})
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	require.Len(t, outboxEventsFor(t, "regulatory-rule.expired", rule.JurisdictionRuleID), 1, "the end of the interval is announced")

	// A DRAFT rule reaching its start is not in force: nothing is announced.
	draft, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: j.JurisdictionID, RuleDomain: "TAX", RuleCode: "S26D",
		RuleName: "draft", EffectiveFrom: time.Now().UTC(), RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "author"})
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	require.Empty(t, outboxEventsFor(t, "regulatory-rule.effective", draft.JurisdictionRuleID))
}

// Two replicas ticking together announce each crossing once (the watermark row is locked).
func TestS26_TwoWatchersAnnounceOnce(t *testing.T) {
	pub := events.NewOutboxPublisher(zap.NewNop(), st, "zoiko.jurisdiction.events.test")
	resetEffectivityWatermark(t)
	j := seedJurisdiction(t, "S26T")
	rule := activeRule(t, j.JurisdictionID, "S26T", time.Now().UTC().Add(-5*time.Second))
	w1 := events.NewEffectivityWatcher(zap.NewNop(), st, pub, time.Minute)
	w2 := events.NewEffectivityWatcher(zap.NewNop(), st, pub, time.Minute)
	done := make(chan error, 2)
	for _, w := range []*events.EffectivityWatcher{w1, w2} {
		go func(w *events.EffectivityWatcher) { _, err := w.Tick(context.Background()); done <- err }(w)
	}
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	require.Len(t, outboxEventsFor(t, "regulatory-rule.effective", rule.JurisdictionRuleID), 1)
}
