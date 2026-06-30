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
	FindByID(ctx context.Context, principalID string) (*domain.Principal, error)
	FindActiveRoleAssignments(ctx context.Context, principalID string, legalEntityID *string) ([]domain.PrincipalRoleAssignment, error)
	FindActiveDelegations(ctx context.Context, principalID string) ([]domain.DelegatedAuthority, error)
	UpdateStatus(ctx context.Context, principalID string, newStatus domain.PrincipalStatus, actorID, correlationID string) error
}

// SessionCache manages the Redis-backed session envelope store.
// SessionContext records are NEVER deleted from the backing store.
// invalidated_at is appended (append-only evidence obligation).
type SessionCache interface {
	Put(ctx context.Context, sessionContextID, envelopeJWT string) error
	Get(ctx context.Context, sessionContextID string) (string, error)
	Evict(ctx context.Context, sessionContextID string) error
	PersistSessionContext(ctx context.Context, sc domain.SessionContext) error
	GetSessionContext(ctx context.Context, sessionContextID string) (*domain.SessionContext, error)
	Invalidate(ctx context.Context, sessionContextID string, reason domain.InvalidationReason, at time.Time) error
	EvictAllForPrincipal(ctx context.Context, principalID string) error
}

// RiskSignalCache provides READ-ONLY access to asynchronously-populated
// risk signals.
//
// ARCHITECTURAL INVARIANT (Q3 resolution):
//   Resolve() must never call the Intelligence Plane or any Tier 2/3 service
//   synchronously. Risk scores arrive via an async consumer that writes into
//   Redis. This interface only exposes reads — writes are on a separate path.
type RiskSignalCache interface {
	GetLatestSignal(ctx context.Context, principalID string) (*domain.RiskSignalCache, error)
}

// UpstreamRegistry is a read-only client to upstream Tier 0 services.
// This service never writes to any upstream domain.
type UpstreamRegistry interface {
	IsTenantActive(ctx context.Context, tenantID string) (bool, error)
	IsPrincipalAuthorizedForEntity(ctx context.Context, principalID, legalEntityID string) (bool, error)
	ResolvePermissionBundles(ctx context.Context, roleIDs []string) ([]string, error)
	FetchActiveDelegations(ctx context.Context, principalID, legalEntityID string) ([]domain.DelegatedAuthority, error)
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
