package middleware

import (
	"context"
	"encoding/json"
	"net/http"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

// TenantContext extracts the gateway-verified X-Tenant-ID header and stores
// it in context. A request carrying none is REFUSED.
//
// It previously substituted the literal string "tenant-default-001" when
// the header was absent, and GetTenantID returned the same literal as its
// own fallback — two independent fabrication sites. That default was not a
// weaker version of a missing check; it was the opposite of one. A missing
// check makes a request fail; this made it SUCCEED, into a tenant that does
// not exist, shared by every header-less caller — and because the store
// pushes the value straight into Postgres via
// set_config('app.tenant_id', ...) for the RLS policy to read back, every
// header-less caller's rows pooled together inside a policy that looked,
// from the outside, like it was working. Same bug, same fix, already found
// and fixed in 15 other services this session, most recently
// vat-gst-svc and withholding-tax-svc.
func TenantContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "missing_tenant_scope",
				"message": "X-Tenant-ID is required — the gateway sets it from a verified identity envelope",
			})
			return
		}
		ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetTenantID returns the verified tenant, or "" when there is none. It
// never substitutes a placeholder — see TenantContext for why.
func GetTenantID(ctx context.Context) string {
	if val, ok := ctx.Value(TenantIDKey).(string); ok {
		return val
	}
	return ""
}

// WithTenant returns a context carrying tenantID. Used by tests to build a
// scoped context without going through an HTTP request.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantIDKey, tenantID)
}
