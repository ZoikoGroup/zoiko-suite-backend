package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"zoiko.io/vat-gst-svc/internal/middleware"
)

// grantingAuthz is a stand-in authorization-svc that GRANTs everything and
// records the X-Tenant-Id of every decision it was asked for.
func grantingAuthz(t *testing.T, calls *int32, tenants *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		*tenants = append(*tenants, r.Header.Get("X-Tenant-Id"))
		_ = json.NewEncoder(w).Encode(map[string]any{"decision_outcome": "GRANTED", "allowed": true})
	}))
}

// Regression: with no verified tenant the client used to ask authorization-svc
// for a decision in the hardcoded tenant 11111111-1111-1111-1111-111111111111.
// It must now deny without making the call at all.
func TestCheckAllowed_NoTenantIsDeniedWithoutCallingAuthz(t *testing.T) {
	var calls int32
	var tenants []string
	srv := grantingAuthz(t, &calls, &tenants)
	defer srv.Close()

	err := NewClient(srv.URL).CheckAllowed(context.Background(), "principal-1", "le-1", "VAT_RETURN_CREATE")
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("expected ErrAuthorizationDenied without a tenant, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("authorization-svc must not be called without a tenant; it was called %d time(s) with tenants %v", calls, tenants)
	}
}

// The verified tenant is the one forwarded to authorization-svc.
func TestCheckAllowed_ForwardsVerifiedTenant(t *testing.T) {
	var calls int32
	var tenants []string
	srv := grantingAuthz(t, &calls, &tenants)
	defer srv.Close()

	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	if err := NewClient(srv.URL).CheckAllowed(ctx, "principal-1", "le-1", "VAT_RETURN_CREATE"); err != nil {
		t.Fatalf("expected GRANTED, got %v", err)
	}
	if len(tenants) != 1 || tenants[0] != "tenant-a" {
		t.Fatalf("authorization-svc saw tenants %v, want [tenant-a]", tenants)
	}
}

// A decision cached for tenant A must not be replayed for tenant B: the same
// principal/legal-entity/action in another tenant needs its own decision.
func TestCheckAllowed_CacheIsPerTenant(t *testing.T) {
	var calls int32
	var tenants []string
	srv := grantingAuthz(t, &calls, &tenants)
	defer srv.Close()

	client := NewClient(srv.URL)
	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")
	for _, ctx := range []context.Context{ctxA, ctxA, ctxB} {
		if err := client.CheckAllowed(ctx, "principal-1", "le-1", "VAT_RETURN_CREATE"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if calls != 2 {
		t.Fatalf("expected one live decision per tenant (2 calls), got %d: %v", calls, tenants)
	}
}
