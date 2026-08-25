package middleware

import (
	"context"
	"encoding/json"
	"net/http"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

// TenantMiddleware records the gateway-verified X-Tenant-ID on the request
// context, and REFUSES a request that carries none.
//
// It previously substituted the literal string "default-tenant" when the
// header was absent, and GetTenantID returned the same literal as its
// fallback — two independent fabrication sites, so fixing either alone
// would have left the hole open.
//
// That default was not a weaker version of a missing check; it was the
// opposite of one. A missing check makes a request fail. This made it
// SUCCEED, into a tenant that does not exist, shared by every header-less
// caller.
//
// On this service that mattered more than usual, because the tenant value
// does not stop at the application layer — the store pushes it straight
// into Postgres:
//
//	SELECT set_config('app.tenant_id', $1, true)
//
// and the row-level security policy reads it back:
//
//	USING (tenant_id = current_setting('app.tenant_id', true))
//
// So with the fabricated default in place the policy was not bypassed, it
// was SATISFIED. Postgres did exactly as instructed and returned every row
// whose tenant_id was 'default-tenant', which is how all header-less
// callers came to share each other's rows legitimately, inside the policy.
//
// That is worse than having no policy at all, because everything looks
// correct from the outside: pg_class reports the policy exists, and any
// test that passes a real tenant goes green. The database was faithfully
// enforcing the caller's own fabricated assertion against itself.
//
// GetTenantID now returns "" when there is no verified tenant. "" matches
// no real tenant_id and, pushed through set_config, matches no row — so the
// failure mode is an empty result rather than a shared bucket.
func TenantMiddleware(next http.Handler) http.Handler {
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

// WithTenant returns a context carrying tenantID. Used by tests to build a
// scoped context without going through an HTTP request.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantIDKey, tenantID)
}

// GetTenantID returns the verified tenant, or "" when there is none. It
// never substitutes a placeholder: see TenantMiddleware for why the previous
// "default-tenant" fallback turned a working RLS policy into one that
// silently pooled unrelated tenants.
func GetTenantID(ctx context.Context) string {
	if val, ok := ctx.Value(TenantIDKey).(string); ok {
		return val
	}
	return ""
}
