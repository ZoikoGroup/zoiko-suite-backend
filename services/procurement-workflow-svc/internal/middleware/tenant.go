package middleware

import (
	"context"
	"net/http"
	"strings"
)

type tenantCtxKey struct{}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Every /v1 route is tenant- and caller-scoped. Refuse unscoped calls
			// here with 401 rather than letting them surface later as a 404
			// (lookup under an empty tenant) or a 503 (store refusing the call).
			if strings.HasPrefix(r.URL.Path, "/v1/") &&
				(r.Header.Get("X-Tenant-Id") == "" || r.Header.Get("X-Principal-Id") == "") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"identity_missing","code":"IDENTITY_MISSING","detail":"X-Tenant-Id and X-Principal-Id are required"}`))
				return
			}
			if tenantID := r.Header.Get("X-Tenant-Id"); tenantID != "" {
				r = r.WithContext(WithTenant(r.Context(), tenantID))
			}
			next.ServeHTTP(w, r)
		})
	}
}
