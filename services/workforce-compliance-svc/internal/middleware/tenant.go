package middleware

import (
	"context"
	"net/http"
)

type contextKey string

const tenantIDKey contextKey = "tenant_id"

func TenantContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("X-Tenant-Id")
		if tenantID == "" {
			tenantID = "default-tenant"
		}
		ctx := context.WithValue(r.Context(), tenantIDKey, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func GetTenantID(ctx context.Context) string {
	val, ok := ctx.Value(tenantIDKey).(string)
	if !ok || val == "" {
		return "default-tenant"
	}
	return val
}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}
