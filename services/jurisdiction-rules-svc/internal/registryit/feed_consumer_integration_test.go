//go:build integration

// J-03 / J-04: a regulatory source change detected by the feed puts every ACTIVE
// rule citing it under review, against a real Postgres.
package registryit_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/consumer"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

func feedEvent(t *testing.T, id, ref string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"event_id": id, "event_type": "regulatory.source.changed", "correlation_id": "corr-" + id,
		"payload": map[string]any{"external_feed_reference": ref, "authority": "HMRC", "change_summary": "standard rate amended",
			"observed_at": time.Now().UTC(), "source_url": "https://example.invalid/notice"}})
	require.NoError(t, err)
	return b
}

func TestJ03_FeedChangePutsCitingActiveRulesUnderReviewOnce(t *testing.T) {
	ref := "FEED-" + uuid.NewString()[:8]
	j := seedJurisdiction(t, "FEED")
	mk := func(code, status string) *domain.JurisdictionRule {
		r, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: j.JurisdictionID, RuleDomain: "TAX", RuleCode: code, RuleName: code,
			EffectiveFrom: time.Now().UTC().Add(-time.Hour), RulePayload: []byte(`{}`), RuleStatus: status, ExternalFeedReference: &ref, CreatedByPrincipalID: "seed"})
		require.NoError(t, err)
		return r
	}
	active, draft := mk("FEEDA", "ACTIVE"), mk("FEEDD", "DRAFT")

	c := consumer.NewFeedConsumer(zap.NewNop(), st, events.NewOutboxPublisher(zap.NewNop(), st, "zoiko.jurisdiction.events.test"))
	id := "evt-" + uuid.NewString()
	res, err := c.Handle(ctx, feedEvent(t, id, ref))
	require.NoError(t, err)
	require.Equal(t, 1, res.RulesMarked, "only the ACTIVE rule citing the reference")

	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT legal_drift_state FROM jurisdiction_rules WHERE jurisdiction_rule_id=$1`, active.JurisdictionRuleID).Scan(&state))
	require.Equal(t, "UNDER_REVIEW", state)
	require.NoError(t, pool.QueryRow(ctx, `SELECT legal_drift_state FROM jurisdiction_rules WHERE jurisdiction_rule_id=$1`, draft.JurisdictionRuleID).Scan(&state))
	require.Equal(t, "CURRENT", state, "a DRAFT rule is not in force: nothing drifts")
	var reason, by string
	require.NoError(t, pool.QueryRow(ctx, `SELECT reason, recorded_by_principal_id FROM jurisdiction_rule_drift_events WHERE jurisdiction_rule_id=$1`,
		active.JurisdictionRuleID).Scan(&reason, &by))
	require.Contains(t, reason, ref)
	require.Contains(t, reason, id, "the drift names the feed event it came from")
	require.Equal(t, consumer.FeedActor, by)
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM event_outbox WHERE event_type='legal.drift.detected' AND partition_key=$1`, active.JurisdictionRuleID))

	// A redelivery changes nothing.
	res, err = c.Handle(ctx, feedEvent(t, id, ref))
	require.NoError(t, err)
	require.True(t, res.Duplicate)
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM jurisdiction_rule_drift_events WHERE jurisdiction_rule_id=$1`, active.JurisdictionRuleID))
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM event_outbox WHERE event_type='legal.drift.detected' AND partition_key=$1`, active.JurisdictionRuleID))

	// A new change while the rule is already under review records nothing new; the rule content is never touched.
	res, err = c.Handle(ctx, feedEvent(t, "evt-"+uuid.NewString(), ref))
	require.NoError(t, err)
	require.Equal(t, 0, res.RulesMarked)
	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT rule_status FROM jurisdiction_rules WHERE jurisdiction_rule_id=$1`, active.JurisdictionRuleID).Scan(&status))
	require.Equal(t, "ACTIVE", status)

	// Malformed and unrelated messages are skipped, not retried.
	res, err = c.Handle(ctx, []byte(`{not json`))
	require.NoError(t, err)
	require.Zero(t, res.RulesMarked)
	res, err = c.Handle(ctx, []byte(`{"event_id":"x","event_type":"external.feed.event.ingested","payload":{}}`))
	require.NoError(t, err)
	require.Zero(t, res.RulesMarked)
}
