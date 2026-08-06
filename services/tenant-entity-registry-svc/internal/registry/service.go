// Package registry implements the core business logic for tenant-entity-registry-svc.
//
// Doctrine invariants enforced here:
//   - No service self-authorizes. Every mutation calls AuthorizationClient first.
//   - Every state-changing operation is idempotent.
//   - No soft-delete. Status transitions, effective end-dating only.
//   - jurisdiction_id references validated synchronously (fail-closed) against
//     the Jurisdiction Rules Service via JurisdictionValidator.
//   - entity.status.changed event published on every EntityStatus transition.
//   - ResidencyRegion writes are IaC-only; service exposes read endpoint only.
package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/classification"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/jurisdiction"
)

// Sentinel errors returned by the service layer.
// Handlers map these to appropriate HTTP status codes.
var (
	ErrNotFound          = errors.New("not found")
	ErrInvalidTransition = errors.New("invalid status transition")
	ErrUnauthorized      = errors.New("unauthorized")
	// ErrUnauthenticated is returned when a mutation arrives with no verified
	// principal. Distinct from ErrUnauthorized: the caller has not been
	// identified at all, so there is nothing to evaluate a grant against.
	// Handlers map this to 401, not 403.
	ErrUnauthenticated    = errors.New("unauthenticated: no verified principal")
	ErrServiceUnavailable = errors.New("upstream service unavailable")
	ErrInvalidInput       = errors.New("invalid input")
	// ErrConflict is returned when a unique constraint is violated (e.g. duplicate
	// tenant_code). Handlers should map this to HTTP 409 Conflict.
	ErrConflict = errors.New("conflict: resource already exists")
	// ErrRegionUnresolved is returned by ResolveTenantRegion when the
	// tenant exists but its active residency policy has no
	// ResidencyRegionID assigned yet — a real, expected state for
	// policies created before that column existed (migration 000003),
	// not a bug. Handlers should map this to HTTP 409 Conflict, distinct
	// from ErrNotFound.
	ErrRegionUnresolved = errors.New("tenant's residency policy has no region assigned")
)

// Service orchestrates all registry operations.
// It owns no HTTP concerns — those belong to internal/handler.
type Service struct {
	store  Store
	events EventPublisher
	authz  AuthorizationClient
	jurisd JurisdictionValidator

	// platformScopeID is the authorization scope used for operations that have
	// no tenant yet — in practice only ProvisionTenant. authorization-svc
	// rejects an empty legal_entity_id, so tenant creation is evaluated
	// against one synthetic platform-scope entity. Role assignments granting
	// TENANT_PROVISION must be made against this same ID.
	platformScopeID string

	log *zap.Logger
}

// NewService constructs a Service with all required dependencies.
func NewService(
	store Store,
	events EventPublisher,
	authz AuthorizationClient,
	jurisd JurisdictionValidator,
	platformScopeID string,
	log *zap.Logger,
) *Service {
	return &Service{
		store:           store,
		events:          events,
		authz:           authz,
		jurisd:          jurisd,
		platformScopeID: platformScopeID,
		log:             log,
	}
}

// ---------------------------------------------------------------------------
// Tenant operations
// ---------------------------------------------------------------------------

// ProvisionTenant creates a new tenant in ONBOARDING lifecycle state.
// Idempotent: if the tenant already exists the store returns a duplicate error
// which the caller surfaces as a conflict.
//
// A default DataResidencyPolicy is always generated here, rather than taken
// from the caller — data_residency_policies.tenant_id has a FK to tenants,
// so no policy can exist before this tenant does, meaning a client can never
// legitimately supply a pre-existing policy ID at provisioning time. The
// policy starts with no ResidencyRegionID and ConflictResolutionFailClosed;
// an operator assigns a concrete region afterward via the residency-policies
// API.
func (s *Service) ProvisionTenant(
	ctx context.Context,
	req domain.ProvisionTenantRequest,
	correlationID string,
) (*domain.Tenant, error) {
	if err := s.authorize(ctx, "tenant", "provision"); err != nil {
		return nil, err
	}

	tenantID := newID()
	policyID := newID()
	now := time.Now().UTC()
	actor := domain.PrincipalFromContext(ctx)

	t := &domain.Tenant{
		TenantID:                     tenantID,
		TenantCode:                   req.TenantCode,
		LegalName:                    req.LegalName,
		TradingName:                  nullableString(req.TradingName),
		Status:                       domain.TenantStatusActive,
		DefaultCurrencyCode:          req.DefaultCurrencyCode,
		PrimaryTimezone:              req.PrimaryTimezone,
		PrimaryLocale:                req.PrimaryLocale,
		DefaultDataResidencyPolicyID: policyID,
		LifecycleState:               domain.TenantLifecycleOnboarding,
		CreatedAt:                    now,
		CreatedByPrincipalID:         actor,
	}

	defaultPolicy := &domain.DataResidencyPolicy{
		DataResidencyPolicyID:  policyID,
		TenantID:               tenantID,
		PolicyName:             req.LegalName + " - Default Residency Policy",
		PolicyCode:             req.TenantCode + "-DEFAULT",
		ResidencyMode:          domain.ResidencyModePreferredRegion,
		ConflictResolutionMode: domain.ConflictResolutionFailClosed,
		ActiveFlag:             true,
		CreatedAt:              now,
		CreatedByPrincipalID:   actor,
	}

	if err := s.store.CreateTenantWithDefaultResidencyPolicy(ctx, t, defaultPolicy); err != nil {
		s.log.Error("create tenant failed", zap.Error(err), zap.String("correlation_id", correlationID))
		return nil, fmt.Errorf("store.CreateTenantWithDefaultResidencyPolicy: %w", err)
	}

	go s.events.PublishTenantCreated(ctx, t, correlationID)

	s.log.Info("tenant provisioned",
		zap.String("tenant_id", t.TenantID),
		zap.String("correlation_id", correlationID),
	)
	return t, nil
}

// assertTenantScope refuses a request whose path tenant is not the caller's
// own verified tenant.
//
// Row-level security was supposed to make this unnecessary, and it does not.
// Verified live on 2026-08-05: GET /v1/tenants/{A}/entities with
// X-Tenant-Id: B returned tenant A's entities in full. Two independent
// reasons, both confirmed against the running database:
//
//   - The service connects as the Postgres superuser (pg_user.usesuper = t),
//     and superusers bypass RLS unconditionally, whatever the policy says.
//   - The tables are owned by that same user and were created with ENABLE ROW
//     LEVEL SECURITY, not FORCE (pg_class.relforcerowsecurity = f), so the
//     owner bypasses them too.
//
// The policies themselves are present and correctly written — they simply
// never execute. internal/store/tenant_isolation_test.go documents the same
// trap and covers the store methods that take a tenant filter; what it cannot
// cover is this layer, where the tenant comes from the URL rather than from
// the caller's identity. A query filtered by a caller-supplied path parameter
// is not an isolation boundary, however correct its WHERE clause.
//
// ErrNotFound rather than ErrUnauthorized is deliberate: a cross-tenant probe
// must not be able to tell "exists but forbidden" from "does not exist", or
// the 403 itself confirms the tenant.
func (s *Service) assertTenantScope(ctx context.Context, tenantID string) error {
	callerTenant := domain.TenantFromContext(ctx)
	if callerTenant == "" || tenantID == "" {
		// No verified tenant on the request. ProvisionTenant is the only
		// legitimate case and it does not route through here; anything else
		// reaching this point cannot be scoped, so it is refused.
		return ErrNotFound
	}
	if callerTenant != tenantID {
		s.log.Warn("cross-tenant read refused",
			zap.String("caller_tenant", callerTenant),
			zap.String("requested_tenant", tenantID),
		)
		return ErrNotFound
	}
	return nil
}

// GetTenant retrieves a tenant by ID. Returns ErrNotFound if absent, or if the
// caller's verified tenant is not this one.
func (s *Service) GetTenant(ctx context.Context, tenantID string) (*domain.Tenant, error) {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	t, err := s.store.GetTenantByID(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store.GetTenantByID: %w", err)
	}
	if t == nil {
		return nil, ErrNotFound
	}
	return t, nil
}

// TransitionTenantLifecycle moves a tenant through its lifecycle state machine.
// Invalid transitions are rejected fail-closed.
func (s *Service) TransitionTenantLifecycle(
	ctx context.Context,
	tenantID string,
	req domain.TransitionTenantLifecycleRequest,
) error {
	if err := s.authorize(ctx, "tenant", "lifecycle.transition"); err != nil {
		return err
	}

	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return err
	}

	if !isValidTenantTransition(t.LifecycleState, req.TargetState) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.LifecycleState, req.TargetState)
	}

	return s.store.TransitionTenantLifecycle(ctx, tenantID, req.TargetState, domain.PrincipalFromContext(ctx), req.CorrelationID)
}

// ---------------------------------------------------------------------------
// LegalEntity operations
// ---------------------------------------------------------------------------

// CreateEntity creates a new LegalEntity under an existing tenant.
// primary_jurisdiction_id is validated synchronously against the Jurisdiction
// Rules Service before persistence (Q2 resolution — fail-closed).
func (s *Service) CreateEntity(
	ctx context.Context,
	req domain.CreateEntityRequest,
) (*domain.LegalEntity, error) {
	if err := s.authorize(ctx, "entity", "create"); err != nil {
		return nil, err
	}

	// Synchronous jurisdiction validation — fail-closed per Q2 resolution.
	if err := s.jurisd.ValidateExists(ctx, req.PrimaryJurisdictionID); err != nil {
		return nil, s.mapJurisdictionErr(err, req.PrimaryJurisdictionID)
	}

	e := &domain.LegalEntity{
		LegalEntityID:         newID(),
		TenantID:              req.TenantID,
		EntityCode:            req.EntityCode,
		LegalName:             req.LegalName,
		TradingName:           nullableString(req.TradingName),
		EntityType:            req.EntityType,
		DefaultCurrencyCode:   req.DefaultCurrencyCode,
		FiscalCalendarID:      req.FiscalCalendarID,
		PrimaryJurisdictionID: req.PrimaryJurisdictionID,
		EntityStatus:          domain.EntityStatusActive,
		DataResidencyPolicyID: req.DataResidencyPolicyID,
		CreatedAt:             time.Now().UTC(),
		CreatedByPrincipalID:  domain.PrincipalFromContext(ctx),
	}

	if err := s.store.CreateEntity(ctx, e); err != nil {
		s.log.Error("create entity failed", zap.Error(err), zap.String("correlation_id", req.CorrelationID))
		return nil, fmt.Errorf("store.CreateEntity: %w", err)
	}

	go s.events.PublishEntityCreated(ctx, e, req.CorrelationID)

	s.log.Info("entity created",
		zap.String("legal_entity_id", e.LegalEntityID),
		zap.String("tenant_id", e.TenantID),
		zap.String("correlation_id", req.CorrelationID),
	)
	return e, nil
}

// GetEntity retrieves a legal entity by ID. Returns ErrNotFound if absent.
func (s *Service) GetEntity(ctx context.Context, legalEntityID string) (*domain.LegalEntity, error) {
	e, err := s.store.GetEntityByID(ctx, legalEntityID)
	if err != nil {
		return nil, fmt.Errorf("store.GetEntityByID: %w", err)
	}
	if e == nil {
		return nil, ErrNotFound
	}
	return e, nil
}

// ListEntities returns all legal entities for a tenant.
func (s *Service) ListEntities(ctx context.Context, tenantID string) ([]*domain.LegalEntity, error) {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	entities, err := s.store.ListEntitiesByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	// Never null — a caller iterating the JSON array must not get a null
	// where an empty list is meant. Verified live: this endpoint returned
	// `null` rather than `[]` for a tenant with no entities.
	if entities == nil {
		entities = []*domain.LegalEntity{}
	}
	return entities, nil
}

// UpdateEntity applies a partial update to a legal entity.
// Only mutable non-governance fields may be patched (legal_name, trading_name, currency).
func (s *Service) UpdateEntity(
	ctx context.Context,
	legalEntityID string,
	req domain.UpdateEntityRequest,
) (*domain.LegalEntity, error) {
	if err := s.authorize(ctx, "entity", "update"); err != nil {
		return nil, err
	}

	// Populate audit actor from the verified envelope JWT.
	// actorFromJWT performs payload-only decoding — signature is already
	// verified by the Authorization Service before this service is called.
	req.ActorPrincipalID = domain.PrincipalFromContext(ctx)

	e, err := s.store.UpdateEntity(ctx, legalEntityID, req)
	if err != nil {
		return nil, fmt.Errorf("store.UpdateEntity: %w", err)
	}

	go s.events.PublishEntityUpdated(ctx, e, req.CorrelationID)
	return e, nil
}

// GetEntityStatus is the lightweight status probe endpoint.
// GET /v1/entities/{entityID}/status — renamed per approved answers.
// Consumers that need live status without a full entity fetch call this endpoint.
func (s *Service) GetEntityStatus(ctx context.Context, legalEntityID string) (*domain.EntityStatusResponse, error) {
	resp, err := s.store.GetEntityStatus(ctx, legalEntityID)
	if err != nil {
		return nil, fmt.Errorf("store.GetEntityStatus: %w", err)
	}
	if resp == nil {
		return nil, ErrNotFound
	}
	return resp, nil
}

// TransitionEntityStatus atomically applies an entity_status state-machine
// transition and publishes entity.status.changed.
//
// Race-free design: rather than reading current state then writing (two
// transactions, race window), we pass the set of valid prior states to the
// store and perform a single UPDATE WHERE entity_status = ANY($priors).
// If zero rows are affected, either the entity doesn't exist or the current
// state was not in the valid prior set — both map to ErrInvalidTransition.
// No SELECT FOR UPDATE, no serializable isolation needed — the atomicity
// is structural.
//
// Idempotent: newStatus is included in allowedPriorStates only when the
// transition target equals itself (no-op path returns 0 rows; service treats
// 0 rows as a no-op when newStatus == target, see below).
func (s *Service) TransitionEntityStatus(
	ctx context.Context,
	legalEntityID string,
	req domain.TransitionEntityStatusRequest,
) error {
	if err := s.authorize(ctx, "entity", "status.transition"); err != nil {
		return err
	}

	// Compute the set of valid prior states for the requested target transition.
	// ValidEntityStatusTransitions maps FROM → []TO; we need all states that
	// can transition TO req.NewStatus.
	var allowedPriors []domain.EntityStatus
	for fromState, targets := range domain.ValidEntityStatusTransitions {
		for _, t := range targets {
			if t == req.NewStatus {
				allowedPriors = append(allowedPriors, fromState)
				break
			}
		}
	}
	// Include the target status itself so an idempotent re-apply (same → same)
	// succeeds with 0 rows affected and is treated as a no-op below.
	allowedPriors = append(allowedPriors, req.NewStatus)

	affected, tenantID, err := s.store.TransitionEntityStatus(
		ctx, legalEntityID, req.NewStatus, allowedPriors,
		domain.PrincipalFromContext(ctx), req.CorrelationID,
	)
	if err != nil {
		return fmt.Errorf("store.TransitionEntityStatus: %w", err)
	}

	if affected == 0 {
		// Could be: entity not found, or current state not in allowedPriors.
		// Both are indistinguishable without a separate read — we surface as
		// ErrInvalidTransition per the contract (callers should pre-check
		// existence via GetEntity before calling this).
		return fmt.Errorf("%w: entity %s cannot transition to %s from its current state",
			ErrInvalidTransition, legalEntityID, req.NewStatus)
	}

	// Publish entity.status.changed — approved event name per Q4 resolution.
	// tenantID is returned by the store from the updated row (RETURNING clause).
	go s.events.PublishEntityStatusChanged(
		ctx,
		tenantID,
		legalEntityID,
		domain.EntityStatus(""), // previous state not known without a read — omit
		req.NewStatus,
		req.CorrelationID,
	)

	s.log.Info("entity status transitioned",
		zap.String("legal_entity_id", legalEntityID),
		zap.String("to", string(req.NewStatus)),
		zap.String("correlation_id", req.CorrelationID),
	)
	return nil
}

// ---------------------------------------------------------------------------
// EntityHierarchy operations
// ---------------------------------------------------------------------------

// CreateHierarchy establishes an effective-dated parent-child entity relationship.
func (s *Service) CreateHierarchy(
	ctx context.Context,
	req domain.CreateHierarchyRequest,
) (*domain.EntityHierarchy, error) {
	if err := s.authorize(ctx, "entity.hierarchy", "create"); err != nil {
		return nil, err
	}

	h := &domain.EntityHierarchy{
		HierarchyID:          newID(),
		TenantID:             req.TenantID,
		ParentLegalEntityID:  req.ParentLegalEntityID,
		ChildLegalEntityID:   req.ChildLegalEntityID,
		RelationshipType:     req.RelationshipType,
		EffectiveFrom:        req.EffectiveFrom,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: domain.PrincipalFromContext(ctx),
	}

	if err := s.store.CreateHierarchy(ctx, h); err != nil {
		return nil, fmt.Errorf("store.CreateHierarchy: %w", err)
	}

	go s.events.PublishEntityHierarchyChanged(ctx, h, "CREATED", req.CorrelationID)
	return h, nil
}

// EndDateHierarchy closes an entity hierarchy relationship by setting effective_to.
// No hard-delete per doctrine.
func (s *Service) EndDateHierarchy(
	ctx context.Context,
	hierarchyID string,
	endDate time.Time,
	correlationID string,
) error {
	if err := s.authorize(ctx, "entity.hierarchy", "end-date"); err != nil {
		return err
	}

	if err := s.store.EndDateHierarchy(ctx, hierarchyID, endDate, domain.PrincipalFromContext(ctx), correlationID); err != nil {
		return fmt.Errorf("store.EndDateHierarchy: %w", err)
	}

	// Emit a synthetic hierarchy object for the event; store provides full record if needed.
	go s.events.PublishEntityHierarchyChanged(ctx, &domain.EntityHierarchy{HierarchyID: hierarchyID, EffectiveTo: &endDate}, "END_DATED", correlationID)
	return nil
}

// ListHierarchies returns all effective-dated hierarchy records for an entity.
func (s *Service) ListHierarchies(ctx context.Context, legalEntityID string) ([]*domain.EntityHierarchy, error) {
	return s.store.ListHierarchiesByEntity(ctx, legalEntityID)
}

// ---------------------------------------------------------------------------
// EntityJurisdictionAssignment operations
// ---------------------------------------------------------------------------

// AssignJurisdiction creates a new jurisdiction assignment for a legal entity.
// jurisdiction_id is validated synchronously — fail-closed (Q2 resolution).
func (s *Service) AssignJurisdiction(
	ctx context.Context,
	legalEntityID string,
	req domain.AssignJurisdictionRequest,
) (*domain.EntityJurisdictionAssignment, error) {
	if err := s.authorize(ctx, "entity.jurisdiction", "assign"); err != nil {
		return nil, err
	}

	// Synchronous validation — fail-closed.
	if err := s.jurisd.ValidateExists(ctx, req.JurisdictionID); err != nil {
		return nil, s.mapJurisdictionErr(err, req.JurisdictionID)
	}

	a := &domain.EntityJurisdictionAssignment{
		AssignmentID:         newID(),
		TenantID:             domain.TenantFromContext(ctx),
		LegalEntityID:        legalEntityID,
		JurisdictionID:       req.JurisdictionID,
		AssignmentType:       req.AssignmentType,
		EffectiveFrom:        req.EffectiveFrom,
		SourceBasis:          req.SourceBasis,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: domain.PrincipalFromContext(ctx),
	}

	if err := s.store.CreateJurisdictionAssignment(ctx, a); err != nil {
		return nil, fmt.Errorf("store.CreateJurisdictionAssignment: %w", err)
	}

	go s.events.PublishEntityJurisdictionChanged(ctx, a, "ASSIGNED", req.CorrelationID)
	return a, nil
}

// ListJurisdictions returns all effective-dated jurisdiction assignments for an entity.
func (s *Service) ListJurisdictions(ctx context.Context, legalEntityID string) ([]*domain.EntityJurisdictionAssignment, error) {
	return s.store.ListJurisdictionAssignments(ctx, legalEntityID)
}

// EndDateJurisdictionAssignment closes a jurisdiction assignment.
// No hard-delete per doctrine.
func (s *Service) EndDateJurisdictionAssignment(
	ctx context.Context,
	assignmentID string,
	endDate time.Time,
	correlationID string,
) error {
	if err := s.authorize(ctx, "entity.jurisdiction", "end-date"); err != nil {
		return err
	}

	if err := s.store.EndDateJurisdictionAssignment(ctx, assignmentID, endDate, domain.PrincipalFromContext(ctx), correlationID); err != nil {
		return fmt.Errorf("store.EndDateJurisdictionAssignment: %w", err)
	}

	go s.events.PublishEntityJurisdictionChanged(
		ctx,
		&domain.EntityJurisdictionAssignment{AssignmentID: assignmentID, EffectiveTo: &endDate},
		"END_DATED",
		correlationID,
	)
	return nil
}

// ---------------------------------------------------------------------------
// DataResidencyPolicy operations
// ---------------------------------------------------------------------------

// CreateResidencyPolicy creates a data residency policy for a tenant.
func (s *Service) CreateResidencyPolicy(
	ctx context.Context,
	req domain.CreateResidencyPolicyRequest,
) (*domain.DataResidencyPolicy, error) {
	if err := s.authorize(ctx, "residency.policy", "create"); err != nil {
		return nil, err
	}

	p := &domain.DataResidencyPolicy{
		DataResidencyPolicyID:  newID(),
		TenantID:               req.TenantID,
		PolicyName:             req.PolicyName,
		PolicyCode:             req.PolicyCode,
		ResidencyMode:          req.ResidencyMode,
		ConflictResolutionMode: req.ConflictResolutionMode,
		ResidencyRegionID:      req.ResidencyRegionID,
		ActiveFlag:             true,
		CreatedAt:              time.Now().UTC(),
		CreatedByPrincipalID:   domain.PrincipalFromContext(ctx),
	}

	if err := s.store.CreateResidencyPolicy(ctx, p); err != nil {
		return nil, fmt.Errorf("store.CreateResidencyPolicy: %w", err)
	}
	return p, nil
}

// GetResidencyPolicy retrieves a policy by ID.
func (s *Service) GetResidencyPolicy(ctx context.Context, policyID string) (*domain.DataResidencyPolicy, error) {
	p, err := s.store.GetResidencyPolicyByID(ctx, policyID)
	if err != nil {
		return nil, fmt.Errorf("store.GetResidencyPolicyByID: %w", err)
	}
	if p == nil {
		return nil, ErrNotFound
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// ResidencyRegion — read-only (IaC-managed, per Q1 resolution)
// ---------------------------------------------------------------------------

// GetResidencyRegion returns a residency region by ID.
// ResidencyRegion records are IaC-provisioned; no write API is exposed.
func (s *Service) GetResidencyRegion(ctx context.Context, regionID string) (*domain.ResidencyRegion, error) {
	r, err := s.store.GetResidencyRegionByID(ctx, regionID)
	if err != nil {
		return nil, fmt.Errorf("store.GetResidencyRegionByID: %w", err)
	}
	if r == nil {
		return nil, ErrNotFound
	}
	return r, nil
}

// ListResidencyRegions returns all active residency regions.
func (s *Service) ListResidencyRegions(ctx context.Context) ([]*domain.ResidencyRegion, error) {
	return s.store.ListResidencyRegions(ctx)
}

// ResolveTenantRegion is the real tenant-to-region lookup for the Global
// Traffic & Residency Manager (docs/architecture/global-traffic-
// residency-manager-design.md, Q2) — replacing the Phase 1 routing demo's
// header stand-in with an actual resolution against this service's own
// data: Tenant.DefaultDataResidencyPolicyID -> DataResidencyPolicy.ResidencyRegionID
// -> ResidencyRegion.RegionCode.
//
// Returns ErrNotFound if the tenant doesn't exist, and the new, distinct
// ErrRegionUnresolved if the tenant exists but its policy has no region
// assigned yet (migration 000003 added the column nullable, unbackfilled
// — this is an expected, real state for existing policies, not a bug).
func (s *Service) ResolveTenantRegion(ctx context.Context, tenantID string) (*domain.ResolvedTenantRegion, error) {
	if err := s.assertTenantScope(ctx, tenantID); err != nil {
		return nil, err
	}
	t, err := s.store.GetTenantByID(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store.GetTenantByID: %w", err)
	}
	if t == nil {
		return nil, ErrNotFound
	}

	p, err := s.store.GetResidencyPolicyByID(ctx, t.DefaultDataResidencyPolicyID)
	if err != nil {
		return nil, fmt.Errorf("store.GetResidencyPolicyByID: %w", err)
	}
	if p == nil || p.ResidencyRegionID == nil {
		return nil, ErrRegionUnresolved
	}

	r, err := s.store.GetResidencyRegionByID(ctx, *p.ResidencyRegionID)
	if err != nil {
		return nil, fmt.Errorf("store.GetResidencyRegionByID: %w", err)
	}
	if r == nil {
		return nil, ErrRegionUnresolved
	}

	return &domain.ResolvedTenantRegion{
		TenantID:   tenantID,
		RegionCode: r.RegionCode,
		RegionName: r.RegionName,
	}, nil
}

// ---------------------------------------------------------------------------
// TaxIdentityBundle operations
//
// Per Q3 resolution: this service stores the structural header only —
// legal_entity_id, jurisdiction_id, effective dates, and status.
// The actual tax registration number and all evidence artifacts are owned
// by the Tax Service.
// ---------------------------------------------------------------------------

// CreateTaxIdentityBundle creates a new TaxIdentityBundle header.
// jurisdiction_id is validated synchronously — fail-closed.
func (s *Service) CreateTaxIdentityBundle(
	ctx context.Context,
	legalEntityID string,
	req domain.CreateTaxIdentityBundleRequest,
) (*domain.TaxIdentityBundle, error) {
	if err := s.authorize(ctx, "tax-identity-bundle", "create"); err != nil {
		return nil, err
	}

	if req.DataClassification != "" {
		if !classification.Classification(req.DataClassification).Valid() {
			return nil, fmt.Errorf("%w: invalid data classification %q", ErrInvalidInput, req.DataClassification)
		}
	}

	// Validate jurisdiction existence — fail-closed.
	if err := s.jurisd.ValidateExists(ctx, req.JurisdictionID); err != nil {
		return nil, s.mapJurisdictionErr(err, req.JurisdictionID)
	}

	b := &domain.TaxIdentityBundle{
		TaxIdentityBundleID:  newID(),
		TenantID:             domain.TenantFromContext(ctx),
		LegalEntityID:        legalEntityID,
		JurisdictionID:       req.JurisdictionID,
		Status:               domain.TaxIdentityBundlePending,
		EffectiveFrom:        req.EffectiveFrom,
		EffectiveTo:          req.EffectiveTo,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: domain.PrincipalFromContext(ctx),
		DataClassification:   req.DataClassification,
	}

	if err := s.store.CreateTaxIdentityBundle(ctx, b); err != nil {
		return nil, fmt.Errorf("store.CreateTaxIdentityBundle: %w", err)
	}
	return b, nil
}

// GetTaxIdentityBundle retrieves a TaxIdentityBundle header by ID.
func (s *Service) GetTaxIdentityBundle(ctx context.Context, bundleID string) (*domain.TaxIdentityBundle, error) {
	b, err := s.store.GetTaxIdentityBundleByID(ctx, bundleID)
	if err != nil {
		return nil, fmt.Errorf("store.GetTaxIdentityBundleByID: %w", err)
	}
	if b == nil {
		return nil, ErrNotFound
	}
	return b, nil
}

// ListTaxIdentityBundles returns all TaxIdentityBundle headers for an entity.
func (s *Service) ListTaxIdentityBundles(ctx context.Context, legalEntityID string) ([]*domain.TaxIdentityBundle, error) {
	return s.store.ListTaxIdentityBundlesByEntity(ctx, legalEntityID)
}

// TransitionTaxIdentityBundleStatus applies a status transition on a bundle header.
func (s *Service) TransitionTaxIdentityBundleStatus(
	ctx context.Context,
	bundleID string,
	req domain.TransitionTaxIdentityBundleStatusRequest,
) error {
	if err := s.authorize(ctx, "tax-identity-bundle", "status.transition"); err != nil {
		return err
	}
	return s.store.TransitionTaxIdentityBundleStatus(ctx, bundleID, req.NewStatus, domain.PrincipalFromContext(ctx), req.CorrelationID)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// authorize asks authorization-svc whether the calling principal may perform
// resource/action, and fails closed on anything that is not a clear grant.
//
// Both the principal and the scope come from the request context, populated by
// middleware.Identity from gateway-verified headers. They were previously
// taken from the raw Authorization header, which this service base64-decoded
// itself without checking the signature — so both the subject of the decision
// and the audit identity were caller-chosen. See internal/middleware.
//
// Scope is the tenant the caller is acting within. ProvisionTenant is the one
// operation with no tenant yet — it is evaluated against the configured
// platform scope, since authorization-svc rejects an empty legal_entity_id.
func (s *Service) authorize(ctx context.Context, resource, action string) error {
	principalID := domain.PrincipalFromContext(ctx)
	if principalID == "" {
		// No verified identity reached this call. Refusing here rather than
		// substituting a default keeps an unauthenticated request from being
		// recorded as a legitimate one.
		s.log.Warn("mutation attempted with no verified principal — rejecting",
			zap.String("resource", resource),
			zap.String("action", action),
		)
		return ErrUnauthenticated
	}

	scopeID := domain.TenantFromContext(ctx)
	if scopeID == "" {
		scopeID = s.platformScopeID
	}

	if err := s.authz.Authorize(ctx, principalID, scopeID, resource, action); err != nil {
		switch {
		case errors.Is(err, authz.ErrUnauthorized):
			return ErrUnauthorized
		case errors.Is(err, authz.ErrAuthZUnavailable):
			s.log.Error("authorization service unavailable — rejecting (fail-closed)",
				zap.String("resource", resource),
				zap.String("action", action),
			)
			return ErrServiceUnavailable
		}
		return fmt.Errorf("authz.Authorize: %w", err)
	}
	return nil
}

func (s *Service) mapJurisdictionErr(err error, jurisdictionID string) error {
	switch {
	case errors.Is(err, jurisdiction.ErrJurisdictionNotFound):
		return fmt.Errorf("%w: jurisdiction_id %s not found in Jurisdiction Rules Service", ErrInvalidInput, jurisdictionID)
	case errors.Is(err, jurisdiction.ErrValidatorUnavailable):
		s.log.Error("jurisdiction rules service unavailable — rejecting assignment (fail-closed)",
			zap.String("jurisdiction_id", jurisdictionID),
		)
		return ErrServiceUnavailable
	}
	return err
}

func isValidTenantTransition(from, to domain.TenantLifecycleState) bool {
	allowed, ok := domain.ValidTenantLifecycleTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

func isValidEntityTransition(from, to domain.EntityStatus) bool {
	allowed, ok := domain.ValidEntityStatusTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

func newID() string {
	id, _ := uuid.NewV7()
	return id.String()
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// actorFromJWT extracts the principal_id claim from the IdentityContextEnvelope
// JWT. This performs payload-only decoding — signature verification is the
// responsibility of the Authorization Service which the caller has already
// consulted. Returns "system" only as a last resort if the claim is absent,
// which will appear in audit logs as a signal that JWT wiring is incomplete.
// actorFromJWT is deliberately gone.
//
// It base64-decoded the JWT payload from the Authorization header and read
// principal_id out of it, with no signature verification, falling back to the
// literal string "system". Every audit column on every mutation was therefore
// stamped with an identity the caller chose — forging one needed no key, only
// base64 — and an unattributed write was recorded as the platform's own.
//
// The acting principal now comes from domain.PrincipalFromContext, populated
// by middleware.Identity from the gateway-verified X-Principal-Id header, and
// a mutation with no verified principal is refused rather than defaulted.
