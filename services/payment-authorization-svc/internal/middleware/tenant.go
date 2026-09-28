// Package middleware provides HTTP middleware for payment-authorization-svc.
package middleware

import (
	"context"
	"net/http"
)

type tenantCtxKey struct{}
type correlationCtxKey struct{}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationCtxKey{}, correlationID)
}

func CorrelationIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationCtxKey{}).(string)
	return v
}

func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if tenantID := r.Header.Get("X-Tenant-Id"); tenantID != "" {
				ctx = WithTenant(ctx, tenantID)
			}
			if correlationID := r.Header.Get("X-Correlation-ID"); correlationID != "" {
				ctx = WithCorrelationID(ctx, correlationID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
