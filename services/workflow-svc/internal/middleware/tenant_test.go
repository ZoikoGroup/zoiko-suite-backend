package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/workflow-svc/internal/middleware"
)

func TestTenantContext_SetsTenantFromHeader(t *testing.T) {
	handler := middleware.TenantContext()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := middleware.TenantFromContext(r.Context())
		if tenant != "t-1" {
			t.Errorf("TenantFromContext = %q, want t-1", tenant)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("handler returned %d, want 200", w.Code)
	}
}

func TestTenantContext_NoHeaderReturnsEmpty(t *testing.T) {
	handler := middleware.TenantContext()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := middleware.TenantFromContext(r.Context())
		if tenant != "" {
			t.Errorf("TenantFromContext = %q, want empty", tenant)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("handler returned %d, want 200", w.Code)
	}
}

func TestTenantContext_WithTenant(t *testing.T) {
	ctx := middleware.WithTenant(context.Background(), "t-1")
	tenant := middleware.TenantFromContext(ctx)
	if tenant != "t-1" {
		t.Errorf("TenantFromContext = %q, want t-1", tenant)
	}
}

func TestTenantContext_TenantFromContext_NilContext(t *testing.T) {
	tenant := middleware.TenantFromContext(context.Background())
	if tenant != "" {
		t.Errorf("TenantFromContext(empty) = %q, want empty", tenant)
	}
}
