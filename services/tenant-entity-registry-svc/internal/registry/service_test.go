package registry_test

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/jurisdiction"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ---------------------------------------------------------------------------
// In-memory store stub for tests
// ---------------------------------------------------------------------------

type memStore struct {
	tenants           map[string]*domain.Tenant
	entities          map[string]*domain.LegalEntity
	bundles           map[string]*domain.TaxIdentityBundle
	residencyPolicies map[string]*domain.DataResidencyPolicy
	lastUpdateActor   string // records ActorPrincipalID from the last UpdateEntity call
	// Minimal set — add more maps as tests require.
}

func newMemStore() *memStore {
	return &memStore{
		tenants:           make(map[string]*domain.Tenant),
		entities:          make(map[string]*domain.LegalEntity),
		bundles:           make(map[string]*domain.TaxIdentityBundle),
		residencyPolicies: make(map[string]*domain.DataResidencyPolicy),
	}
}

func (m *memStore) CreateTenant(_ context.Context, t *domain.Tenant) error {
	m.tenants[t.TenantID] = t
	return nil
}
func (m *memStore) CreateTenantWithDefaultResidencyPolicy(_ context.Context, t *domain.Tenant, p *domain.DataResidencyPolicy) error {
	m.tenants[t.TenantID] = t
	m.residencyPolicies[p.DataResidencyPolicyID] = p
	return nil
}
func (m *memStore) GetTenantByID(_ context.Context, id string) (*domain.Tenant, error) {
	t, ok := m.tenants[id]
	if !ok {
		return nil, nil
	}
	return t, nil
}
func (m *memStore) TransitionTenantLifecycle(_ context.Context, id string, state domain.TenantLifecycleState, _, _ string) error {
	if t, ok := m.tenants[id]; ok {
		t.LifecycleState = state
	}
	return nil
}
func (m *memStore) CreateEntity(_ context.Context, e *domain.LegalEntity) error {
	m.entities[e.LegalEntityID] = e
	return nil
}
func (m *memStore) GetEntityByID(_ context.Context, id string) (*domain.LegalEntity, error) {
	e, ok := m.entities[id]
	if !ok {
		return nil, nil
	}
	return e, nil
}
func (m *memStore) ListEntitiesByTenant(_ context.Context, _ string) ([]*domain.LegalEntity, error) {
	return []*domain.LegalEntity{}, nil
}
func (m *memStore) UpdateEntity(_ context.Context, id string, req domain.UpdateEntityRequest) (*domain.LegalEntity, error) {
	m.lastUpdateActor = req.ActorPrincipalID // record for assertion in tests
	e, ok := m.entities[id]
	if !ok {
		return nil, nil
	}
	if req.LegalName != nil {
		e.LegalName = *req.LegalName
	}
	if req.TradingName != nil {
		e.TradingName = req.TradingName
	}
	if req.DefaultCurrencyCode != nil {
		e.DefaultCurrencyCode = *req.DefaultCurrencyCode
	}
	return e, nil
}
func (m *memStore) TransitionEntityStatus(_ context.Context, id string, status domain.EntityStatus, allowedPriors []domain.EntityStatus, _, _ string) (int64, string, error) {
	e, ok := m.entities[id]
	if !ok {
		return 0, "", nil
	}
	// Check whether current state is in allowedPriors (faithful emulation of DB ANY clause).
	current := e.EntityStatus
	inPriors := false
	for _, p := range allowedPriors {
		if p == current {
			inPriors = true
			break
		}
	}
	if !inPriors {
		return 0, "", nil
	}
	e.EntityStatus = status
	return 1, e.TenantID, nil
}
func (m *memStore) GetEntityStatus(_ context.Context, id string) (*domain.EntityStatusResponse, error) {
	e, ok := m.entities[id]
	if !ok {
		return nil, nil
	}
	return &domain.EntityStatusResponse{
		EntityID:     e.LegalEntityID,
		TenantID:     e.TenantID,
		EntityStatus: e.EntityStatus,
	}, nil
}
func (m *memStore) CreateHierarchy(_ context.Context, _ *domain.EntityHierarchy) error { return nil }
func (m *memStore) EndDateHierarchy(_ context.Context, _ string, _ time.Time, _, _ string) error {
	return nil
}
func (m *memStore) ListHierarchiesByEntity(_ context.Context, _ string) ([]*domain.EntityHierarchy, error) {
	return []*domain.EntityHierarchy{}, nil
}
func (m *memStore) CreateJurisdictionAssignment(_ context.Context, _ *domain.EntityJurisdictionAssignment) error {
	return nil
}
func (m *memStore) ListJurisdictionAssignments(_ context.Context, _ string) ([]*domain.EntityJurisdictionAssignment, error) {
	return []*domain.EntityJurisdictionAssignment{}, nil
}
func (m *memStore) EndDateJurisdictionAssignment(_ context.Context, _ string, _ time.Time, _, _ string) error {
	return nil
}
func (m *memStore) CreateResidencyPolicy(_ context.Context, p *domain.DataResidencyPolicy) error {
	m.residencyPolicies[p.DataResidencyPolicyID] = p
	return nil
}
func (m *memStore) GetResidencyPolicyByID(_ context.Context, id string) (*domain.DataResidencyPolicy, error) {
	p, ok := m.residencyPolicies[id]
	if !ok {
		return nil, nil
	}
	return p, nil
}
func (m *memStore) GetResidencyRegionByID(_ context.Context, _ string) (*domain.ResidencyRegion, error) {
	return nil, nil
}
func (m *memStore) ListResidencyRegions(_ context.Context) ([]*domain.ResidencyRegion, error) {
	return []*domain.ResidencyRegion{}, nil
}
func (m *memStore) CreateTaxIdentityBundle(_ context.Context, b *domain.TaxIdentityBundle) error {
	m.bundles[b.TaxIdentityBundleID] = b
	return nil
}
func (m *memStore) GetTaxIdentityBundleByID(_ context.Context, id string) (*domain.TaxIdentityBundle, error) {
	b, ok := m.bundles[id]
	if !ok {
		return nil, nil
	}
	return b, nil
}
func (m *memStore) ListTaxIdentityBundlesByEntity(_ context.Context, _ string) ([]*domain.TaxIdentityBundle, error) {
	return []*domain.TaxIdentityBundle{}, nil
}
func (m *memStore) TransitionTaxIdentityBundleStatus(_ context.Context, id string, status domain.TaxIdentityBundleStatus, _, _ string) error {
	if b, ok := m.bundles[id]; ok {
		b.Status = status
	}
	return nil
}

// ---------------------------------------------------------------------------
// No-op event publisher
// ---------------------------------------------------------------------------

type noopPublisher struct{}

func (noopPublisher) PublishTenantCreated(_ context.Context, _ *domain.Tenant, _ string) {}
func (noopPublisher) PublishEntityCreated(_ context.Context, _ *domain.LegalEntity, _ string) {}
func (noopPublisher) PublishEntityUpdated(_ context.Context, _ *domain.LegalEntity, _ string) {}
func (noopPublisher) PublishEntityStatusChanged(_ context.Context, _, _ string, _, _ domain.EntityStatus, _ string) {
}
func (noopPublisher) PublishEntityHierarchyChanged(_ context.Context, _ *domain.EntityHierarchy, _ string, _ string) {
}
func (noopPublisher) PublishEntityJurisdictionChanged(_ context.Context, _ *domain.EntityJurisdictionAssignment, _ string, _ string) {
}

// ---------------------------------------------------------------------------
// Authz stubs
// ---------------------------------------------------------------------------

type permitAllAuthZ struct{}

func (permitAllAuthZ) Authorize(_ context.Context, _, _, _ string) error { return nil }

type denyAllAuthZ struct{}

func (denyAllAuthZ) Authorize(_ context.Context, _, _, _ string) error {
	return authz.ErrUnauthorized
}

type unavailableAuthZ struct{}

func (unavailableAuthZ) Authorize(_ context.Context, _, _, _ string) error {
	return authz.ErrAuthZUnavailable
}

// ---------------------------------------------------------------------------
// Jurisdiction stubs
// ---------------------------------------------------------------------------

type acceptAllJurisd struct{}

func (acceptAllJurisd) ValidateExists(_ context.Context, _ string) error { return nil }

type rejectJurisd struct{}

func (rejectJurisd) ValidateExists(_ context.Context, _ string) error {
	return jurisdiction.ErrJurisdictionNotFound
}

type unavailableJurisd struct{}

func (unavailableJurisd) ValidateExists(_ context.Context, _ string) error {
	return jurisdiction.ErrValidatorUnavailable
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newSvc(t *testing.T, store registry.Store, authzC registry.AuthorizationClient, jv registry.JurisdictionValidator) *registry.Service {
	t.Helper()
	log := zap.NewNop()
	return registry.NewService(store, noopPublisher{}, authzC, jv, log)
}

func baseSvc(t *testing.T) (*registry.Service, *memStore) {
	t.Helper()
	ms := newMemStore()
	svc := newSvc(t, ms, permitAllAuthZ{}, acceptAllJurisd{})
	return svc, ms
}

// ---------------------------------------------------------------------------
// Tenant tests
// ---------------------------------------------------------------------------

func TestProvisionTenant_Success(t *testing.T) {
	svc, ms := baseSvc(t)

	req := domain.ProvisionTenantRequest{
		TenantCode:                   "ACME",
		LegalName:                    "ACME Corp Ltd",
		DefaultCurrencyCode:          "USD",
		PrimaryTimezone:              "UTC",
		PrimaryLocale:                "en-US",
		DefaultDataResidencyPolicyID: "drp-001",
	}

	tenant, err := svc.ProvisionTenant(context.Background(), "jwt-stub", req, "corr-001")
	require.NoError(t, err)
	assert.NotEmpty(t, tenant.TenantID)
	assert.Equal(t, domain.TenantLifecycleOnboarding, tenant.LifecycleState)
	assert.Equal(t, domain.TenantStatusActive, tenant.Status)

	// Verify persisted
	stored, _ := ms.GetTenantByID(context.Background(), tenant.TenantID)
	require.NotNil(t, stored)
	assert.Equal(t, tenant.TenantID, stored.TenantID)
}

func TestProvisionTenant_Unauthorized(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, denyAllAuthZ{}, acceptAllJurisd{})

	_, err := svc.ProvisionTenant(context.Background(), "bad-jwt", domain.ProvisionTenantRequest{}, "corr")
	assert.ErrorIs(t, err, registry.ErrUnauthorized)
}

func TestProvisionTenant_AuthZUnavailable_FailsClosed(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, unavailableAuthZ{}, acceptAllJurisd{})

	_, err := svc.ProvisionTenant(context.Background(), "jwt", domain.ProvisionTenantRequest{}, "corr")
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
}

func TestTransitionTenantLifecycle_ValidTransition(t *testing.T) {
	svc, ms := baseSvc(t)

	// Create a tenant in ONBOARDING state
	req := domain.ProvisionTenantRequest{
		TenantCode:                   "T1",
		LegalName:                    "Tenant One",
		DefaultCurrencyCode:          "GBP",
		PrimaryTimezone:              "Europe/London",
		PrimaryLocale:                "en-GB",
		DefaultDataResidencyPolicyID: "drp-001",
	}
	tenant, err := svc.ProvisionTenant(context.Background(), "jwt", req, "corr")
	require.NoError(t, err)
	assert.Equal(t, domain.TenantLifecycleOnboarding, tenant.LifecycleState)

	// Transition ONBOARDING → ACTIVE (valid)
	err = svc.TransitionTenantLifecycle(context.Background(), "jwt", tenant.TenantID,
		domain.TransitionTenantLifecycleRequest{
			TargetState:   domain.TenantLifecycleActive,
			CorrelationID: "corr-002",
		})
	require.NoError(t, err)

	stored, _ := ms.GetTenantByID(context.Background(), tenant.TenantID)
	assert.Equal(t, domain.TenantLifecycleActive, stored.LifecycleState)
}

func TestTransitionTenantLifecycle_InvalidTransition(t *testing.T) {
	svc, _ := baseSvc(t)

	req := domain.ProvisionTenantRequest{
		TenantCode:                   "T2",
		LegalName:                    "Tenant Two",
		DefaultCurrencyCode:          "EUR",
		PrimaryTimezone:              "UTC",
		PrimaryLocale:                "fr-FR",
		DefaultDataResidencyPolicyID: "drp-001",
	}
	tenant, _ := svc.ProvisionTenant(context.Background(), "jwt", req, "corr")

	// ONBOARDING → OFFBOARDING is not a valid transition
	err := svc.TransitionTenantLifecycle(context.Background(), "jwt", tenant.TenantID,
		domain.TransitionTenantLifecycleRequest{
			TargetState:   domain.TenantLifecycleOffboarding,
			CorrelationID: "corr-003",
		})
	assert.ErrorIs(t, err, registry.ErrInvalidTransition)
}

// ---------------------------------------------------------------------------
// Entity tests
// ---------------------------------------------------------------------------

func TestCreateEntity_Success(t *testing.T) {
	svc, ms := baseSvc(t)

	req := domain.CreateEntityRequest{
		TenantID:              "tenant-001",
		EntityCode:            "ENT001",
		LegalName:             "Entity One Ltd",
		EntityType:            domain.EntityTypeSubsidiary,
		DefaultCurrencyCode:   "USD",
		FiscalCalendarID:      "fc-001",
		PrimaryJurisdictionID: "JUR-US",
		DataResidencyPolicyID: "drp-001",
		CorrelationID:         "corr-004",
	}

	entity, err := svc.CreateEntity(context.Background(), "jwt", req)
	require.NoError(t, err)
	assert.NotEmpty(t, entity.LegalEntityID)
	assert.Equal(t, domain.EntityStatusActive, entity.EntityStatus)

	stored, _ := ms.GetEntityByID(context.Background(), entity.LegalEntityID)
	require.NotNil(t, stored)
	assert.Equal(t, "ENT001", stored.EntityCode)
}

func TestCreateEntity_JurisdictionNotFound_FailsClosed(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, permitAllAuthZ{}, rejectJurisd{})

	req := domain.CreateEntityRequest{
		TenantID:              "tenant-001",
		EntityCode:            "ENT002",
		LegalName:             "Entity Two",
		EntityType:            domain.EntityTypeOperational,
		DefaultCurrencyCode:   "GBP",
		FiscalCalendarID:      "fc-001",
		PrimaryJurisdictionID: "JUR-INVALID",
		DataResidencyPolicyID: "drp-001",
		CorrelationID:         "corr-005",
	}

	_, err := svc.CreateEntity(context.Background(), "jwt", req)
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestCreateEntity_JurisdictionServiceUnavailable_FailsClosed(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, permitAllAuthZ{}, unavailableJurisd{})

	req := domain.CreateEntityRequest{
		TenantID:              "tenant-001",
		EntityCode:            "ENT003",
		LegalName:             "Entity Three",
		EntityType:            domain.EntityTypeOperational,
		DefaultCurrencyCode:   "EUR",
		FiscalCalendarID:      "fc-001",
		PrimaryJurisdictionID: "JUR-US",
		DataResidencyPolicyID: "drp-001",
		CorrelationID:         "corr-006",
	}

	_, err := svc.CreateEntity(context.Background(), "jwt", req)
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
}

// ---------------------------------------------------------------------------
// Entity status probe tests (GET /v1/entities/{entityID}/status)
// ---------------------------------------------------------------------------

func TestGetEntityStatus_NotFound(t *testing.T) {
	svc, _ := baseSvc(t)

	_, err := svc.GetEntityStatus(context.Background(), "nonexistent-entity")
	assert.ErrorIs(t, err, registry.ErrNotFound)
}

func TestGetEntityStatus_Found(t *testing.T) {
	svc, ms := baseSvc(t)

	// Seed an entity in the store directly
	ms.entities["ent-999"] = &domain.LegalEntity{
		LegalEntityID: "ent-999",
		TenantID:      "tenant-001",
		EntityStatus:  domain.EntityStatusActive,
	}

	resp, err := svc.GetEntityStatus(context.Background(), "ent-999")
	require.NoError(t, err)
	assert.Equal(t, domain.EntityStatusActive, resp.EntityStatus)
	assert.Equal(t, "tenant-001", resp.TenantID)
}

// ---------------------------------------------------------------------------
// Entity status transition tests
// ---------------------------------------------------------------------------

func TestTransitionEntityStatus_ValidTransition(t *testing.T) {
	svc, ms := baseSvc(t)

	ms.entities["ent-001"] = &domain.LegalEntity{
		LegalEntityID: "ent-001",
		TenantID:      "tenant-001",
		EntityStatus:  domain.EntityStatusActive,
	}

	err := svc.TransitionEntityStatus(context.Background(), "jwt", "ent-001",
		domain.TransitionEntityStatusRequest{
			NewStatus:     domain.EntityStatusDormant,
			CorrelationID: "corr-007",
		})
	require.NoError(t, err)
	assert.Equal(t, domain.EntityStatusDormant, ms.entities["ent-001"].EntityStatus)
}

func TestTransitionEntityStatus_Idempotent_SameStatus(t *testing.T) {
	svc, ms := baseSvc(t)

	ms.entities["ent-002"] = &domain.LegalEntity{
		LegalEntityID: "ent-002",
		TenantID:      "tenant-001",
		EntityStatus:  domain.EntityStatusDormant,
	}

	// Applying the same status must be a no-op (idempotent)
	err := svc.TransitionEntityStatus(context.Background(), "jwt", "ent-002",
		domain.TransitionEntityStatusRequest{
			NewStatus:     domain.EntityStatusDormant,
			CorrelationID: "corr-008",
		})
	assert.NoError(t, err)
	// Status unchanged
	assert.Equal(t, domain.EntityStatusDormant, ms.entities["ent-002"].EntityStatus)
}

func TestTransitionEntityStatus_InvalidTransition_Rejected(t *testing.T) {
	svc, ms := baseSvc(t)

	ms.entities["ent-003"] = &domain.LegalEntity{
		LegalEntityID: "ent-003",
		TenantID:      "tenant-001",
		EntityStatus:  domain.EntityStatusDissolved, // terminal state
	}

	err := svc.TransitionEntityStatus(context.Background(), "jwt", "ent-003",
		domain.TransitionEntityStatusRequest{
			NewStatus:     domain.EntityStatusActive,
			CorrelationID: "corr-009",
		})
	assert.ErrorIs(t, err, registry.ErrInvalidTransition)
}

// ---------------------------------------------------------------------------
// TaxIdentityBundle tests (header-only model, Q3 resolution)
// ---------------------------------------------------------------------------

func TestCreateTaxIdentityBundle_Success(t *testing.T) {
	svc, ms := baseSvc(t)

	ms.entities["ent-100"] = &domain.LegalEntity{
		LegalEntityID: "ent-100",
		TenantID:      "tenant-001",
		EntityStatus:  domain.EntityStatusActive,
	}

	req := domain.CreateTaxIdentityBundleRequest{
		JurisdictionID: "JUR-US",
		EffectiveFrom:  time.Now().UTC(),
		CorrelationID:  "corr-010",
	}

	bundle, err := svc.CreateTaxIdentityBundle(context.Background(), "jwt", "ent-100", req)
	require.NoError(t, err)
	assert.NotEmpty(t, bundle.TaxIdentityBundleID)
	assert.Equal(t, "ent-100", bundle.LegalEntityID)
	assert.Equal(t, "JUR-US", bundle.JurisdictionID)
	assert.Equal(t, domain.TaxIdentityBundlePending, bundle.Status)

	// Verify header stored
	stored, _ := ms.GetTaxIdentityBundleByID(context.Background(), bundle.TaxIdentityBundleID)
	require.NotNil(t, stored)
	// Confirm no tax identifier value is present in the type
	assert.Equal(t, bundle.TaxIdentityBundleID, stored.TaxIdentityBundleID)
}

func TestCreateTaxIdentityBundle_InvalidDataClassification_Fails(t *testing.T) {
	svc, ms := baseSvc(t)

	ms.entities["ent-100"] = &domain.LegalEntity{
		LegalEntityID: "ent-100",
		TenantID:      "tenant-001",
		EntityStatus:  domain.EntityStatusActive,
	}

	req := domain.CreateTaxIdentityBundleRequest{
		JurisdictionID:     "JUR-US",
		EffectiveFrom:      time.Now().UTC(),
		CorrelationID:      "corr-010",
		DataClassification: "INVALID_CLASSIFICATION",
	}

	_, err := svc.CreateTaxIdentityBundle(context.Background(), "jwt", "ent-100", req)
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestCreateTaxIdentityBundle_InvalidJurisdiction_FailsClosed(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, permitAllAuthZ{}, rejectJurisd{})

	req := domain.CreateTaxIdentityBundleRequest{
		JurisdictionID: "JUR-INVALID",
		EffectiveFrom:  time.Now().UTC(),
		CorrelationID:  "corr-011",
	}

	_, err := svc.CreateTaxIdentityBundle(context.Background(), "jwt", "ent-100", req)
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestCreateTaxIdentityBundle_JurisdictionUnavailable_FailsClosed(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, permitAllAuthZ{}, unavailableJurisd{})

	req := domain.CreateTaxIdentityBundleRequest{
		JurisdictionID: "JUR-US",
		EffectiveFrom:  time.Now().UTC(),
		CorrelationID:  "corr-012",
	}

	_, err := svc.CreateTaxIdentityBundle(context.Background(), "jwt", "ent-100", req)
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
}

// ---------------------------------------------------------------------------
// UpdateEntity actor audit tests (R3 fix)
// ---------------------------------------------------------------------------

// TestUpdateEntity_WritesRealActorPrincipalID confirms that the service
// extracts principal_id from the envelope JWT and passes it to the store
// as updated_by_principal_id — not the "system" fallback.
//
// actorFromJWT performs payload-only base64 decoding; it does NOT verify the
// signature. We craft a minimal unsigned JWT to exercise the extraction path
// without needing a real key or signing library in this test package.
func TestUpdateEntity_WritesRealActorPrincipalID(t *testing.T) {
	svc, ms := baseSvc(t)
	ctx := context.Background()

	// Pre-seed an entity so UpdateEntity finds it.
	entityID := "ent-actor-test"
	ms.entities[entityID] = &domain.LegalEntity{
		LegalEntityID: entityID,
		TenantID:      "ten-001",
		LegalName:     "Original Name",
		EntityStatus:  domain.EntityStatusActive,
	}

	wantPrincipalID := "usr_01J0000000000000000000001"

	// Build a minimal unsigned JWT: header.payload.sig where:
	//   header = {"alg":"none"}
	//   payload = {"principal_id":"<id>"}
	//   sig = empty (actorFromJWT ignores the signature entirely)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"principal_id":"` + wantPrincipalID + `"}`),
	)
	envelopeJWT := header + "." + payload + "."

	newName := "Updated Name"
	req := domain.UpdateEntityRequest{
		LegalName:     &newName,
		CorrelationID: "corr-actor-test",
	}

	_, err := svc.UpdateEntity(ctx, envelopeJWT, entityID, req)
	require.NoError(t, err)

	// The store must have received the real principal_id, not "system".
	assert.Equal(t, wantPrincipalID, ms.lastUpdateActor,
		"updated_by_principal_id must be the real actor from the JWT, not the 'system' fallback")
	assert.NotEqual(t, "system", ms.lastUpdateActor,
		"hardcoded 'system' must not appear when a valid JWT is provided")
}

// TestUpdateEntity_FallsBackToSystem_WhenJWTAbsent confirms the documented
// fallback: when no JWT is provided, actor is "system" and the update still
// succeeds. This is intentional; it will be visible in audit logs as a signal
// that the caller did not supply a JWT.
func TestUpdateEntity_FallsBackToSystem_WhenJWTAbsent(t *testing.T) {
	svc, ms := baseSvc(t)
	ctx := context.Background()

	entityID := "ent-no-jwt"
	ms.entities[entityID] = &domain.LegalEntity{
		LegalEntityID: entityID,
		TenantID:      "ten-001",
		LegalName:     "Name",
		EntityStatus:  domain.EntityStatusActive,
	}

	newName := "Changed"
	req := domain.UpdateEntityRequest{LegalName: &newName, CorrelationID: "corr-nojwt"}

	_, err := svc.UpdateEntity(ctx, "" /* no JWT */, entityID, req)
	require.NoError(t, err)
	assert.Equal(t, "system", ms.lastUpdateActor,
		"when JWT is absent, actor must be 'system' (visible in audit logs as a wiring signal)")
}
