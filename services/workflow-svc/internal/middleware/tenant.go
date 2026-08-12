// Package middleware provides HTTP middleware for workflow-svc.
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}

// WithTenant returns a context carrying tenantID for tenant-scoped queries.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFromContext returns the tenant_id set by TenantContext, or "" if absent.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// TenantContext reads the caller's tenant scope from X-Tenant-Id — set by
// gateway-auth-svc's ForwardAuth verification, same pattern as
// accounts-payable-svc/tax-rules-svc/document-vault-svc.
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
