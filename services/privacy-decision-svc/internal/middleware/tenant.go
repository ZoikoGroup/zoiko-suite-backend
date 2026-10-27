// Package middleware provides HTTP middleware and context utilities for privacy-decision-svc.
package middleware

import (
	"context"
	"net/http"
	"strings"
)

type (
	tenantCtxKey      struct{}
	principalCtxKey   struct{}
	correlationCtxKey struct{}
)

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, strings.TrimSpace(tenantID))
}

func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

func WithPrincipal(ctx context.Context, principalID string) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, strings.TrimSpace(principalID))
}

func PrincipalFromContext(ctx context.Context) string {
	v, _ := ctx.Value(principalCtxKey{}).(string)
	return v
}

func WithCorrelation(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationCtxKey{}, strings.TrimSpace(correlationID))
}

func CorrelationFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationCtxKey{}).(string)
	return v
}

// RequestContext extracts X-Tenant-Id, X-Principal-Id, and X-Correlation-ID headers into the request context.
func RequestContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id")); tenantID != "" {
				ctx = WithTenant(ctx, tenantID)
			}
			if principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id")); principalID != "" {
				ctx = WithPrincipal(ctx, principalID)
			}
			if correlationID := strings.TrimSpace(r.Header.Get("X-Correlation-ID")); correlationID != "" {
				ctx = WithCorrelation(ctx, correlationID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// TenantContext is maintained for backwards compatibility with cmd/server.
func TenantContext() func(http.Handler) http.Handler {
	return RequestContext()
}
