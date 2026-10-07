package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTenantContext_MissingHeader_Refuses pins the fix for a real bug: this
// middleware used to substitute the literal tenant "tenant-default-001"
// when X-Tenant-ID was absent, and GetTenantID returned the same literal as
// its own fallback — so every header-less request succeeded under a
// shared, nonexistent tenant instead of being refused.
func TestTenantContext_MissingHeader_Refuses(t *testing.T) {
	var sawTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTenant = GetTenantID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/anomalies", nil)
	w := httptest.NewRecorder()
	TenantContext(next).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Tenant-ID, got %d", w.Code)
	}
	if sawTenant != "" {
		t.Fatalf("handler must never be reached without a tenant, but saw tenant %q", sawTenant)
	}
}

func TestTenantContext_WithHeader_PassesThrough(t *testing.T) {
	var sawTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTenant = GetTenantID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/anomalies", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	w := httptest.NewRecorder()
	TenantContext(next).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with a real tenant, got %d", w.Code)
	}
	if sawTenant != "tenant-a" {
		t.Fatalf("expected the handler to see tenant-a, got %q", sawTenant)
	}
}

func TestGetTenantID_NoContext_ReturnsEmpty(t *testing.T) {
	if got := GetTenantID(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != "" {
		t.Fatalf("expected empty string for a context with no tenant, got %q", got)
	}
}