package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies all migrations
// from a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set —
// same convention as jurisdiction-rules-svc and identity-context-svc.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS workspaces, tax_identity_bundles, entity_jurisdiction_assignments, entity_hierarchies, legal_entities, data_residency_policies, tenants, residency_regions CASCADE;`)

	for _, mig := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_tenant_id_to_junction_tables.up.sql",
		"000003_add_residency_region_to_policies.up.sql",
		"000004_add_data_classification.up.sql",
		"000005_add_workspaces.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", mig))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", mig, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", mig, err)
		}
	}

	return pool
}

func TestPgStore_CreateTenant_And_GetTenantByID(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenant := &domain.Tenant{
		TenantID:                     uuid.New().String(),
		TenantCode:                   "ACME",
		LegalName:                    "Acme Corp",
		Status:                       domain.TenantStatusActive,
		DefaultCurrencyCode:          "USD",
		PrimaryTimezone:              "UTC",
		PrimaryLocale:                "en-US",
		DefaultDataResidencyPolicyID: uuid.New().String(), // no FK on this column — any UUID is valid
		LifecycleState:               domain.TenantLifecycleOnboarding,
		CreatedByPrincipalID:         "test-admin",
	}

	ctx = domain.WithTenant(ctx, tenant.TenantID) // RLS needs this set even for the tenant's own creation
	if err := s.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant failed: %v", err)
	}

	got, err := s.GetTenantByID(ctx, tenant.TenantID)
	if err != nil || got == nil {
		t.Fatalf("GetTenantByID failed: %v", err)
	}
	if got.TenantCode != "ACME" {
		t.Errorf("expected tenant_code ACME, got %q", got.TenantCode)
	}
}

func TestPgStore_CreateEntity_And_GetEntityByID(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx = domain.WithTenant(ctx, tenantID)

	tenant := &domain.Tenant{
		TenantID: tenantID, TenantCode: "ACME2", LegalName: "Acme Two",
		Status: domain.TenantStatusActive, DefaultCurrencyCode: "USD",
		PrimaryTimezone: "UTC", PrimaryLocale: "en-US",
		DefaultDataResidencyPolicyID: uuid.New().String(),
		LifecycleState:               domain.TenantLifecycleOnboarding,
		CreatedByPrincipalID:         "test-admin",
	}
	if err := s.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant failed: %v", err)
	}

	policy := &domain.DataResidencyPolicy{
		DataResidencyPolicyID:  uuid.New().String(),
		TenantID:               tenantID,
		PolicyName:             "Default",
		PolicyCode:             "DEFAULT",
		ResidencyMode:          domain.ResidencyModeFollowEntity,
		ConflictResolutionMode: domain.ConflictResolutionFailClosed,
		ActiveFlag:             true,
		CreatedByPrincipalID:   "test-admin",
	}
	if err := s.CreateResidencyPolicy(ctx, policy); err != nil {
		t.Fatalf("CreateResidencyPolicy failed: %v", err)
	}

	entity := &domain.LegalEntity{
		LegalEntityID:         uuid.New().String(),
		TenantID:              tenantID,
		EntityCode:            "ACME-US",
		LegalName:             "Acme US Inc",
		EntityType:            domain.EntityTypeSubsidiary,
		DefaultCurrencyCode:   "USD",
		FiscalCalendarID:      uuid.New().String(), // no FK on this one
		EntityStatus:          domain.EntityStatusActive,
		PrimaryJurisdictionID: uuid.New().String(),          // no FK — validated via HTTP elsewhere, not DB
		DataResidencyPolicyID: policy.DataResidencyPolicyID, // MUST be real — this one has an FK
		CreatedByPrincipalID:  "test-admin",
	}
	if err := s.CreateEntity(ctx, entity); err != nil {
		t.Fatalf("CreateEntity failed: %v", err)
	}

	got, err := s.GetEntityByID(ctx, entity.LegalEntityID)
	if err != nil || got == nil {
		t.Fatalf("GetEntityByID failed: %v", err)
	}
	if got.EntityCode != "ACME-US" {
		t.Errorf("expected entity_code ACME-US, got %q", got.EntityCode)
	}
} // TestPgStore_RLS_TenantIsolation tests RLS tenant isolation using ListEntitiesByTenant.
// NOTE: This test originally passed even before the superuser bypass was fixed
// because ListEntitiesByTenant happened to have an explicit 'WHERE tenant_id = $1'
// filter in its SQL query. It did NOT catch leaks in methods that were missing
// explicit tenant_id filters (like GetEntityByID, GetTaxIdentityBundleByID, etc.),
// which were successfully bypassed by the superuser connection.
//
// For complete isolation coverage of all audited methods, see internal/store/tenant_isolation_test.go.
func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	setupTenantWithEntity := func(tenantCode string) (tenantID, entityID string) {
		tenantID = uuid.New().String()
		tctx := domain.WithTenant(ctx, tenantID)

		tenant := &domain.Tenant{
			TenantID: tenantID, TenantCode: tenantCode, LegalName: tenantCode,
			Status: domain.TenantStatusActive, DefaultCurrencyCode: "USD",
			PrimaryTimezone: "UTC", PrimaryLocale: "en-US",
			DefaultDataResidencyPolicyID: uuid.New().String(),
			LifecycleState:               domain.TenantLifecycleOnboarding,
			CreatedByPrincipalID:         "test-admin",
		}
		if err := s.CreateTenant(tctx, tenant); err != nil {
			t.Fatalf("CreateTenant(%s) failed: %v", tenantCode, err)
		}

		policy := &domain.DataResidencyPolicy{
			DataResidencyPolicyID: uuid.New().String(), TenantID: tenantID,
			PolicyName: "Default", PolicyCode: tenantCode + "-POLICY",
			ResidencyMode:          domain.ResidencyModeFollowEntity,
			ConflictResolutionMode: domain.ConflictResolutionFailClosed,
			ActiveFlag:             true, CreatedByPrincipalID: "test-admin",
		}
		if err := s.CreateResidencyPolicy(tctx, policy); err != nil {
			t.Fatalf("CreateResidencyPolicy(%s) failed: %v", tenantCode, err)
		}

		entityID = uuid.New().String()
		entity := &domain.LegalEntity{
			LegalEntityID: entityID, TenantID: tenantID,
			EntityCode: tenantCode + "-E1", LegalName: tenantCode + " Entity",
			EntityType: domain.EntityTypeSubsidiary, DefaultCurrencyCode: "USD",
			FiscalCalendarID: uuid.New().String(), EntityStatus: domain.EntityStatusActive,
			PrimaryJurisdictionID: uuid.New().String(),
			DataResidencyPolicyID: policy.DataResidencyPolicyID,
			CreatedByPrincipalID:  "test-admin",
		}
		if err := s.CreateEntity(tctx, entity); err != nil {
			t.Fatalf("CreateEntity(%s) failed: %v", tenantCode, err)
		}
		return tenantID, entityID
	}

	tenantA, entityA := setupTenantWithEntity("TENANT-A")
	_, entityB := setupTenantWithEntity("TENANT-B")

	// Query AS TENANT A. This is the actual test: does RLS hide tenant B's row?
	ctxAsA := domain.WithTenant(ctx, tenantA)
	entities, err := s.ListEntitiesByTenant(ctxAsA, tenantA)
	if err != nil {
		t.Fatalf("ListEntitiesByTenant failed: %v", err)
	}

	foundA, foundB := false, false
	for _, e := range entities {
		if e.LegalEntityID == entityA {
			foundA = true
		}
		if e.LegalEntityID == entityB {
			foundB = true
		}
	}
	if !foundA {
		t.Error("expected to see tenant A's own entity, but it was missing")
	}
	if foundB {
		t.Fatal("RLS FAILURE: tenant A's query returned tenant B's entity — tenant isolation is broken")
	}
}

// ── Workspace mutation (SQL exercised against real Postgres) ─────────────────
//
// These cover the two statements added with the workspace write path: the
// COALESCE patch and the CTE-guarded status transition. Both are only
// meaningful against a real database — the CTE's FOR UPDATE and the
// ANY($n::text[]) prior-state guard have no in-memory equivalent, so a stub
// reimplementation would be testing the stub.

// seedTenantAndWorkspace creates the minimum rows a workspace needs and
// returns the tenant id and workspace id.
func seedTenantAndWorkspace(t *testing.T, ctx context.Context, s *store.PgStore, class domain.BillingClassification) (string, string, context.Context) {
	t.Helper()
	tenantID := uuid.New().String()
	ctx = domain.WithTenant(ctx, tenantID)

	tenant := &domain.Tenant{
		TenantID: tenantID, TenantCode: "WS" + tenantID[:4], LegalName: "Workspace Co",
		Status: domain.TenantStatusActive, DefaultCurrencyCode: "USD",
		PrimaryTimezone: "UTC", PrimaryLocale: "en-US",
		DefaultDataResidencyPolicyID: uuid.New().String(),
		LifecycleState:               domain.TenantLifecycleOnboarding,
		CreatedByPrincipalID:         "test-admin",
	}
	if err := s.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	wsID := uuid.New().String()
	unit := "Pilots"
	if err := s.CreateWorkspace(ctx, &domain.Workspace{
		WorkspaceID:           wsID,
		TenantID:              tenantID,
		Name:                  "Pilot",
		BusinessUnit:          &unit,
		BillingClassification: class,
		BillingSource:         domain.BillingSourceNone,
		Status:                domain.WorkspaceStatusActive,
		CreatedAt:             time.Now().UTC(),
		CreatedByPrincipalID:  "test-admin",
	}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return tenantID, wsID, ctx
}

func TestPgStore_UpdateWorkspace_ReclassifiesAndStampsActor(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	_, wsID, ctx := seedTenantAndWorkspace(t, ctx, s, domain.BillingClassificationInternal)

	name := "Acme Production"
	class := string(domain.BillingClassificationCommercialStandalone)
	got, err := s.UpdateWorkspace(ctx, wsID, domain.UpdateWorkspaceRequest{
		Name:                  &name,
		BillingClassification: &class,
		ActorPrincipalID:      "principal-7",
	})
	if err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if got.Name != name {
		t.Errorf("name = %q, want %q", got.Name, name)
	}
	if got.BillingClassification != domain.BillingClassificationCommercialStandalone {
		t.Errorf("classification = %q, want COMMERCIAL_STANDALONE", got.BillingClassification)
	}
	if got.UpdatedByPrincipalID != "principal-7" {
		t.Errorf("updated_by_principal_id = %q, want principal-7", got.UpdatedByPrincipalID)
	}
	// This is the COALESCE contract: an omitted column keeps its value.
	if got.BusinessUnit == nil || *got.BusinessUnit != "Pilots" {
		t.Errorf("business_unit was not preserved on a partial patch: %v", got.BusinessUnit)
	}
}

// A workspace in another tenant must not be reachable, tenant filter and RLS
// both considered. This is the isolation guarantee the whole registry rests on.
func TestPgStore_UpdateWorkspace_CrossTenantRefused(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	_, wsID, _ := seedTenantAndWorkspace(t, ctx, s, domain.BillingClassificationInternal)

	// A second tenant, whose context must not reach the first tenant's row.
	otherTenant := uuid.New().String()
	otherCtx := domain.WithTenant(context.Background(), otherTenant)
	if err := s.CreateTenant(otherCtx, &domain.Tenant{
		TenantID: otherTenant, TenantCode: "OTHER", LegalName: "Other Co",
		Status: domain.TenantStatusActive, DefaultCurrencyCode: "USD",
		PrimaryTimezone: "UTC", PrimaryLocale: "en-US",
		DefaultDataResidencyPolicyID: uuid.New().String(),
		LifecycleState:               domain.TenantLifecycleOnboarding,
		CreatedByPrincipalID:         "test-admin",
	}); err != nil {
		t.Fatalf("CreateTenant(other): %v", err)
	}

	name := "Hijacked"
	got, err := s.UpdateWorkspace(otherCtx, wsID, domain.UpdateWorkspaceRequest{
		Name: &name, ActorPrincipalID: "attacker",
	})
	if err != nil {
		t.Fatalf("UpdateWorkspace returned an error rather than no-rows: %v", err)
	}
	if got != nil {
		t.Fatalf("a foreign tenant updated another tenant's workspace: %+v", got)
	}
}

func TestPgStore_UpdateWorkspace_AbsentReturnsNil(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	_, _, ctx = seedTenantAndWorkspace(t, ctx, s, domain.BillingClassificationInternal)

	name := "Nope"
	got, err := s.UpdateWorkspace(ctx, uuid.New().String(), domain.UpdateWorkspaceRequest{Name: &name})
	if err != nil {
		t.Fatalf("expected no error for a missing row, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for a missing workspace, got %+v", got)
	}
}

// The guarded transition: it must apply only from an allowed prior state, and
// report the state it moved away from.
func TestPgStore_TransitionWorkspaceStatus_ReturnsPreviousStatus(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	_, wsID, ctx := seedTenantAndWorkspace(t, ctx, s, domain.BillingClassificationInternal)

	affected, previous, err := s.TransitionWorkspaceStatus(ctx, wsID,
		domain.WorkspaceStatusArchived,
		[]domain.WorkspaceStatus{domain.WorkspaceStatusActive},
		"principal-7", "corr-1")
	if err != nil {
		t.Fatalf("TransitionWorkspaceStatus: %v", err)
	}
	if affected != 1 {
		t.Fatalf("rowsAffected = %d, want 1", affected)
	}
	if previous != domain.WorkspaceStatusActive {
		t.Fatalf("previous = %q, want ACTIVE", previous)
	}

	got, err := s.GetWorkspaceByID(ctx, wsID)
	if err != nil || got == nil {
		t.Fatalf("GetWorkspaceByID: %v", err)
	}
	if got.Status != domain.WorkspaceStatusArchived {
		t.Fatalf("status = %q, want ARCHIVED", got.Status)
	}
	if got.UpdatedByPrincipalID != "principal-7" {
		t.Fatalf("updated_by_principal_id = %q, want principal-7", got.UpdatedByPrincipalID)
	}
}

// The prior-state guard is the whole point: a transition from a state not in
// the allowed set must touch nothing and report zero rows.
func TestPgStore_TransitionWorkspaceStatus_DisallowedPriorTouchesNothing(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	_, wsID, ctx := seedTenantAndWorkspace(t, ctx, s, domain.BillingClassificationInternal)

	// The workspace is ACTIVE; only ARCHIVED is allowed as a prior state here.
	affected, previous, err := s.TransitionWorkspaceStatus(ctx, wsID,
		domain.WorkspaceStatusActive,
		[]domain.WorkspaceStatus{domain.WorkspaceStatusArchived},
		"principal-7", "corr-1")
	if err != nil {
		t.Fatalf("TransitionWorkspaceStatus: %v", err)
	}
	if affected != 0 {
		t.Fatalf("rowsAffected = %d, want 0 for a disallowed prior state", affected)
	}
	if previous != "" {
		t.Fatalf("previous = %q, want empty when nothing was updated", previous)
	}

	got, _ := s.GetWorkspaceByID(ctx, wsID)
	if got.UpdatedByPrincipalID == "principal-7" {
		t.Fatal("a refused transition still stamped updated_by_principal_id")
	}
}

func TestPgStore_TransitionWorkspaceStatus_AbsentReturnsZero(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	_, _, ctx = seedTenantAndWorkspace(t, ctx, s, domain.BillingClassificationInternal)

	affected, _, err := s.TransitionWorkspaceStatus(ctx, uuid.New().String(),
		domain.WorkspaceStatusArchived,
		[]domain.WorkspaceStatus{domain.WorkspaceStatusActive},
		"principal-7", "corr-1")
	if err != nil {
		t.Fatalf("expected no error for a missing row, got %v", err)
	}
	if affected != 0 {
		t.Fatalf("rowsAffected = %d, want 0", affected)
	}
}
