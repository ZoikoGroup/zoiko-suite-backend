// Package registry defines the service-level interfaces for tenant-entity-registry-svc.
//
// All dependencies — data store, event publisher, authorization, and jurisdiction
// validation — are expressed as interfaces so that production implementations
// can be wired at startup and test implementations swapped without infrastructure.
package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// ---------------------------------------------------------------------------
// Store — authoritative data-access contract.
//
// One method = one bounded read or write. Implementations must be idempotent
// where the method is called from a state-changing API path.
// ---------------------------------------------------------------------------

// Store is the data-access contract for all objects owned by this service.
type Store interface {
	// ── Tenant ──────────────────────────────────────────────────────────────

	CreateTenant(ctx context.Context, t *domain.Tenant) error
	// CreateTenantWithDefaultResidencyPolicy inserts a tenant and its default
	// DataResidencyPolicy in a single transaction. tenants.default_data_residency_policy_id
	// is NOT NULL, but data_residency_policies.tenant_id has a FK back to tenants —
	// so the policy cannot exist before the tenant, and the tenant cannot reference
	// a policy that doesn't exist yet. This method breaks that cycle by inserting
	// both rows atomically: the tenant first, then the policy that references it.
	CreateTenantWithDefaultResidencyPolicy(ctx context.Context, t *domain.Tenant, p *domain.DataResidencyPolicy) error
	GetTenantByID(ctx context.Context, tenantID string) (*domain.Tenant, error)
	TransitionTenantLifecycle(ctx context.Context, tenantID string, newState domain.TenantLifecycleState, actorID, correlationID string) error

	// ── LegalEntity ─────────────────────────────────────────────────────────

	CreateEntity(ctx context.Context, e *domain.LegalEntity) error
	GetEntityByID(ctx context.Context, legalEntityID string) (*domain.LegalEntity, error)
	ListEntitiesByTenant(ctx context.Context, tenantID string) ([]*domain.LegalEntity, error)
	UpdateEntity(ctx context.Context, legalEntityID string, req domain.UpdateEntityRequest) (*domain.LegalEntity, error)
	// TransitionEntityStatus atomically applies an entity_status transition.
	// The UPDATE uses WHERE entity_status = ANY($allowedPriorStates) so the state
	// machine check and the write are a single atomic statement — no separate read
	// needed, no race window. Returns (rowsAffected, tenantID, error).
	// rowsAffected == 0 means entity not found or state not in allowed set.
	// tenantID is returned via RETURNING for event publishing without a second query.
	TransitionEntityStatus(ctx context.Context, legalEntityID string, newStatus domain.EntityStatus, allowedPriorStates []domain.EntityStatus, actorID, correlationID string) (int64, string, error)
	GetEntityStatus(ctx context.Context, legalEntityID string) (*domain.EntityStatusResponse, error)

	// ── EntityHierarchy ─────────────────────────────────────────────────────

	CreateHierarchy(ctx context.Context, h *domain.EntityHierarchy) error
	EndDateHierarchy(ctx context.Context, hierarchyID string, endDate time.Time, actorID, correlationID string) error
	ListHierarchiesByEntity(ctx context.Context, legalEntityID string) ([]*domain.EntityHierarchy, error)

	// ── EntityJurisdictionAssignment ────────────────────────────────────────

	CreateJurisdictionAssignment(ctx context.Context, a *domain.EntityJurisdictionAssignment) error
	ListJurisdictionAssignments(ctx context.Context, legalEntityID string) ([]*domain.EntityJurisdictionAssignment, error)
	EndDateJurisdictionAssignment(ctx context.Context, assignmentID string, endDate time.Time, actorID, correlationID string) error

	// ── DataResidencyPolicy ─────────────────────────────────────────────────

	CreateResidencyPolicy(ctx context.Context, p *domain.DataResidencyPolicy) error
	GetResidencyPolicyByID(ctx context.Context, policyID string) (*domain.DataResidencyPolicy, error)

	// ── ResidencyRegion (read-only — IaC-managed) ───────────────────────────

	GetResidencyRegionByID(ctx context.Context, regionID string) (*domain.ResidencyRegion, error)
	ListResidencyRegions(ctx context.Context) ([]*domain.ResidencyRegion, error)

	// ── TaxIdentityBundle ───────────────────────────────────────────────────

	CreateTaxIdentityBundle(ctx context.Context, b *domain.TaxIdentityBundle) error
	GetTaxIdentityBundleByID(ctx context.Context, bundleID string) (*domain.TaxIdentityBundle, error)
	ListTaxIdentityBundlesByEntity(ctx context.Context, legalEntityID string) ([]*domain.TaxIdentityBundle, error)
	// TransitionTaxIdentityBundleStatus applies a status transition on a bundle header.
	// Must be idempotent.
	TransitionTaxIdentityBundleStatus(ctx context.Context, bundleID string, newStatus domain.TaxIdentityBundleStatus, actorID, correlationID string) error
}

// ---------------------------------------------------------------------------
// EventPublisher — append-only domain event publishing contract.
// ---------------------------------------------------------------------------

// EventPublisher emits append-only domain events to the event backbone.
// All publish calls are fire-and-forget from the service's perspective.
// DB writes are NOT rolled back on publish failure — an outbox pattern
// handles redelivery.
type EventPublisher interface {
	PublishTenantCreated(ctx context.Context, tenant *domain.Tenant, correlationID string)
	PublishEntityCreated(ctx context.Context, entity *domain.LegalEntity, correlationID string)
	PublishEntityUpdated(ctx context.Context, entity *domain.LegalEntity, correlationID string)
	PublishEntityStatusChanged(ctx context.Context, tenantID, legalEntityID string, previousStatus, newStatus domain.EntityStatus, correlationID string)
	PublishEntityHierarchyChanged(ctx context.Context, hierarchy *domain.EntityHierarchy, changeType string, correlationID string)
	PublishEntityJurisdictionChanged(ctx context.Context, assignment *domain.EntityJurisdictionAssignment, changeType string, correlationID string)
}

// ---------------------------------------------------------------------------
// AuthorizationClient — governance plane dependency.
//
// Per doctrine: no domain service self-authorizes a material action.
// Every mutating API call must receive an authorization decision before
// execution proceeds. If the Authorization Service is unreachable the
// call MUST be rejected (fail-closed).
// ---------------------------------------------------------------------------

// AuthorizationClient is the contract for authorizing mutations.
// The concrete type lives in internal/authz and satisfies this interface.
type AuthorizationClient interface {
	// Authorize returns nil if the action is permitted.
	// Returns authz.ErrUnauthorized if denied.
	// Returns authz.ErrAuthZUnavailable if service unreachable — callers fail-closed.
	Authorize(ctx context.Context, envelopeJWT, resource, action string) error
}

// ---------------------------------------------------------------------------
// JurisdictionValidator — jurisdiction existence check.
//
// Q2 resolution: synchronously validated on assignment creation, fail-closed.
// ---------------------------------------------------------------------------

// JurisdictionValidator is the contract for jurisdiction existence validation.
// The concrete type lives in internal/jurisdiction.
type JurisdictionValidator interface {
	// ValidateExists returns nil if the jurisdiction_id is known and active.
	// Returns jurisdiction.ErrJurisdictionNotFound if the ID does not exist.
	// Returns jurisdiction.ErrValidatorUnavailable if the service is unreachable — callers fail-closed.
	ValidateExists(ctx context.Context, jurisdictionID string) error
}
