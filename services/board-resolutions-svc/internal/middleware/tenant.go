package middleware

import (
	"context"
	"net/http"
)

type contextKey string

const tenantIDKey contextKey = "tenantID"

// TenantContextMiddleware puts the caller's tenant on the context, and nothing
// else — a request with no X-Tenant-Id gets no tenant, and the handlers refuse
// it with 401.
//
// It used to substitute the literal tenant "default" in that case, at both
// ends: here, and again in GetTenantID. Every unscoped request therefore
// landed in one shared bucket that any caller could read and write without
// presenting a tenant at all — board minutes and resolutions from anyone who
// forgot the header, pooled together and visible to each other, with RLS
// dutifully enforcing isolation for a tenant id that identified nobody. A
// missing tenant is a missing scope, not a tenant named "default".
func TenantContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("X-Tenant-Id")
		if tenantID == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), tenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetTenantID returns the caller's tenant, or "" when the request carried no
// tenant scope.
func GetTenantID(ctx context.Context) string {
	if v, ok := ctx.Value(tenantIDKey).(string); ok {
		return v
	}
	return ""
}

// WithTenant is the context constructor tests and the store share, so nothing
// has to know the unexported key.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}
