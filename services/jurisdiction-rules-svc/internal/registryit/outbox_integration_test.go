//go:build integration

package registryit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// A message the broker keeps refusing used to block the whole outbox: the
// worker stops a batch on its first failure and the pending query never
// skipped it. After MaxOutboxAttempts it is parked and the rest flow.
func TestOutbox_ParksAPoisonEvent(t *testing.T) {
	_ = context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM event_outbox`); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEventToOutbox(ctx, pool, "poison", "t", "k", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEventToOutbox(ctx, pool, "healthy", "t", "k", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE event_outbox SET publish_attempts = $1 WHERE event_type = 'poison'`, store.MaxOutboxAttempts); err != nil {
		t.Fatal(err)
	}
	evs, err := st.GetPendingOutboxEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].EventType != "healthy" {
		t.Fatalf("the parked event must be skipped and the healthy one returned, got %+v", evs)
	}
}

// The registry's provenance command also sets supersedes_rule_id; rule_version
// must follow the chain there too, and a chain cannot fork.
func TestProvenance_SupersedesSetsTheVersion(t *testing.T) {
	j := seedJurisdiction(t, "PV")
	v1 := seedRule(t, j.JurisdictionID, "PV_RATE")

	second, _, err := st.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID: j.JurisdictionID, RuleDomain: "TAX", RuleCode: "PV_RATE", RuleName: "v2",
		EffectiveFrom: time.Now().UTC(), RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRuleProvenance(ctx, domain.SetRuleProvenanceParams{
		JurisdictionRuleID: second.JurisdictionRuleID, SupersedesRuleID: &v1.JurisdictionRuleID, ActorID: "author"}); err != nil {
		t.Fatalf("set provenance: %v", err)
	}
	got, err := st.FindRuleByID(ctx, second.JurisdictionRuleID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RuleVersion != 2 {
		t.Fatalf("a successor set through provenance must be version 2, got %d", got.RuleVersion)
	}

	third, _, err := st.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID: j.JurisdictionID, RuleDomain: "TAX", RuleCode: "PV_RATE", RuleName: "fork",
		EffectiveFrom: time.Now().UTC().Add(time.Hour), RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRuleProvenance(ctx, domain.SetRuleProvenanceParams{
		JurisdictionRuleID: third.JurisdictionRuleID, SupersedesRuleID: &v1.JurisdictionRuleID, ActorID: "author"}); !errors.Is(err, domain.ErrSupersedesTaken) {
		t.Fatalf("a second successor through provenance: want ErrSupersedesTaken, got %v", err)
	}
}
