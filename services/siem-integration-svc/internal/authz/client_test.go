package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	svcenvelope "zoiko.io/siem-integration-svc/internal/envelope"
)

// TestCheckAllowed_ForwardsTenantFromEnvelope pins the fix for a real bug:
// authorization-svc's resolveTenantScope silently narrows to global-only SoD
// rules when no tenant is forwarded — a dangerous-because-silent gap. The
// tenant must come from the canonical envelope in context, not be omitted.
func TestCheckAllowed_ForwardsTenantFromEnvelope(t *testing.T) {
	var gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{TenantID: "tenant-a"})
	if err := c.CheckAllowed(ctx, "principal-1", "le-1", "SOME_ACTION"); err != nil {
		t.Fatalf("CheckAllowed: %v", err)
	}
	if gotTenant != "tenant-a" {
		t.Fatalf("expected X-Tenant-Id to be forwarded as %q, got %q", "tenant-a", gotTenant)
	}
}
