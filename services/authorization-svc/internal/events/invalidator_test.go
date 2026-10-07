package events_test

import (
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/events"
)

type tenantCacheStub struct{ tenants []string }

func (c *tenantCacheStub) InvalidateTenant(t string) { c.tenants = append(c.tenants, t) }

// Every replica's invalidator drops its cache for the events after which a
// cached read may be wrong — including this service's own IAM events — and
// ignores the decision stream.
func TestCacheInvalidator(t *testing.T) {
	c := &tenantCacheStub{}
	inv := events.NewCacheInvalidator(zap.NewNop(), c)
	for _, et := range []string{"iam.assignment.revoked", "iam.delegation.revoked", "iam.policy_set.published",
		"authorization.cache.invalidated", "principal.status.changed", "entity.status.changed", "authority.revoked"} {
		raw, _ := json.Marshal(map[string]any{"event_type": et, "tenant_id": "t-1"})
		inv.Handle(raw)
	}
	if len(c.tenants) != 7 {
		t.Fatalf("invalidated %d times, want 7", len(c.tenants))
	}
	for _, et := range []string{"authorization.granted", "authorization.denied", "something.else"} {
		raw, _ := json.Marshal(map[string]any{"event_type": et, "tenant_id": "t-1"})
		inv.Handle(raw)
	}
	inv.Handle([]byte("not json"))
	if len(c.tenants) != 7 {
		t.Errorf("decision events / junk invalidated the cache: %d", len(c.tenants))
	}
}
