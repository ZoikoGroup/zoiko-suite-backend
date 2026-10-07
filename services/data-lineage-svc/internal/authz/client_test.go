package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	svcenvelope "zoiko.io/data-lineage-svc/internal/envelope"
)

// TestCheckAllowed_CallsRealAuthorizeRoute pins the fix for a real bug:
// this client previously POSTed to /v1/authorization/check, a route
// authorization-svc does not register (it only has POST /v1/authorize).
// Every call 404'd, and CheckAllowed's own fail-closed handling mapped
// every non-200 to a denial — so every authz-gated action in this
// service was permanently refused, in every environment, with nothing
// ever indicating why. No test caught this because none existed.
func TestCheckAllowed_CallsRealAuthorizeRoute(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	if err := c.CheckAllowed(context.Background(), "principal-1", "le-1", "SOME_ACTION"); err != nil {
		t.Fatalf("CheckAllowed: %v", err)
	}
	if gotPath != "/v1/authorize" {
		t.Fatalf("expected POST to /v1/authorize (authorization-svc's real route), got %q", gotPath)
	}
}

// TestCheckAllowed_ForwardsTenantFromEnvelope pins the companion fix:
// authorization-svc's resolveTenantScope silently narrows to
// global-only SoD rules when no tenant is forwarded — a real,
// dangerous-because-silent gap, not a visible failure like the wrong
// route above. The tenant must come from the canonical envelope in
// context, not be invented.
func TestCheckAllowed_ForwardsTenantFromEnvelope(t *testing.T) {
	var gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{TenantID: "tenant-a"})
	if err := c.CheckAllowed(ctx, "principal-1", "le-1", "SOME_ACTION"); err != nil {
		t.Fatalf("CheckAllowed: %v", err)
	}
	if gotTenant != "tenant-a" {
		t.Fatalf("expected X-Tenant-Id to be forwarded as %q, got %q", "tenant-a", gotTenant)
	}
}

func TestCheckAllowed_Denied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "DENIED"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	if err := c.CheckAllowed(context.Background(), "principal-1", "le-1", "SOME_ACTION"); err != ErrAuthorizationDenied {
		t.Fatalf("expected ErrAuthorizationDenied, got %v", err)
	}
}

func TestCheckAllowed_ServiceUnavailable_FailsClosed(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", zap.NewNop()) // nothing listens here
	if err := c.CheckAllowed(context.Background(), "principal-1", "le-1", "SOME_ACTION"); err != ErrAuthzServiceUnavailable {
		t.Fatalf("expected ErrAuthzServiceUnavailable, got %v", err)
	}
}
