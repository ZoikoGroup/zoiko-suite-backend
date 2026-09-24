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
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
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

	// ── Workspace ───────────────────────────────────────────────────────────

	CreateWorkspace(ctx context.Context, w *domain.Workspace) error
	GetWorkspaceByID(ctx context.Context, workspaceID string) (*domain.Workspace, error)
	ListWorkspacesByTenant(ctx context.Context, tenantID string) ([]*domain.Workspace, error)
	UpdateWorkspace(ctx context.Context, workspaceID string, req domain.UpdateWorkspaceRequest) (*domain.Workspace, error)
	// TransitionWorkspaceStatus applies an archive or restore as a single
	// guarded UPDATE, the same shape as TransitionEntityStatus.
	// Returns (rowsAffected, previousStatus, error). rowsAffected == 0 means the
	// workspace is absent or its current status was not in allowedPriorStates.
	// previousStatus is read in the same statement, so a consumer of the event
	// learns what the workspace moved away from.
	TransitionWorkspaceStatus(ctx context.Context, workspaceID string, newStatus domain.WorkspaceStatus, allowedPriorStates []domain.WorkspaceStatus, actorID, correlationID string) (int64, domain.WorkspaceStatus, error)

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

	// ORG-02/ORG-03 surfaces — see ORGStore at the bottom of this file.
	ORGStore
	ApprovalStore
	ORGGapStore
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
	PublishEntityStatusChanged(ctx context.Context, tenantID, legalEntityID, actorID string, previousStatus, newStatus domain.EntityStatus, correlationID string)
	PublishEntityHierarchyChanged(ctx context.Context, hierarchy *domain.EntityHierarchy, changeType string, correlationID string)
	PublishEntityJurisdictionChanged(ctx context.Context, assignment *domain.EntityJurisdictionAssignment, changeType string, correlationID string)
	PublishWorkspaceCreated(ctx context.Context, workspace *domain.Workspace, correlationID string)
	PublishWorkspaceUpdated(ctx context.Context, workspace *domain.Workspace, correlationID string)
	PublishWorkspaceStatusChanged(ctx context.Context, tenantID, workspaceID, actorID string, previousStatus, newStatus domain.WorkspaceStatus, correlationID string)
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
	//
	// principalID is the gateway-verified acting principal and scopeID the
	// tenant the decision is evaluated within. This used to take the raw
	// envelope JWT, which the service then decoded itself without verifying
	// the signature — so the subject of the decision was caller-chosen.
	Authorize(ctx context.Context, principalID, scopeID, resource, action string) error
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

// ---------------------------------------------------------------------------
// ORG-02 / ORG-03 store contract
//
// Added for the Organization / Legal Entity specification §4.2, §4.3, §8 and
// §9.2. Kept as its own interface, embedded into Store below, for two reasons:
// the ORG surface is coherent on its own and reads as one thing, and a reader
// asking "what did the ORG completion add" gets an answer without diffing.
// ---------------------------------------------------------------------------

// TenantCommandParams is one named ORG-02 lifecycle command, ready to apply.
type TenantCommandParams struct {
	TenantID    string
	Command     domain.TenantCommand
	TargetState domain.TenantLifecycleState
	// AllowedFrom are the lifecycle states this command may be invoked from.
	// Passed to the store rather than checked before it so the state-machine
	// test and the write are a single atomic statement — the same race-free
	// shape TransitionEntityStatus already uses.
	AllowedFrom []domain.TenantLifecycleState
	// ExpectedVersion is always non-zero by the time it reaches the store: the
	// service substitutes the version it read when the caller supplied none,
	// which turns its read-then-write into a compare-and-swap.
	ExpectedVersion int64
	Reason          string
	ActorID         string
	ApprovedBy      string
	CorrelationID   string
	// Approval, when set, is the verified decision that released this
	// command. The store marks it APPROVED in the same transaction as the
	// lifecycle change, guarded on the request still being PENDING and
	// unexpired, so two approvers racing cannot both release it.
	Approval *domain.ApprovalDecision
}

// TenantCommandResult reports what a successful command did.
type TenantCommandResult struct {
	FromState  domain.TenantLifecycleState `json:"from_state"`
	ToState    domain.TenantLifecycleState `json:"to_state"`
	NewVersion int64                       `json:"record_version"`
	// Status is the tenant's status column after the command. Suspension moves
	// it in step with lifecycle_state, because a SUSPENDED tenant whose status
	// still reads ACTIVE is exactly the inconsistency §8 NP4 turns on.
	Status domain.TenantStatus `json:"status"`
}

// ORGStore is the data-access contract for the ORG-02/ORG-03 surfaces.
//
// Every write here takes an *outbox.Record and is responsible for writing it
// in the SAME transaction as the business fact. A nil record means "no event",
// which is legitimate; an implementation that accepts a non-nil record and
// does not write it transactionally is not implementing this interface.
type ORGStore interface {
	// ── ORG-02: named commands ──────────────────────────────────────────────

	ExecuteTenantCommand(ctx context.Context, p TenantCommandParams, ev *outbox.Record) (*TenantCommandResult, error)
	ChangeDefaultLocale(ctx context.Context, tenantID, locale, timezone, reason, actorID, correlationID string, expectedVersion int64, ev *outbox.Record) (*domain.Tenant, error)

	// ── ORG-02: read surfaces ───────────────────────────────────────────────

	ListTenantLifecycleHistory(ctx context.Context, tenantID string) ([]*domain.TenantLifecycleEvent, error)
	GetTenantDefaults(ctx context.Context, tenantID string) (*domain.TenantDefaults, error)

	// ── ORG-02: host bindings (ResolveTenantByHost, §8 NP3) ─────────────────

	BindTenantHost(ctx context.Context, b *domain.TenantHostBinding) error
	// ResolveTenantByHost is deliberately NOT tenant-scoped: it is the lookup
	// that establishes which tenant a request belongs to. Returns (nil, nil)
	// for an unknown hostname.
	ResolveTenantByHost(ctx context.Context, hostname string) (*domain.ResolvedTenantByHost, error)
	ListTenantHostBindings(ctx context.Context, tenantID string) ([]*domain.TenantHostBinding, error)

	// ── ORG-03: profile versions ────────────────────────────────────────────

	CreateInitialProfileVersion(ctx context.Context, v *domain.LegalEntityProfileVersion) error
	AmendLegalProfile(ctx context.Context, legalEntityID string, next *domain.LegalEntityProfileVersion, expectedVersion int64, ev *outbox.Record) (*domain.LegalEntityProfileVersion, error)
	ListEntityProfileVersions(ctx context.Context, legalEntityID string) ([]*domain.LegalEntityProfileVersion, error)
	GetEntityProfileAsOf(ctx context.Context, legalEntityID string, asOf time.Time) (*domain.EntityAsOf, error)
	FindEntitiesByRegistryNumber(ctx context.Context, registrationNumber, jurisdictionID string) ([]*domain.LegalEntity, error)

	// ── ORG-03: registry conflict quarantine (§8 NP5) ───────────────────────

	// FindActiveEntityByRegistry returns the ACTIVE entity already holding this
	// registry identity in this jurisdiction, or (nil, nil) if there is none.
	FindActiveEntityByRegistry(ctx context.Context, registrationNumber, jurisdictionID string) (*domain.LegalEntity, error)
	RecordRegistryConflict(ctx context.Context, c *domain.EntityRegistryConflict) error
	ListRegistryConflicts(ctx context.Context, openOnly bool) ([]*domain.EntityRegistryConflict, error)
	ResolveRegistryConflict(ctx context.Context, conflictID string, status domain.RegistryConflictStatus, note, actorID string) error
	// GetRegistryConflict returns one conflict, or (nil, nil) if absent.
	GetRegistryConflict(ctx context.Context, conflictID string) (*domain.EntityRegistryConflict, error)
	// ResolveRegistryConflictApproved records a resolution released by an
	// independent approval, in the same transaction as the approval decision.
	ResolveRegistryConflictApproved(ctx context.Context, conflictID string, status domain.RegistryConflictStatus, note, resolvedBy string, d domain.ApprovalDecision) error
}

// ---------------------------------------------------------------------------
// Approval store — verified maker-checker (ORG-02 §4.2, ORG-03 §4.3)
// ---------------------------------------------------------------------------

// ApprovalStore is the data-access contract for approval requests.
type ApprovalStore interface {
	// CreateApprovalRequest files a PENDING request. Any PENDING request for
	// the same subject whose TTL has passed is marked EXPIRED first, in the
	// same transaction, so an abandoned proposal cannot block the subject
	// forever. A live PENDING request for the subject returns ErrApprovalPending.
	CreateApprovalRequest(ctx context.Context, a *domain.ApprovalRequest) error
	// GetApprovalRequest returns one request, or (nil, nil) if absent.
	GetApprovalRequest(ctx context.Context, approvalRequestID string) (*domain.ApprovalRequest, error)
	// ListApprovalRequests lists the caller's tenant's requests, newest first.
	// pendingOnly excludes decided and expired ones.
	ListApprovalRequests(ctx context.Context, pendingOnly bool) ([]*domain.ApprovalRequest, error)
	// LatestApprovalForSubject returns the most recent request for a subject,
	// or (nil, nil) if there has never been one.
	LatestApprovalForSubject(ctx context.Context, subjectType domain.ApprovalSubjectType, subjectID string) (*domain.ApprovalRequest, error)
	// DecideApprovalRequest moves a PENDING request to status on its own —
	// for REJECTED, STALE and EXPIRED, and for APPROVED where approval
	// releases no further write (tenant creation). Returns
	// ErrApprovalNotPending if the request is no longer PENDING (or, for
	// APPROVED, has expired).
	DecideApprovalRequest(ctx context.Context, d domain.ApprovalDecision, status domain.ApprovalStatus) error
}

// ---------------------------------------------------------------------------
// ORG gap store — onboarding idempotency, FailedProvisioning, entity
// verification and non-destructive merge (migration 000008)
// ---------------------------------------------------------------------------

// ProvisioningCompletion is the follow-on provisioning step: filing the
// creation approval and enqueueing tenant.created, in one transaction.
type ProvisioningCompletion struct {
	TenantID string
	Approval *domain.ApprovalRequest
	Event    *outbox.Record
	// FromFailed is RetryProvisioning: the same step, plus moving the tenant
	// FAILED_PROVISIONING → ONBOARDING and recording the command, atomically.
	FromFailed      bool
	ExpectedVersion int64
	ActorID         string
	Reason          string
	CorrelationID   string
}

// EntityVerification is an approved DRAFT → VERIFIED transition.
type EntityVerification struct {
	LegalEntityID   string
	VerifiedBy      string
	EvidenceRef     string
	ExpectedVersion int64
	Decision        domain.ApprovalDecision
}

// ORGGapStore is the data-access contract for the 000008 surfaces.
type ORGGapStore interface {
	// ResolveOnboardingKey returns the tenant a key produced and the request
	// fingerprint it was produced from, or ("", "", nil) for an unused key.
	// Not tenant-scoped: a replay does not know its tenant.
	ResolveOnboardingKey(ctx context.Context, key string) (tenantID, fingerprint string, err error)
	CompleteProvisioning(ctx context.Context, p ProvisioningCompletion) error
	// MarkProvisioningFailed moves an ONBOARDING tenant to FAILED_PROVISIONING
	// and records why.
	MarkProvisioningFailed(ctx context.Context, tenantID, reason, actorID string) error

	VerifyLegalEntity(ctx context.Context, v EntityVerification, ev *outbox.Record) error
	ActivateLegalEntity(ctx context.Context, legalEntityID, actorID string, expectedVersion int64, ev *outbox.Record) error

	MergeEntities(ctx context.Context, m *domain.EntityMergeRecord, d domain.ApprovalDecision, expectedVersion int64, ev *outbox.Record) error
	UnmergeEntity(ctx context.Context, duplicateID, unmergedBy, reason string, d domain.ApprovalDecision, expectedVersion int64, ev *outbox.Record) error
	ListEntityMergeRecords(ctx context.Context, legalEntityID string) ([]*domain.EntityMergeRecord, error)
}
