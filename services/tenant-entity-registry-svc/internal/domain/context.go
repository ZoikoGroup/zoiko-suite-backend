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

type principalKey struct{}

// WithPrincipal injects the acting principal into the context.
//
// This is the identity every audit column on a mutation is stamped with, and
// the subject every authorization decision is evaluated for. It comes from the
// gateway-verified X-Principal-Id header — never from a token this service
// parses itself. See internal/identity for why that distinction matters.
func WithPrincipal(ctx context.Context, principalID string) context.Context {
	return context.WithValue(ctx, principalKey{}, principalID)
}

// PrincipalFromContext extracts the acting principal from the context.
// Returns "" when no verified identity was presented — callers must treat
// that as unauthenticated, never as a default or a system actor.
func PrincipalFromContext(ctx context.Context) string {
	if p, ok := ctx.Value(principalKey{}).(string); ok {
		return p
	}
	return ""
}

type legalEntityKey struct{}

// WithLegalEntity injects the gateway-resolved legal entity scope, when the
// caller's envelope carried one.
func WithLegalEntity(ctx context.Context, legalEntityID string) context.Context {
	return context.WithValue(ctx, legalEntityKey{}, legalEntityID)
}

// LegalEntityFromContext extracts the caller's legal entity scope.
func LegalEntityFromContext(ctx context.Context) string {
	if e, ok := ctx.Value(legalEntityKey{}).(string); ok {
		return e
	}
	return ""
}
