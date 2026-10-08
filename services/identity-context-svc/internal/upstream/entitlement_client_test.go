package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GOV-01 §4: the entitlement context reference now resolves from COM-03.
func TestEntitlementClient_ResolvesADeterministicReference(t *testing.T) {
	calls := 0
	sub, pv, lim, unit := "sub-1", 3, int64(50), "users"
	decisions := []comDecision{
		{CapabilityKey: "seats", Outcome: "ALLOW", LimitValue: &lim, LimitUnit: &unit, SubscriptionID: &sub, PolicyVersion: &pv},
		{CapabilityKey: "ai.assist", Outcome: "DENY", SubscriptionID: &sub, PolicyVersion: &pv},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/commercial/entitlements/effective" || r.URL.Query().Get("organization_id") != "t-1" ||
			r.Header.Get("X-Principal-Id") != "svc-identity-context" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"decisions": decisions})
	}))
	defer srv.Close()

	c := NewEntitlementClient(srv.URL, "svc-identity-context", 0)
	ref, err := c.ResolveEntitlementContext(context.Background(), "t-1", "le-1")
	if err != nil || ref == nil || !strings.HasPrefix(*ref, "com03:sub=sub-1;policy=3;restr=;d=") {
		t.Fatalf("ref = %v, %v", ref, err)
	}
	// Cached: the commercial plane is not on every login.
	if _, err := c.ResolveEntitlementContext(context.Background(), "t-1", ""); err != nil || calls != 1 {
		t.Fatalf("calls = %d, %v", calls, err)
	}
	// Order-independent, and any change to an outcome changes it.
	rev := []comDecision{decisions[1], decisions[0]}
	if EntitlementReference(rev) != *ref {
		t.Fatal("reference depends on response order")
	}
	decisions[1].Outcome = "ALLOW"
	if EntitlementReference(decisions) == *ref {
		t.Fatal("a changed entitlement must change the reference")
	}
}

func TestEntitlementClient_FailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := NewEntitlementClient(srv.URL, "svc", 0).ResolveEntitlementContext(context.Background(), "t-1", ""); err == nil {
		t.Fatal("a refused read must surface (the resolver records UPSTREAM_UNAVAILABLE)")
	}
}
