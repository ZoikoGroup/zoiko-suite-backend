package events_test

import (
	"encoding/json"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
)

func outboxEnvelopes(t *testing.T, d domain.AccessDecisionLog) []map[string]any {
	t.Helper()
	msgs, err := events.DecisionEvents(d)
	if err != nil {
		t.Fatalf("DecisionEvents: %v", err)
	}
	var out []map[string]any
	for _, m := range msgs {
		var env map[string]any
		if err := json.Unmarshal(m.Value, &env); err != nil {
			t.Fatalf("envelope is not JSON: %v", err)
		}
		if env["event_type"] != m.EventType {
			t.Errorf("row event_type %q but envelope says %v", m.EventType, env["event_type"])
		}
		if m.Key != d.AccessDecisionID {
			t.Errorf("key = %q, want the decision id", m.Key)
		}
		out = append(out, env)
	}
	return out
}

func TestDecisionEvents_GrantedCarriesTenant(t *testing.T) {
	tenant := "11111111-1111-4111-8111-111111111111"
	envs := outboxEnvelopes(t, domain.AccessDecisionLog{AccessDecisionID: "d-1", DecisionOutcome: "GRANTED", DecisionBasis: "rbac:role=X", TenantID: &tenant})
	if len(envs) != 1 || envs[0]["event_type"] != "authorization.granted" {
		t.Fatalf("got %v, want one authorization.granted", envs)
	}
	if envs[0]["tenant_id"] != tenant {
		t.Errorf("envelope tenant_id = %v, want %s (audit: the envelope had no tenant)", envs[0]["tenant_id"], tenant)
	}
}

func TestDecisionEvents_TenantlessOmitsTenant(t *testing.T) {
	envs := outboxEnvelopes(t, domain.AccessDecisionLog{AccessDecisionID: "d-2", DecisionOutcome: "DENIED", DecisionBasis: "no_grant"})
	if len(envs) != 1 || envs[0]["event_type"] != "authorization.denied" {
		t.Fatalf("got %v, want one authorization.denied", envs)
	}
	if _, ok := envs[0]["tenant_id"]; ok {
		t.Error("a tenantless decision's envelope carries a fabricated tenant_id")
	}
}

func TestDecisionEvents_SoDDenialAlsoPublishesViolation(t *testing.T) {
	envs := outboxEnvelopes(t, domain.AccessDecisionLog{AccessDecisionID: "d-3", ActionType: "PAYMENT_RELEASE", DecisionOutcome: "DENIED", DecisionBasis: "sod:conflict_with=PAYMENT_APPROVE"})
	if len(envs) != 2 || envs[0]["event_type"] != "authorization.denied" || envs[1]["event_type"] != "sod.violation.detected" {
		t.Fatalf("got %v, want denied + sod.violation.detected", envs)
	}
	p := envs[1]["payload"].(map[string]any)
	if p["conflicting_action"] != "PAYMENT_APPROVE" || p["candidate_action"] != "PAYMENT_RELEASE" {
		t.Errorf("sod payload = %v", p)
	}
}

// The direct path published authorization.denied for every non-GRANTED
// outcome, STEP_UP included; the outbox keeps that.
func TestDecisionEvents_StepUpIsDenied(t *testing.T) {
	envs := outboxEnvelopes(t, domain.AccessDecisionLog{AccessDecisionID: "d-4", DecisionOutcome: "STEP_UP", DecisionBasis: "step_up:assurance"})
	if len(envs) != 1 || envs[0]["event_type"] != "authorization.denied" {
		t.Fatalf("got %v, want authorization.denied", envs)
	}
}
