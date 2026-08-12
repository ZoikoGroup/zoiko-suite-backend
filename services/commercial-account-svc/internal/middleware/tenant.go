// Package middleware provides HTTP middleware for commercial-account-svc.
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}

// WithTenant returns a context carrying the caller's organization scope.
func WithTenant(ctx context.Context, organizationID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, organizationID)
}

// TenantFromContext returns the organization_id set by TenantContext, or ""
// if absent — callers must treat an empty value as "no verified scope", not
// as a wildcard, per this platform's established RLS-stop-gap discipline
// (see accounts-payable-svc/tax-rules-svc/document-vault-svc's own
// middleware.tenant.go for the same pattern).
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext reads the caller's organization scope from X-Tenant-Id — set
// by gateway-auth-svc's ForwardAuth verification, same pattern as every
// other service in this platform.
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
