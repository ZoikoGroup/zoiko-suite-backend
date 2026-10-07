package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/migration-integrity-svc/internal/middleware"
)

// TestCheckAllowed_NoTenantInContext_FailsClosed pins the fix for a real bug:
// checkAllowedLive previously substituted a hardcoded placeholder tenant UUID
// ("11111111-1111-1111-1111-111111111111") whenever context carried none,
// so an authorization decision would be evaluated -- and potentially
// GRANTED -- against a tenant the caller was never verified to belong to.
// middleware.GetTenantID deliberately returns "" rather than a fabricated
// default for exactly this reason (see tenant.go); the authz client must
// refuse rather than invent one.
func TestCheckAllowed_NoTenantInContext_FailsClosed(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	err := c.CheckAllowed(context.Background(), "principal-1", "le-1", "SOME_ACTION")
	if err != ErrTenantMissing {
		t.Fatalf("expected ErrTenantMissing, got %v", err)
	}
	if called {
		t.Fatalf("authorization-svc must not be called with no verified tenant in context")
	}
}

func TestCheckAllowed_ForwardsRealTenant(t *testing.T) {
	var gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, zap.NewNop())
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
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
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	if err := c.CheckAllowed(ctx, "principal-1", "le-1", "SOME_ACTION"); err != ErrAuthorizationDenied {
		t.Fatalf("expected ErrAuthorizationDenied, got %v", err)
	}
}
