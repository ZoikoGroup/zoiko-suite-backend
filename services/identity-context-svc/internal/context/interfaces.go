// Package context defines the dependency interfaces for the resolver.
// All dependencies are expressed as interfaces so implementations can be
// swapped for mocks in tests without any real infrastructure.
package context

import (
	"context"
	"time"

	"zoiko.io/identity-context-svc/internal/domain"
)

// PrincipalStore is the data-access contract for Principal records.
// Owned by this service per the authoritative data boundary in the spec.
type PrincipalStore interface {
	FindByIDPSubject(ctx context.Context, subject, tenantID string) (*domain.Principal, error)
	FindByID(ctx context.Context, principalID, tenantID string) (*domain.Principal, error)
	FindActiveRoleAssignments(ctx context.Context, principalID, tenantID string, legalEntityID *string) ([]domain.PrincipalRoleAssignment, error)
	FindActiveDelegations(ctx context.Context, principalID, tenantID string) ([]domain.DelegatedAuthority, error)
	UpdateStatus(ctx context.Context, principalID, tenantID string, newStatus domain.PrincipalStatus, actorID, correlationID string) error
}

// SessionCache manages the Redis-backed session envelope store.
// SessionContext records are NEVER deleted from the backing store.
// invalidated_at is appended (append-only evidence obligation).
type SessionCache interface {
	Put(ctx context.Context, sessionContextID, envelopeJWT string) error

	// Get, GetSessionContext, Invalidate and EvictAllForPrincipal all take the
	// caller's verified tenant. Redis session keys are flat — session:jwt:<ulid>
	// carries no owner — so the tenant cannot be derived from the id, and
	// without it these routes served and revoked across tenant boundaries while
	// every /v1/principals route beside them scoped correctly.
	Get(ctx context.Context, sessionContextID, tenantID string) (string, error)
	Evict(ctx context.Context, sessionContextID string) error
	PersistSessionContext(ctx context.Context, sc domain.SessionContext) error
	GetSessionContext(ctx context.Context, sessionContextID, tenantID string) (*domain.SessionContext, error)
	Invalidate(ctx context.Context, sessionContextID, tenantID string, reason domain.InvalidationReason, at time.Time) error

	// EvictAllForPrincipal revokes every live session for a principal and
	// returns how many were revoked. reason is recorded on each SessionContext
	// so the evidence trail distinguishes a delegation revocation from a
	// logout; a bare eviction would leave every revoked session looking as
	// though it had simply expired.
	EvictAllForPrincipal(ctx context.Context, principalID, tenantID string, reason domain.InvalidationReason) (int, error)
}

// RiskSignalCache provides READ-ONLY access to asynchronously-populated
// risk signals.
//
// ARCHITECTURAL INVARIANT (Q3 resolution):
//
//	Resolve() must never call the Intelligence Plane or any Tier 2/3 service
//	synchronously. Risk scores arrive via an async consumer that writes into
//	Redis. This interface only exposes reads — writes are on a separate path.
type RiskSignalCache interface {
	GetLatestSignal(ctx context.Context, principalID string) (*domain.RiskSignalCache, error)
}

// UpstreamRegistry is a read-only client to upstream Tier 0 services.
// This service never writes to any upstream domain.
//
// FetchActiveDelegations used to be here and is deliberately gone. Delegated
// authority for a principal lives in THIS service's own delegated_authorities
// table and PrincipalStore already reads it, so routing the question through an
// "upstream" meant a local fact was answered by a stub that returned an empty
// slice — and an empty slice is indistinguishable from "this principal holds no
// delegations" once it is inside a signed envelope. The resolver reads the store
// directly, exactly as it already does for role assignments.
type UpstreamRegistry interface {
	IsTenantActive(ctx context.Context, tenantID string) (bool, error)

	// ResolveEntityScope takes the principal's own verified tenant because the
	// registry scopes the entity read by it — that scoping IS the isolation
	// check, so it cannot be derived from the entity being asked for.
	//
	// Returns the scope rather than a bool so the entity's data residency
	// policy, which arrives on the same response, reaches the SessionContext
	// instead of being discarded.
	ResolveEntityScope(ctx context.Context, principalID, tenantID, legalEntityID string) (*domain.EntityScope, error)

	// ResolvePermissionBundles takes the principal's verified tenant because
	// access-control-svc scopes the read by it, exactly as the entity read is
	// scoped. Fan-out per role — there is no batch route.
	ResolvePermissionBundles(ctx context.Context, tenantID string, roleIDs []string) ([]string, error)
}

// EventPublisher emits append-only domain events to the event backbone.
// Publish calls are fire-and-forget from Resolve()'s perspective — the
// resolver does not block on event publication, but errors returned by
// these methods ARE logged at ERROR level with principal_id and event_type
// context so they are observable (Gap 1 fix).
// Gap 2 NOTE: there is still no drain/WaitGroup on shutdown — in-flight
// goroutines may be lost on SIGTERM. Tracked as a Phase 1 exit-criteria
// gap to be addressed in a follow-up PR with an outbox or WaitGroup drain.
type EventPublisher interface {
	PublishContextResolved(ctx context.Context, principalID, tenantID, legalEntityID, sessionContextID, correlationID string) error
	PublishResolutionFailed(ctx context.Context, subject, correlationID, reason string) error
	PublishSessionInvalidated(ctx context.Context, sessionContextID, principalID string, reason domain.InvalidationReason, correlationID string) error
	PublishRiskSignalUnavailable(ctx context.Context, principalID, correlationID string) error
	PublishPrincipalStatusChanged(ctx context.Context, principalID, tenantID string, newStatus domain.PrincipalStatus, actorID, correlationID string) error
}

// TokenVerifier validates an inbound bearer token or SAML assertion
// and returns the verified claims. Swappable for test mocks.
type TokenVerifier interface {
	VerifyBearer(ctx context.Context, token string) (*domain.VerifiedClaims, error)
}

// EnvelopeSigner signs an IdentityContextEnvelope as a short-lived JWT.
// Production implementation uses RS256 with KMS-backed keypair (Q2).
// Test implementation returns a deterministic stub.
type EnvelopeSigner interface {
	Sign(envelope *domain.IdentityContextEnvelope) (string, error)
}
