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

// WithCorrelation puts the request's correlation id on the context.
func WithCorrelation(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationCtxKey{}, correlationID)
}

// CorrelationFromContext returns the correlation id of the request in flight,
// or "" outside one.
//
// The store reads it when it builds an event. Without it, an event enqueued by
// an update carried the correlation id the ROW has held since it was created —
// so role.updated could not be joined to the request that caused the update,
// only to the one that created the role, which is the single most useful thing
// a correlation id is for.
func CorrelationFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationCtxKey{}).(string)
	return v
}

// TenantContext lifts the verified X-Tenant-Id and the request's correlation id
// onto the context.
//
// It sets no default for either: a request with no tenant reaches the handler
// with an empty one and is refused there, which keeps "the gateway did not set
// it" distinguishable from "it was set to something".
func TenantContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if tenantID := r.Header.Get("X-Tenant-Id"); tenantID != "" {
				ctx = WithTenant(ctx, tenantID)
			}
			if correlationID := r.Header.Get("X-Correlation-ID"); correlationID != "" {
				ctx = WithCorrelation(ctx, correlationID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
