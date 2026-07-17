// Package middleware provides HTTP middleware for purchase-request-svc.
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}

// WithTenant returns a context carrying tenantID for RLS enforcement.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFromContext returns the tenant_id set by TenantContext, or "" if absent.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext reads the caller's tenant scope from X-Tenant-Id — set by
// gateway-auth-svc's ForwardAuth verification (or Traefik, in a real
// deployment) after checking the signed IdentityContextEnvelope JWT, exactly
// like X-Principal-Id (see internal/handler.requirePrincipal). This service
// never decodes a JWT itself: identity and tenant scope are resolved once,
// upstream of every backend, not re-derived independently by each service
// (03-microservices.md §9.1 critical constraint).
func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tenantID := r.Header.Get("X-Tenant-Id"); tenantID != "" {
				r = r.WithContext(WithTenant(r.Context(), tenantID))
			}
			next.ServeHTTP(w, r)
		})
	}
}
