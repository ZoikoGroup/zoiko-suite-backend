// Package middleware provides HTTP middleware for expense-claim-svc.
package middleware

import (
	"context"
	"net/http"
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
			if tenantID := r.Header.Get("X-Tenant-Id"); tenantID != "" {
				r = r.WithContext(WithTenant(r.Context(), tenantID))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireTenant refuses a request that carries no tenant. Every row this
// service writes is tenant-scoped (row-level security has no NULL-tenant
// escape hatch), so a tenant-less request can neither read nor write
// anything and is rejected up front with a stable code.
func RequireTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if TenantFromContext(r.Context()) == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"X-Tenant-Id header is required","code":"VALIDATION_FAILED"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
