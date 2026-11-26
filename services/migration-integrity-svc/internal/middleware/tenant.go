package middleware

import (
	"context"
	"net/http"
	"strings"

	"zoiko.io/migration-integrity-svc/internal/problem"
)

type contextKey string

// TenantIDKey is the request-context key holding the verified tenant.
const TenantIDKey contextKey = "tenant_id"

// TenantHeader is the only source of tenant authority. Go canonicalises
// header names, so "X-Tenant-ID" and "X-Tenant-Id" are the same header.
const TenantHeader = "X-Tenant-Id"

// TenantMiddleware records the gateway-verified X-Tenant-Id on the request
// context, and REFUSES (401, RFC 9457) a request that carries none.
//
// History: it once substituted the literal "default-tenant" when the header
// was absent, and GetTenantID returned the same literal as its fallback.
// Because the store pushes the tenant into Postgres
// (set_config('app.tenant_id', $1, true)) and the RLS policy reads it back,
// that default did not bypass row-level security — it SATISFIED it, pooling
// every header-less caller into one shared tenant. Never reintroduce a
// fallback: a missing tenant is an authentication failure.
func TenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := strings.TrimSpace(r.Header.Get(TenantHeader))
		if tenantID == "" {
			problem.Write(w, r, http.StatusUnauthorized, "missing-tenant-scope", "Missing tenant scope",
				"X-Tenant-Id is required; the gateway sets it from a verified identity envelope")
			return
		}
		ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WithTenant returns a context carrying tenantID. Used by tests to build a
// scoped context without going through an HTTP request.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantIDKey, tenantID)
}

// GetTenantID returns the verified tenant, or "" when there is none. It never
// substitutes a placeholder: "" matches no tenant_id row under RLS.
func GetTenantID(ctx context.Context) string {
	if val, ok := ctx.Value(TenantIDKey).(string); ok {
		return val
	}
	return ""
}
