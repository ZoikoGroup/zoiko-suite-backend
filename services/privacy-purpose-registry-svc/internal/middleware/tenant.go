// Package middleware provides HTTP middleware for privacy-purpose-registry-svc.
package middleware

import (
	"context"
	"net/http"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

type tenantCtxKey struct{}
type platformCtxKey struct{}

// WithTenant records the verified X-Tenant-Id for the request. When the value
// is the platform-scope sentinel (domain.PlatformScopeID) the context is also
// flagged as an explicit platform context — see IsPlatformScope.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	ctx = context.WithValue(ctx, tenantCtxKey{}, tenantID)
	return context.WithValue(ctx, platformCtxKey{}, tenantID == domain.PlatformScopeID)
}

// TenantFromContext returns the verified tenant ("" when the caller sent none).
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

// IsPlatformScope reports whether the caller explicitly asked to act in the
// platform plane. It is true ONLY for the sentinel — an absent tenant is
// never platform scope. The store turns this into the app.platform_scope
// capability that the RLS write policies require for tenant_id IS NULL rows.
func IsPlatformScope(ctx context.Context) bool {
	v, _ := ctx.Value(platformCtxKey{}).(bool)
	return v
}

// TenantContext copies X-Tenant-Id into the request context. It does not
// reject a missing header itself — /healthz and /readyz live outside this
// middleware's group, and every registry handler calls requireTenant, which
// answers 401 TENANT_SCOPE_REQUIRED.
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
