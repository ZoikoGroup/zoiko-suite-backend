package outbox_test

import (
	"encoding/json"
	"strings"
	"testing"

	"zoiko.io/identity-context-svc/internal/outbox"
)

func TestCanonicalTypeMapsLegacyNames(t *testing.T) {
	for legacy, want := range map[string]string{
		"identity.context.resolved":             "com.zoikosuite.identity.context.resolved",
		"identity.mfa_factor.enrolled":          "com.zoikosuite.identity.mfa-factor.enrolled",
		"identity.federation.version_proposed":  "com.zoikosuite.identity.federation.version-proposed",
		"identity.refresh_token.reuse_detected": "com.zoikosuite.identity.refresh-token.reuse-detected",
		"session.invalidated":                   "com.zoikosuite.identity.session.invalidated",
		"principal.status.changed":              "com.zoikosuite.identity.principal.status-changed",
	} {
		if got := outbox.CanonicalType(legacy); got != want {
			t.Errorf("%s -> %s, want %s", legacy, got, want)
		}
	}
}

func rendered(tenant string, payload string) outbox.Record {
	return outbox.Record{
		EventID: "evt-1", EventType: "identity.subject.merged", TenantID: tenant, PartitionKey: "p-1",
		Payload: []byte(`{"event_id":"evt-1","event_type":"identity.subject.merged","emitted_at":"2026-11-01T10:00:00Z",` +
			`"source_service":"identity-context-svc","tenant_id":"` + tenant + `","actor_id":"a-1","correlation_id":"c-1","payload":` + payload + `}`),
	}
}

func TestToEnvelopeKeepsTheLegacyWireAndAddsTheCanonicalType(t *testing.T) {
	outbox.SetEventRegion("uk")
	t.Cleanup(func() { outbox.SetEventRegion("") })

	env, err := outbox.ToEnvelope(rendered("t-1", `{"merge_id":"m-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"type":           "com.zoikosuite.identity.subject.merged",
		"event_type":     "identity.subject.merged", // what existing consumers filter on
		"source_service": "identity-context-svc",
		"tenant_id":      "t-1",
		"actor_id":       "a-1",
		"correlation_id": "c-1",
	} {
		if wire[k] != want {
			t.Errorf("%s = %v, want %s", k, wire[k], want)
		}
	}
	if p, _ := wire["payload"].(map[string]any); p["merge_id"] != "m-1" {
		t.Errorf("legacy payload member lost: %v", wire["payload"])
	}
}

func TestATenantlessEventIsScopedToThePlatformTenant(t *testing.T) {
	outbox.SetEventRegion("uk")
	t.Cleanup(func() { outbox.SetEventRegion("") })
	rec := rendered("", `{"failure_reason":"unknown subject"}`)
	rec.EventType = "identity.context.resolution_failed"
	env, err := outbox.ToEnvelope(rec)
	if err != nil {
		t.Fatal(err)
	}
	if env.TenantID != outbox.PlatformTenant {
		t.Fatalf("tenant = %q, want %q", env.TenantID, outbox.PlatformTenant)
	}
}

func TestAnEventWithoutAResidencyRegionIsRefused(t *testing.T) {
	outbox.SetEventRegion("")
	_, err := outbox.ToEnvelope(rendered("t-1", `{}`))
	if err == nil || !strings.Contains(err.Error(), "residency region") {
		t.Fatalf("want a residency refusal, got %v", err)
	}
}

func TestACredentialInAPayloadIsRefusedNotPublished(t *testing.T) {
	outbox.SetEventRegion("uk")
	t.Cleanup(func() { outbox.SetEventRegion("") })
	_, err := outbox.ToEnvelope(rendered("t-1", `{"password":"hunter2hunter2"}`))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("want a refusal that does not echo the value, got %v", err)
	}
}
