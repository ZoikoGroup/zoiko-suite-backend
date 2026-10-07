package middleware

import "context"

type principalCtxKey struct{}

// WithPrincipal returns a context carrying the caller's verified principal.
func WithPrincipal(ctx context.Context, principalID string) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, principalID)
}

// PrincipalFromContext returns the verified principal, or "" if none was
// set — mirrors TenantFromContext's own "never substitute a fabricated
// default" doctrine.
func PrincipalFromContext(ctx context.Context) string {
	v, _ := ctx.Value(principalCtxKey{}).(string)
	return v
}
