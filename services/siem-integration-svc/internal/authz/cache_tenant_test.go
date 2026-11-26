package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	svcenvelope "zoiko.io/siem-integration-svc/internal/envelope"
)

// A decision cached for one tenant must not answer the same principal, legal
// entity and action asked under another tenant: authorization-svc evaluates
// the forwarded tenant's own rules, so the two questions can differ.
func TestCheckAllowed_CacheKeyIncludesTenant(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		outcome := "DENIED"
		if r.Header.Get("X-Tenant-Id") == "tenant-a" {
			outcome = "GRANTED"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": outcome})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctxA := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{TenantID: "tenant-a"})
	ctxB := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{TenantID: "tenant-b"})

	if err := c.CheckAllowed(ctxA, "p-1", "le-1", "ACTION"); err != nil {
		t.Fatalf("tenant-a: expected GRANTED, got %v", err)
	}
	if err := c.CheckAllowed(ctxB, "p-1", "le-1", "ACTION"); err != ErrAuthorizationDenied {
		t.Fatalf("tenant-b must be asked afresh and DENIED, got %v (cache answered across tenants)", err)
	}
	if err := c.CheckAllowed(ctxA, "p-1", "le-1", "ACTION"); err != nil {
		t.Fatalf("tenant-a (cached): expected GRANTED, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 live calls (one per tenant, then a cache hit), got %d", calls)
	}
}
