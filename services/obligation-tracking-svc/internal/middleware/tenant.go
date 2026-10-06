package middleware

import (
	"context"
	"encoding/json"
	"net/http"
)

type contextKey string

const tenantIDKey contextKey = "tenantID"

// TenantContextMiddleware extracts the gateway-verified X-Tenant-Id header
// and stores it in context. A request carrying none is REFUSED.
//
// It previously substituted the literal string "default" when the header
// was absent, and GetTenantID returned the same literal as its own
// fallback — two independent fabrication sites, the same bug already found
// and fixed in board-resolutions-svc, contract-lifecycle-svc and
// clause-template-svc this session. Compounded here by the store carrying
// no explicit tenant_id predicate of its own (see migration 000002's doc
// comment), a header-less caller's obligations pooled together inside an
// RLS policy that never actually applied to this connection.
func TenantContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("X-Tenant-Id")
		if tenantID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "missing_tenant_scope",
				"message": "X-Tenant-Id is required — the gateway sets it from a verified identity envelope",
			})
			return
		}
		ctx := context.WithValue(r.Context(), tenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetTenantID returns the verified tenant, or "" when there is none. It
// never substitutes a placeholder — see TenantContextMiddleware for why.
func GetTenantID(ctx context.Context) string {
	if v, ok := ctx.Value(tenantIDKey).(string); ok {
		return v
	}
	return ""
}

// WithTenant returns a context carrying tenantID. Used by tests to build a
// scoped context without going through an HTTP request.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}
