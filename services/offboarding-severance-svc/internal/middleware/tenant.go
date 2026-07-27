package middleware

import (
	"context"
	"net/http"
)

type contextKey string

const tenantIDKey contextKey = "tenant_id"

// TenantContextMiddleware resolves the tenant from X-Tenant-Id. A missing
// header is left unresolved (empty string) rather than defaulting to a
// shared bucket — callers must fail closed on an empty tenant, never
// silently pool unrelated tenants' data together.
func TenantContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("X-Tenant-Id")
		ctx := context.WithValue(r.Context(), tenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func GetTenantID(ctx context.Context) string {
	val, _ := ctx.Value(tenantIDKey).(string)
	return val
}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}
