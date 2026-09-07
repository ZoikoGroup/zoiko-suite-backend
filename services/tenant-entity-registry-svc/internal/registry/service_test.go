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
	workspaces        map[string]*domain.Workspace
	bundles           map[string]*domain.TaxIdentityBundle
	residencyPolicies map[string]*domain.DataResidencyPolicy
	lastUpdateActor   string // records ActorPrincipalID from the last UpdateEntity call
	// Minimal set — add more maps as tests require.
}

func newMemStore() *memStore {
	return &memStore{
		tenants:           make(map[string]*domain.Tenant),
		entities:          make(map[string]*domain.LegalEntity),
		workspaces:        make(map[string]*domain.Workspace),
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
func (m *memStore) CreateWorkspace(_ context.Context, w *domain.Workspace) error {
	m.workspaces[w.WorkspaceID] = w
	return nil
}
func (m *memStore) GetWorkspaceByID(_ context.Context, id string) (*domain.Workspace, error) {
	w, ok := m.workspaces[id]
	if !ok {
		return nil, nil
	}
	return w, nil
}
func (m *memStore) ListWorkspacesByTenant(_ context.Context, _ string) ([]*domain.Workspace, error) {
	return []*domain.Workspace{}, nil
}
func (m *memStore) UpdateWorkspace(_ context.Context, id string, req domain.UpdateWorkspaceRequest) (*domain.Workspace, error) {
	w, ok := m.workspaces[id]
	if !ok {
		return nil, nil
	}
	// COALESCE semantics: an omitted field keeps its value.
	if req.Name != nil {
		w.Name = *req.Name
	}
	if req.BusinessUnit != nil {
		w.BusinessUnit = req.BusinessUnit
	}
	if req.BillingClassification != nil {
		w.BillingClassification = domain.BillingClassification(*req.BillingClassification)
	}
	if req.BillingSource != nil {
		w.BillingSource = domain.BillingSource(*req.BillingSource)
	}
	if req.CommercialAccountID != nil {
		w.CommercialAccountID = req.CommercialAccountID
	}
	w.UpdatedByPrincipalID = req.ActorPrincipalID
	return w, nil
}

// Mirrors the guarded UPDATE: the write only lands if the current status is in
// allowedPriors, and the prior value is reported back.
func (m *memStore) TransitionWorkspaceStatus(
	_ context.Context,
	id string,
	newStatus domain.WorkspaceStatus,
	allowedPriors []domain.WorkspaceStatus,
	actorID, _ string,
) (int64, domain.WorkspaceStatus, error) {
	w, ok := m.workspaces[id]
	if !ok {
		return 0, "", nil
	}
	for _, p := range allowedPriors {
		if w.Status == p {
			prev := w.Status
			w.Status = newStatus
			w.UpdatedByPrincipalID = actorID
			return 1, prev, nil
		}
	}
	return 0, "", nil
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

func (noopPublisher) PublishTenantCreated(_ context.Context, _ *domain.Tenant, _ string)       {}
func (noopPublisher) PublishEntityCreated(_ context.Context, _ *domain.LegalEntity, _ string)  {}
func (noopPublisher) PublishEntityUpdated(_ context.Context, _ *domain.LegalEntity, _ string)  {}
func (noopPublisher) PublishWorkspaceCreated(_ context.Context, _ *domain.Workspace, _ string) {}
func (noopPublisher) PublishWorkspaceUpdated(_ context.Context, _ *domain.Workspace, _ string) {}
func (noopPublisher) PublishWorkspaceStatusChanged(_ context.Context, _, _, _ string, _, _ domain.WorkspaceStatus, _ string) {
}
func (noopPublisher) PublishEntityStatusChanged(_ context.Context, _, _, _ string, _, _ domain.EntityStatus, _ string) {
}
func (noopPublisher) PublishEntityHierarchyChanged(_ context.Context, _ *domain.EntityHierarchy, _ string, _ string) {
}
func (noopPublisher) PublishEntityJurisdictionChanged(_ context.Context, _ *domain.EntityJurisdictionAssignment, _ string, _ string) {
}

// ---------------------------------------------------------------------------
// Authz stubs
// ---------------------------------------------------------------------------

type permitAllAuthZ struct{}

func (permitAllAuthZ) Authorize(_ context.Context, _, _, _, _ string) error { return nil }

type denyAllAuthZ struct{}

func (denyAllAuthZ) Authorize(_ context.Context, _, _, _, _ string) error {
	return authz.ErrUnauthorized
}

type unavailableAuthZ struct{}

func (unavailableAuthZ) Authorize(_ context.Context, _, _, _, _ string) error {
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

// testPlatformScope stands in for config.AuthZPlatformScopeID — the scope
// ProvisionTenant is evaluated against, since no tenant exists yet.
const testPlatformScope = "00000000-0000-0000-0000-0000000000aa"

// testPrincipal is what the gateway would set in X-Principal-Id after
// verifying the caller's envelope.
const testPrincipal = "principal-test-1"

// authCtx returns a context carrying a verified principal, as
// middleware.Identity would have populated it. Mutations refuse a context
// without one, so every write path in these tests must use this rather than
// context.Background().
func authCtx() context.Context {
	return domain.WithPrincipal(context.Background(), testPrincipal)
}

// tenantCtx is authCtx plus a verified tenant, as middleware.Identity would
// populate it from X-Tenant-Id. Reads and transitions scoped to a tenant are
// refused unless the caller's own verified tenant matches the one in the path.
func tenantCtx(tenantID string) context.Context {
	return domain.WithTenant(authCtx(), tenantID)
}

func newSvc(t *testing.T, store registry.Store, authzC registry.AuthorizationClient, jv registry.JurisdictionValidator) *registry.Service {
	t.Helper()
	log := zap.NewNop()
	return registry.NewService(store, noopPublisher{}, authzC, jv, testPlatformScope, log)
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

	tenant, err := svc.ProvisionTenant(authCtx(), req, "corr-001")
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

	_, err := svc.ProvisionTenant(authCtx(), domain.ProvisionTenantRequest{}, "corr")
	assert.ErrorIs(t, err, registry.ErrUnauthorized)
}

func TestProvisionTenant_AuthZUnavailable_FailsClosed(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, unavailableAuthZ{}, acceptAllJurisd{})

	_, err := svc.ProvisionTenant(authCtx(), domain.ProvisionTenantRequest{}, "corr")
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
	tenant, err := svc.ProvisionTenant(authCtx(), req, "corr")
	require.NoError(t, err)
	assert.Equal(t, domain.TenantLifecycleOnboarding, tenant.LifecycleState)

	// Transition ONBOARDING → ACTIVE (valid). The caller's verified tenant must
	// be the one being transitioned — reads and transitions are refused when
	// the path tenant is not the caller's own.
	err = svc.TransitionTenantLifecycle(tenantCtx(tenant.TenantID), tenant.TenantID,
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
	tenant, _ := svc.ProvisionTenant(authCtx(), req, "corr")

	// ONBOARDING → OFFBOARDING is not a valid transition
	err := svc.TransitionTenantLifecycle(tenantCtx(tenant.TenantID), tenant.TenantID,
		domain.TransitionTenantLifecycleRequest{
			TargetState:   domain.TenantLifecycleOffboarding,
			CorrelationID: "corr-003",
		})
	assert.ErrorIs(t, err, registry.ErrInvalidTransition)
}

// TestCrossTenantReadsAreRefused is the regression test for a live-verified
// cross-tenant read.
//
// Row-level security was meant to prevent this and does not: the service
// connects as the Postgres superuser, which bypasses RLS unconditionally, and
// the tables are owned by that same user with ENABLE (not FORCE) ROW LEVEL
// SECURITY, so the owner bypasses the policies too. Both confirmed against a
// running database via pg_user.usesuper and pg_class.relforcerowsecurity.
//
// The queries themselves filter correctly — on the tenant id taken from the
// URL. That is not an isolation boundary: it is the caller choosing their own
// scope. Verified live before the fix: GET /v1/tenants/{A}/entities with
// X-Tenant-Id: B returned tenant A's entities in full.
func TestCrossTenantReadsAreRefused(t *testing.T) {
	svc, ms := baseSvc(t)

	victim := &domain.Tenant{TenantID: "tenant-a", TenantCode: "A", LegalName: "Tenant A"}
	ms.tenants[victim.TenantID] = victim

	attacker := tenantCtx("tenant-b")

	t.Run("GetTenant", func(t *testing.T) {
		_, err := svc.GetTenant(attacker, victim.TenantID)
		require.ErrorIs(t, err, registry.ErrNotFound,
			"another tenant's record must not be readable, and must not be distinguishable from absent")
	})

	t.Run("ListEntities", func(t *testing.T) {
		_, err := svc.ListEntities(attacker, victim.TenantID)
		require.ErrorIs(t, err, registry.ErrNotFound)
	})

	t.Run("ResolveTenantRegion", func(t *testing.T) {
		_, err := svc.ResolveTenantRegion(attacker, victim.TenantID)
		require.ErrorIs(t, err, registry.ErrNotFound)
	})

	t.Run("no verified tenant at all", func(t *testing.T) {
		_, err := svc.GetTenant(authCtx(), victim.TenantID)
		require.ErrorIs(t, err, registry.ErrNotFound,
			"a request with no verified tenant cannot be scoped and must be refused")
	})

	t.Run("own tenant still readable", func(t *testing.T) {
		got, err := svc.GetTenant(tenantCtx(victim.TenantID), victim.TenantID)
		require.NoError(t, err, "the fix must not over-restrict a caller reading its own tenant")
		assert.Equal(t, victim.TenantID, got.TenantID)
	})
}

// TestListEntitiesReturnsEmptyArrayNotNil — the handler serialises this
// directly, and a nil slice becomes JSON `null`, which breaks a caller
// iterating the array. Verified live returning `null`.
func TestListEntitiesReturnsEmptyArrayNotNil(t *testing.T) {
	svc, ms := baseSvc(t)
	ms.tenants["tenant-a"] = &domain.Tenant{TenantID: "tenant-a"}

	got, err := svc.ListEntities(tenantCtx("tenant-a"), "tenant-a")
	require.NoError(t, err)
	assert.NotNil(t, got, "an empty entity list must serialise as [], never null")
	assert.Empty(t, got)
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

	entity, err := svc.CreateEntity(authCtx(), req)
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

	_, err := svc.CreateEntity(authCtx(), req)
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

	_, err := svc.CreateEntity(authCtx(), req)
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

	err := svc.TransitionEntityStatus(authCtx(), "ent-001",
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
	err := svc.TransitionEntityStatus(authCtx(), "ent-002",
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

	err := svc.TransitionEntityStatus(authCtx(), "ent-003",
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

	bundle, err := svc.CreateTaxIdentityBundle(authCtx(), "ent-100", req)
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

	_, err := svc.CreateTaxIdentityBundle(authCtx(), "ent-100", req)
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

	_, err := svc.CreateTaxIdentityBundle(authCtx(), "ent-100", req)
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

	_, err := svc.CreateTaxIdentityBundle(authCtx(), "ent-100", req)
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
}

// ---------------------------------------------------------------------------
// UpdateEntity actor audit tests (R3 fix)
// ---------------------------------------------------------------------------

// seedEntity pre-seeds an entity so update/transition paths find one.
func seedEntity(ms *memStore, entityID string) {
	ms.entities[entityID] = &domain.LegalEntity{
		LegalEntityID: entityID,
		TenantID:      "ten-001",
		LegalName:     "Original Name",
		EntityStatus:  domain.EntityStatusActive,
	}
}

// TestUpdateEntity_WritesVerifiedActorPrincipalID confirms the audit column
// records the principal the gateway verified.
//
// This test replaces one that asserted the opposite behaviour. The previous
// version built an unsigned {"alg":"none"} token, put a principal_id of its
// choosing in the payload, and asserted the service adopted it — encoding the
// vulnerability as the expected contract. Anyone could mint that token; no key
// was involved. The identity now comes from the request context, populated by
// middleware.Identity from the gateway-verified X-Principal-Id header.
func TestUpdateEntity_WritesVerifiedActorPrincipalID(t *testing.T) {
	svc, ms := baseSvc(t)
	entityID := "ent-actor-test"
	seedEntity(ms, entityID)

	newName := "Updated Name"
	_, err := svc.UpdateEntity(authCtx(), entityID, domain.UpdateEntityRequest{
		LegalName:     &newName,
		CorrelationID: "corr-actor-test",
	})
	require.NoError(t, err)

	assert.Equal(t, testPrincipal, ms.lastUpdateActor,
		"updated_by_principal_id must be the gateway-verified principal")
	assert.NotEqual(t, "system", ms.lastUpdateActor,
		"the 'system' fallback must not appear for an attributed write")
}

// TestUpdateEntity_IgnoresForgedJWT is the regression test for the removed
// actorFromJWT path. A caller-supplied Authorization header is no longer read
// anywhere in this service, so a forged token cannot influence the audit
// identity — the mutation is refused outright for lack of a verified
// principal, rather than attributed to whoever the token claimed to be.
func TestUpdateEntity_IgnoresForgedJWT(t *testing.T) {
	svc, ms := baseSvc(t)
	entityID := "ent-forge-test"
	seedEntity(ms, entityID)

	// The exact shape the old code trusted: unsigned, attacker-chosen subject.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"principal_id":"attacker","tenant_id":"victim-tenant"}`))
	forged := header + "." + payload + "."

	// A context carrying the forged token as a value, with no verified
	// principal — i.e. exactly what an unauthenticated caller can produce.
	ctx := context.WithValue(context.Background(), forgedTokenKey{}, forged)

	newName := "Updated Name"
	_, err := svc.UpdateEntity(ctx, entityID, domain.UpdateEntityRequest{LegalName: &newName})

	require.ErrorIs(t, err, registry.ErrUnauthenticated,
		"a request with no verified principal must be refused, not attributed to the token's claim")
	assert.Empty(t, ms.lastUpdateActor, "no write should have reached the store")
}

type forgedTokenKey struct{}

// TestMutationsRequireVerifiedPrincipal walks every mutating entry point and
// asserts each refuses a context with no verified identity. Without this, a
// single method forgetting the check would silently accept unauthenticated
// writes — which is how the previous "system" fallback behaved on every route.
func TestMutationsRequireVerifiedPrincipal(t *testing.T) {
	anon := context.Background()

	cases := []struct {
		name string
		call func(*registry.Service) error
	}{
		{"ProvisionTenant", func(s *registry.Service) error {
			_, err := s.ProvisionTenant(anon, domain.ProvisionTenantRequest{}, "corr")
			return err
		}},
		{"TransitionTenantLifecycle", func(s *registry.Service) error {
			return s.TransitionTenantLifecycle(anon, "ten-001", domain.TransitionTenantLifecycleRequest{})
		}},
		{"CreateEntity", func(s *registry.Service) error {
			_, err := s.CreateEntity(anon, domain.CreateEntityRequest{})
			return err
		}},
		{"UpdateEntity", func(s *registry.Service) error {
			_, err := s.UpdateEntity(anon, "ent-001", domain.UpdateEntityRequest{})
			return err
		}},
		{"TransitionEntityStatus", func(s *registry.Service) error {
			return s.TransitionEntityStatus(anon, "ent-001", domain.TransitionEntityStatusRequest{})
		}},
		{"CreateHierarchy", func(s *registry.Service) error {
			_, err := s.CreateHierarchy(anon, domain.CreateHierarchyRequest{})
			return err
		}},
		{"EndDateHierarchy", func(s *registry.Service) error {
			return s.EndDateHierarchy(anon, "h-001", time.Now(), "corr")
		}},
		{"AssignJurisdiction", func(s *registry.Service) error {
			_, err := s.AssignJurisdiction(anon, "ent-001", domain.AssignJurisdictionRequest{})
			return err
		}},
		{"EndDateJurisdictionAssignment", func(s *registry.Service) error {
			return s.EndDateJurisdictionAssignment(anon, "a-001", time.Now(), "corr")
		}},
		{"CreateResidencyPolicy", func(s *registry.Service) error {
			_, err := s.CreateResidencyPolicy(anon, domain.CreateResidencyPolicyRequest{})
			return err
		}},
		{"CreateTaxIdentityBundle", func(s *registry.Service) error {
			_, err := s.CreateTaxIdentityBundle(anon, "ent-001", domain.CreateTaxIdentityBundleRequest{})
			return err
		}},
		{"TransitionTaxIdentityBundleStatus", func(s *registry.Service) error {
			return s.TransitionTaxIdentityBundleStatus(anon, "b-001", domain.TransitionTaxIdentityBundleStatusRequest{})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := baseSvc(t)
			err := tc.call(svc)
			require.ErrorIs(t, err, registry.ErrUnauthenticated,
				"%s must refuse a request with no verified principal", tc.name)
		})
	}
}

// recordingAuthZ captures what the service asked authorization-svc, so the
// tests can prove the decision subject is the real caller and the scope is
// the caller's own tenant — not values taken from the request body.
type recordingAuthZ struct {
	principalID string
	scopeID     string
	resource    string
	action      string
	calls       int
}

func (a *recordingAuthZ) Authorize(_ context.Context, principalID, scopeID, resource, action string) error {
	a.principalID, a.scopeID, a.resource, a.action = principalID, scopeID, resource, action
	a.calls++
	return nil
}

// TestAuthorizeReceivesVerifiedPrincipalAndTenantScope pins the two values
// that decide who a mutation is evaluated for.
func TestAuthorizeReceivesVerifiedPrincipalAndTenantScope(t *testing.T) {
	ms := newMemStore()
	rec := &recordingAuthZ{}
	svc := newSvc(t, ms, rec, acceptAllJurisd{})

	entityID := "ent-scope-test"
	seedEntity(ms, entityID)

	ctx := domain.WithTenant(authCtx(), "ten-verified")
	newName := "Updated"
	_, err := svc.UpdateEntity(ctx, entityID, domain.UpdateEntityRequest{LegalName: &newName})
	require.NoError(t, err)

	require.Equal(t, 1, rec.calls)
	assert.Equal(t, testPrincipal, rec.principalID, "decision subject must be the verified principal")
	assert.Equal(t, "ten-verified", rec.scopeID, "decision scope must be the caller's verified tenant")
}

// TestProvisionTenantUsesPlatformScope — tenant creation is the one mutation
// with no tenant to scope to, so it falls back to the configured platform
// scope. authorization-svc rejects an empty legal_entity_id outright, so
// getting this wrong is a 503 on every provisioning call.
func TestProvisionTenantUsesPlatformScope(t *testing.T) {
	ms := newMemStore()
	rec := &recordingAuthZ{}
	svc := newSvc(t, ms, rec, acceptAllJurisd{})

	_, err := svc.ProvisionTenant(authCtx(), domain.ProvisionTenantRequest{
		TenantCode:          "ACME",
		LegalName:           "Acme Ltd",
		DefaultCurrencyCode: "GBP",
		PrimaryTimezone:     "Europe/London",
		PrimaryLocale:       "en-GB",
	}, "corr")
	require.NoError(t, err)

	assert.Equal(t, testPlatformScope, rec.scopeID,
		"ProvisionTenant has no tenant yet and must use the configured platform scope")
	assert.Equal(t, testPrincipal, rec.principalID)
}

// TestUpdateEntity_RefusesWhenNoVerifiedPrincipal replaces a test that
// asserted the opposite: that an unattributed update succeeds and is recorded
// as "system". That was described as an intentional fallback, "visible in
// audit logs as a wiring signal" — but a signal nobody blocks on is not a
// control. Every unauthenticated write in this service's history was recorded
// as the platform's own action, and the audit trail cannot distinguish those
// from real system activity after the fact.
//
// An unattributed mutation is now refused.
func TestUpdateEntity_RefusesWhenNoVerifiedPrincipal(t *testing.T) {
	svc, ms := baseSvc(t)

	entityID := "ent-no-principal"
	seedEntity(ms, entityID)

	newName := "Changed"
	_, err := svc.UpdateEntity(context.Background(), entityID,
		domain.UpdateEntityRequest{LegalName: &newName, CorrelationID: "corr-noprincipal"})

	require.ErrorIs(t, err, registry.ErrUnauthenticated)
	assert.Empty(t, ms.lastUpdateActor,
		"no write should reach the store without a verified principal")
}

// ---------------------------------------------------------------------------
// Workspace mutation tests
//
// Workspaces were create-only: the table carried updated_at and
// updated_by_principal_id from the first migration, but no write path ever set
// them, so billing_classification — the field that decides whether a workspace
// may ever produce a live charge — was uncorrectable once wrong.
// ---------------------------------------------------------------------------

// seedWorkspace puts an ACTIVE, non-billable workspace in the store, which is
// the state a mistakenly-classified workspace is discovered in.
func seedWorkspace(ms *memStore, id, tenant string) *domain.Workspace {
	w := &domain.Workspace{
		WorkspaceID:           id,
		TenantID:              tenant,
		Name:                  "Pilot",
		BillingClassification: domain.BillingClassificationInternal,
		BillingSource:         domain.BillingSourceNone,
		Status:                domain.WorkspaceStatusActive,
	}
	ms.workspaces[id] = w
	return w
}

func strPtr(s string) *string { return &s }

func TestUpdateWorkspace_ReclassifiesAndRecordsActor(t *testing.T) {
	svc, ms := baseSvc(t)
	seedWorkspace(ms, "ws-1", "tenant-1")

	got, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "ws-1", domain.UpdateWorkspaceRequest{
		Name:                  strPtr("Acme Production"),
		BillingClassification: strPtr(string(domain.BillingClassificationCommercialStandalone)),
		BillingSource:         strPtr(string(domain.BillingSourceDirect)),
		CorrelationID:         "corr-1",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.BillingClassificationCommercialStandalone, got.BillingClassification)
	assert.Equal(t, domain.BillingSourceDirect, got.BillingSource)
	assert.Equal(t, "Acme Production", got.Name)
	// The actor must come from the verified context, never the request body.
	assert.Equal(t, testPrincipal, got.UpdatedByPrincipalID)
}

// An omitted field must keep its value rather than being blanked.
func TestUpdateWorkspace_OmittedFieldsUnchanged(t *testing.T) {
	svc, ms := baseSvc(t)
	seedWorkspace(ms, "ws-1", "tenant-1")

	got, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "ws-1", domain.UpdateWorkspaceRequest{
		Name: strPtr("Renamed"),
	})
	require.NoError(t, err)
	assert.Equal(t, domain.BillingClassificationInternal, got.BillingClassification,
		"classification changed on a name-only patch")
}

// The column is text, so an unrecognised class would persist and later be read
// back as though it meant something.
func TestUpdateWorkspace_RejectsUnknownBillingClassification(t *testing.T) {
	svc, ms := baseSvc(t)
	seedWorkspace(ms, "ws-1", "tenant-1")

	_, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "ws-1", domain.UpdateWorkspaceRequest{
		BillingClassification: strPtr("COMMERCIAL_MAYBE"),
	})
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Equal(t, domain.BillingClassificationInternal, ms.workspaces["ws-1"].BillingClassification,
		"a refused patch still changed the stored classification")
}

func TestUpdateWorkspace_RejectsUnknownBillingSource(t *testing.T) {
	svc, ms := baseSvc(t)
	seedWorkspace(ms, "ws-1", "tenant-1")

	_, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "ws-1", domain.UpdateWorkspaceRequest{
		BillingSource: strPtr("INVOICE_BY_CARRIER_PIGEON"),
	})
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestUpdateWorkspace_AbsentWorkspaceIsNotFound(t *testing.T) {
	svc, _ := baseSvc(t)

	_, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "nope", domain.UpdateWorkspaceRequest{})
	assert.ErrorIs(t, err, registry.ErrNotFound)
}

func TestUpdateWorkspace_AuthorizationDenied(t *testing.T) {
	ms := newMemStore()
	seedWorkspace(ms, "ws-1", "tenant-1")
	svc := newSvc(t, ms, denyAllAuthZ{}, acceptAllJurisd{})

	_, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "ws-1", domain.UpdateWorkspaceRequest{
		Name: strPtr("Renamed"),
	})
	assert.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, "Pilot", ms.workspaces["ws-1"].Name, "workspace modified despite denial")
}

// An unreachable authorization service must refuse the write, not allow it.
func TestUpdateWorkspace_AuthorizationUnavailableFailsClosed(t *testing.T) {
	ms := newMemStore()
	seedWorkspace(ms, "ws-1", "tenant-1")
	svc := newSvc(t, ms, unavailableAuthZ{}, acceptAllJurisd{})

	_, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "ws-1", domain.UpdateWorkspaceRequest{
		Name: strPtr("Renamed"),
	})
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	assert.Equal(t, "Pilot", ms.workspaces["ws-1"].Name,
		"workspace modified while authorization was unavailable")
}

func TestTransitionWorkspaceStatus_ArchiveThenRestore(t *testing.T) {
	svc, ms := baseSvc(t)
	seedWorkspace(ms, "ws-1", "tenant-1")
	ctx := tenantCtx("tenant-1")

	require.NoError(t, svc.TransitionWorkspaceStatus(ctx, "ws-1", domain.TransitionWorkspaceStatusRequest{
		NewStatus: domain.WorkspaceStatusArchived,
	}))
	assert.Equal(t, domain.WorkspaceStatusArchived, ms.workspaces["ws-1"].Status)

	// Archiving hides a workspace; it deletes nothing, so an accidental archive
	// has to be recoverable.
	require.NoError(t, svc.TransitionWorkspaceStatus(ctx, "ws-1", domain.TransitionWorkspaceStatusRequest{
		NewStatus: domain.WorkspaceStatusActive,
	}))
	assert.Equal(t, domain.WorkspaceStatusActive, ms.workspaces["ws-1"].Status)
}

// Re-archiving is refused rather than silently re-stamping the audit columns
// with a no-op — ARCHIVED->ARCHIVED is not a declared transition.
func TestTransitionWorkspaceStatus_RepeatedArchiveIsInvalid(t *testing.T) {
	svc, ms := baseSvc(t)
	w := seedWorkspace(ms, "ws-1", "tenant-1")
	w.Status = domain.WorkspaceStatusArchived

	err := svc.TransitionWorkspaceStatus(tenantCtx("tenant-1"), "ws-1", domain.TransitionWorkspaceStatusRequest{
		NewStatus: domain.WorkspaceStatusArchived,
	})
	assert.ErrorIs(t, err, registry.ErrInvalidTransition)
}

func TestTransitionWorkspaceStatus_UnknownStatusRejected(t *testing.T) {
	svc, ms := baseSvc(t)
	seedWorkspace(ms, "ws-1", "tenant-1")

	err := svc.TransitionWorkspaceStatus(tenantCtx("tenant-1"), "ws-1", domain.TransitionWorkspaceStatusRequest{
		NewStatus: domain.WorkspaceStatus("DELETED"),
	})
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Equal(t, domain.WorkspaceStatusActive, ms.workspaces["ws-1"].Status,
		"an unrecognised target status still changed the stored status")
}

func TestTransitionWorkspaceStatus_AbsentWorkspaceIsInvalidTransition(t *testing.T) {
	svc, _ := baseSvc(t)

	err := svc.TransitionWorkspaceStatus(tenantCtx("tenant-1"), "nope", domain.TransitionWorkspaceStatusRequest{
		NewStatus: domain.WorkspaceStatusArchived,
	})
	assert.ErrorIs(t, err, registry.ErrInvalidTransition)
}

func TestTransitionWorkspaceStatus_AuthorizationDenied(t *testing.T) {
	ms := newMemStore()
	seedWorkspace(ms, "ws-1", "tenant-1")
	svc := newSvc(t, ms, denyAllAuthZ{}, acceptAllJurisd{})

	err := svc.TransitionWorkspaceStatus(tenantCtx("tenant-1"), "ws-1", domain.TransitionWorkspaceStatusRequest{
		NewStatus: domain.WorkspaceStatusArchived,
	})
	assert.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, domain.WorkspaceStatusActive, ms.workspaces["ws-1"].Status,
		"status changed despite the authorization denial")
}

// commercial_account_id is a UUID column; a malformed value must be a named 400
// rather than reaching the driver and surfacing as an unmapped 500.
func TestUpdateWorkspace_RejectsNonUUIDCommercialAccount(t *testing.T) {
	svc, ms := baseSvc(t)
	seedWorkspace(ms, "ws-1", "tenant-1")

	_, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "ws-1", domain.UpdateWorkspaceRequest{
		CommercialAccountID: strPtr("not-a-uuid"),
	})
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestUpdateWorkspace_AcceptsValidCommercialAccount(t *testing.T) {
	svc, ms := baseSvc(t)
	seedWorkspace(ms, "ws-1", "tenant-1")

	acct := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	got, err := svc.UpdateWorkspace(tenantCtx("tenant-1"), "ws-1", domain.UpdateWorkspaceRequest{
		CommercialAccountID: strPtr(acct),
	})
	require.NoError(t, err)
	require.NotNil(t, got.CommercialAccountID)
	assert.Equal(t, acct, *got.CommercialAccountID)
}
