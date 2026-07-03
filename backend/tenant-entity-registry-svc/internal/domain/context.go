package domain

import "context"

type tenantKey struct{}

// WithTenant injects a tenant ID into the context for RLS usage.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

// TenantFromContext extracts the tenant ID from the context.
func TenantFromContext(ctx context.Context) string {
	if t, ok := ctx.Value(tenantKey{}).(string); ok {
		return t
	}
	return ""
}
