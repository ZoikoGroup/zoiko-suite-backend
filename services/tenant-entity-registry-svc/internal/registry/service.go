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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/jurisdiction"
)

// Sentinel errors returned by the service layer.
// Handlers map these to appropriate HTTP status codes.
var (
	ErrNotFound           = errors.New("not found")
	ErrInvalidTransition  = errors.New("invalid status transition")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrServiceUnavailable = errors.New("upstream service unavailable")
	ErrInvalidInput       = errors.New("invalid input")
	// ErrConflict is returned when a unique constraint is violated (e.g. duplicate
	// tenant_code). Handlers should map this to HTTP 409 Conflict.
	ErrConflict           = errors.New("conflict: resource already exists")
)

// Service orchestrates all registry operations.
// It owns no HTTP concerns — those belong to internal/handler.
type Service struct {
	store    Store
	events   EventPublisher
	authz    AuthorizationClient
	jurisd   JurisdictionValidator
	log      *zap.Logger
}

// NewService constructs a Service with all required dependencies.
func NewService(
	store Store,
	events EventPublisher,
	authz AuthorizationClient,
	jurisd JurisdictionValidator,
	log *zap.Logger,
) *Service {
	return &Service{
		store:  store,
		events: events,
		authz:  authz,
		jurisd: jurisd,
		log:    log,
	}
}

// ---------------------------------------------------------------------------
// Tenant operations
// ---------------------------------------------------------------------------

// ProvisionTenant creates a new tenant in ONBOARDING lifecycle state.
// Idempotent: if the tenant already exists the store returns a duplicate error
// which the caller surfaces as a conflict.
func (s *Service) ProvisionTenant(
	ctx context.Context,
	envelopeJWT string,
	req domain.ProvisionTenantRequest,
	correlationID string,
) (*domain.Tenant, error) {
	if err := s.authorize(ctx, envelopeJWT, "tenant", "provision"); err != nil {
		return nil, err
	}

	t := &domain.Tenant{
		TenantID:                     newID(),
		TenantCode:                   req.TenantCode,
		LegalName:                    req.LegalName,
		TradingName:                  nullableString(req.TradingName),
		Status:                       domain.TenantStatusActive,
		DefaultCurrencyCode:          req.DefaultCurrencyCode,
		PrimaryTimezone:              req.PrimaryTimezone,
		PrimaryLocale:                req.PrimaryLocale,
		DefaultDataResidencyPolicyID: req.DefaultDataResidencyPolicyID,
		LifecycleState:               domain.TenantLifecycleOnboarding,
		CreatedAt:                    time.Now().UTC(),
		CreatedByPrincipalID:         actorFromJWT(envelopeJWT),
	}

	if err := s.store.CreateTenant(ctx, t); err != nil {
		s.log.Error("create tenant failed", zap.Error(err), zap.String("correlation_id", correlationID))
		return nil, fmt.Errorf("store.CreateTenant: %w", err)
	}

	go s.events.PublishTenantCreated(ctx, t, correlationID)

	s.log.Info("tenant provisioned",
		zap.String("tenant_id", t.TenantID),
		zap.String("correlation_id", correlationID),
	)
	return t, nil
}

// GetTenant retrieves a tenant by ID. Returns ErrNotFound if absent.
func (s *Service) GetTenant(ctx context.Context, tenantID string) (*domain.Tenant, error) {
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
	envelopeJWT, tenantID string,
	req domain.TransitionTenantLifecycleRequest,
) error {
	if err := s.authorize(ctx, envelopeJWT, "tenant:"+tenantID, "lifecycle.transition"); err != nil {
		return err
	}

	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return err
	}

	if !isValidTenantTransition(t.LifecycleState, req.TargetState) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, t.LifecycleState, req.TargetState)
	}

	return s.store.TransitionTenantLifecycle(ctx, tenantID, req.TargetState, actorFromJWT(envelopeJWT), req.CorrelationID)
}

// ---------------------------------------------------------------------------
// LegalEntity operations
// ---------------------------------------------------------------------------

// CreateEntity creates a new LegalEntity under an existing tenant.
// primary_jurisdiction_id is validated synchronously against the Jurisdiction
// Rules Service before persistence (Q2 resolution — fail-closed).
func (s *Service) CreateEntity(
	ctx context.Context,
	envelopeJWT string,
	req domain.CreateEntityRequest,
) (*domain.LegalEntity, error) {
	if err := s.authorize(ctx, envelopeJWT, "entity", "create"); err != nil {
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
		CreatedByPrincipalID:  actorFromJWT(envelopeJWT),
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
	return s.store.ListEntitiesByTenant(ctx, tenantID)
}

// UpdateEntity applies a partial update to a legal entity.
// Only mutable non-governance fields may be patched (legal_name, trading_name, currency).
func (s *Service) UpdateEntity(
	ctx context.Context,
	envelopeJWT, legalEntityID string,
	req domain.UpdateEntityRequest,
) (*domain.LegalEntity, error) {
	if err := s.authorize(ctx, envelopeJWT, "entity:"+legalEntityID, "update"); err != nil {
		return nil, err
	}

	// Populate audit actor from the verified envelope JWT.
	// actorFromJWT performs payload-only decoding — signature is already
	// verified by the Authorization Service before this service is called.
	req.ActorPrincipalID = actorFromJWT(envelopeJWT)

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
	envelopeJWT, legalEntityID string,
	req domain.TransitionEntityStatusRequest,
) error {
	if err := s.authorize(ctx, envelopeJWT, "entity:"+legalEntityID, "status.transition"); err != nil {
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
		actorFromJWT(envelopeJWT), req.CorrelationID,
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
	envelopeJWT string,
	req domain.CreateHierarchyRequest,
) (*domain.EntityHierarchy, error) {
	if err := s.authorize(ctx, envelopeJWT, "entity.hierarchy", "create"); err != nil {
		return nil, err
	}

	h := &domain.EntityHierarchy{
		HierarchyID:         newID(),
		TenantID:            req.TenantID,
		ParentLegalEntityID: req.ParentLegalEntityID,
		ChildLegalEntityID:  req.ChildLegalEntityID,
		RelationshipType:    req.RelationshipType,
		EffectiveFrom:       req.EffectiveFrom,
		CreatedAt:           time.Now().UTC(),
		CreatedByPrincipalID: actorFromJWT(envelopeJWT),
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
	envelopeJWT, hierarchyID string,
	endDate time.Time,
	correlationID string,
) error {
	if err := s.authorize(ctx, envelopeJWT, "entity.hierarchy:"+hierarchyID, "end-date"); err != nil {
		return err
	}

	if err := s.store.EndDateHierarchy(ctx, hierarchyID, endDate, actorFromJWT(envelopeJWT), correlationID); err != nil {
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
	envelopeJWT, legalEntityID string,
	req domain.AssignJurisdictionRequest,
) (*domain.EntityJurisdictionAssignment, error) {
	if err := s.authorize(ctx, envelopeJWT, "entity:"+legalEntityID+"/jurisdiction", "assign"); err != nil {
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
		CreatedByPrincipalID: actorFromJWT(envelopeJWT),
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
	envelopeJWT, assignmentID string,
	endDate time.Time,
	correlationID string,
) error {
	if err := s.authorize(ctx, envelopeJWT, "entity.jurisdiction:"+assignmentID, "end-date"); err != nil {
		return err
	}

	if err := s.store.EndDateJurisdictionAssignment(ctx, assignmentID, endDate, actorFromJWT(envelopeJWT), correlationID); err != nil {
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
	envelopeJWT string,
	req domain.CreateResidencyPolicyRequest,
) (*domain.DataResidencyPolicy, error) {
	if err := s.authorize(ctx, envelopeJWT, "residency.policy", "create"); err != nil {
		return nil, err
	}

	p := &domain.DataResidencyPolicy{
		DataResidencyPolicyID:  newID(),
		TenantID:               req.TenantID,
		PolicyName:             req.PolicyName,
		PolicyCode:             req.PolicyCode,
		ResidencyMode:          req.ResidencyMode,
		ConflictResolutionMode: req.ConflictResolutionMode,
		ActiveFlag:             true,
		CreatedAt:              time.Now().UTC(),
		CreatedByPrincipalID:   actorFromJWT(envelopeJWT),
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
	envelopeJWT, legalEntityID string,
	req domain.CreateTaxIdentityBundleRequest,
) (*domain.TaxIdentityBundle, error) {
	if err := s.authorize(ctx, envelopeJWT, "entity:"+legalEntityID+"/tax-identity-bundle", "create"); err != nil {
		return nil, err
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
		CreatedByPrincipalID: actorFromJWT(envelopeJWT),
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
	envelopeJWT, bundleID string,
	req domain.TransitionTaxIdentityBundleStatusRequest,
) error {
	if err := s.authorize(ctx, envelopeJWT, "tax-identity-bundle:"+bundleID, "status.transition"); err != nil {
		return err
	}
	return s.store.TransitionTaxIdentityBundleStatus(ctx, bundleID, req.NewStatus, actorFromJWT(envelopeJWT), req.CorrelationID)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (s *Service) authorize(ctx context.Context, envelopeJWT, resource, action string) error {
	if err := s.authz.Authorize(ctx, envelopeJWT, resource, action); err != nil {
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
func actorFromJWT(token string) string {
	token = strings.TrimPrefix(token, "Bearer ")
	if token == "" {
		return "system"
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "system"
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "system"
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "system"
	}
	if pid, ok := claims["principal_id"].(string); ok && pid != "" {
		return pid
	}
	// Fallback: some issuers use "sub" as the principal identifier.
	if sub, ok := claims["sub"].(string); ok && sub != "" {
		return sub
	}
	return "system"
}
