package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies both migrations
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS tax_identity_bundles, entity_jurisdiction_assignments, entity_hierarchies, legal_entities, data_residency_policies, tenants, residency_regions CASCADE;`)

	for _, mig := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_tenant_id_to_junction_tables.up.sql",
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
		PrimaryJurisdictionID: uuid.New().String(), // no FK — validated via HTTP elsewhere, not DB
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
}


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
			ResidencyMode: domain.ResidencyModeFollowEntity,
			ConflictResolutionMode: domain.ConflictResolutionFailClosed,
			ActiveFlag: true, CreatedByPrincipalID: "test-admin",
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
