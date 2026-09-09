package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
	"zoiko.io/inventory-management-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies every migration
// from a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set —
// same convention as every other service in this platform.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS
		inventory_location_hierarchy, inventory_locations,
		inventory_valuation_policies, inventory_tracking_policies, inventory_items
		CASCADE;`)

	migrationDir := filepath.Join(base, "../../deployments/migrations")
	migrations, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("failed to glob migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatalf("no *.up.sql migrations found under %s", migrationDir)
	}
	sort.Strings(migrations)
	for _, migration := range migrations {
		sql, err := os.ReadFile(migration)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", filepath.Base(migration), err)
		}
	}
	return pool
}

func newTestItem(tenantID, legalEntityID, sku string) *domain.InventoryItem {
	return &domain.InventoryItem{
		ItemID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		SKU: sku, Description: "Widget", BaseUOM: "EACH",
		Status: domain.ItemStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
}

func TestPgStore_CreateItem_And_GetItem(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	it := newTestItem(tenantID, legalEntityID, "SKU-1")
	if err := s.CreateItem(ctx, it); err != nil {
		t.Fatalf("CreateItem failed: %v", err)
	}
	got, err := s.GetItem(ctx, it.ItemID)
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	if got.SKU != "SKU-1" || got.Status != domain.ItemStatusDraft {
		t.Fatalf("unexpected item: %+v", got)
	}
}

// Negative path (derived): duplicate SKU within the same legal entity is
// refused by the real UNIQUE(tenant_id, legal_entity_id, sku) constraint.
func TestPgStore_CreateItem_DuplicateSKU_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first := newTestItem(tenantID, legalEntityID, "SKU-DUP")
	if err := s.CreateItem(ctx, first); err != nil {
		t.Fatalf("first CreateItem failed: %v", err)
	}
	second := newTestItem(tenantID, legalEntityID, "SKU-DUP")
	if err := s.CreateItem(ctx, second); err != domain.ErrDuplicateSKU {
		t.Fatalf("expected ErrDuplicateSKU, got %v", err)
	}
}

func TestPgStore_ActivateItem_RequiresDraft(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	it := newTestItem(tenantID, legalEntityID, "SKU-2")
	if err := s.CreateItem(ctx, it); err != nil {
		t.Fatalf("CreateItem failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ActivateItem(ctx, it.ItemID, "approver-1", now); err != nil {
		t.Fatalf("first activate failed: %v", err)
	}
	if err := s.ActivateItem(ctx, it.ItemID, "approver-1", now); err != domain.ErrInvalidItemTransition {
		t.Fatalf("expected ErrInvalidItemTransition re-activating, got %v", err)
	}
}

// TestPgStore_SetTrackingPolicy_Versions proves SetTrackingPolicy never
// mutates a prior version in place — the old row is end-dated and a new
// one takes over, exactly the versioned-entity pattern migration 000001
// documents.
func TestPgStore_SetTrackingPolicy_Versions(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	it := newTestItem(tenantID, legalEntityID, "SKU-3")
	if err := s.CreateItem(ctx, it); err != nil {
		t.Fatalf("CreateItem failed: %v", err)
	}

	now := time.Now().UTC()
	v1 := &domain.TrackingPolicy{PolicyVersionID: uuid.New().String(), ItemID: it.ItemID, RequiresLotTracking: false, EffectiveFrom: now, CreatedAt: now, CreatedByPrincipalID: "preparer-1"}
	if err := s.SetTrackingPolicy(ctx, v1, now); err != nil {
		t.Fatalf("SetTrackingPolicy v1 failed: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("expected version 1, got %d", v1.Version)
	}

	later := now.Add(time.Hour)
	v2 := &domain.TrackingPolicy{PolicyVersionID: uuid.New().String(), ItemID: it.ItemID, RequiresLotTracking: true, EffectiveFrom: later, CreatedAt: later, CreatedByPrincipalID: "preparer-1"}
	if err := s.SetTrackingPolicy(ctx, v2, later); err != nil {
		t.Fatalf("SetTrackingPolicy v2 failed: %v", err)
	}
	if v2.Version != 2 || v2.PolicyID != v1.PolicyID {
		t.Fatalf("expected v2 to be version 2 of the same logical policy, got version=%d policy_id=%s (v1=%s)", v2.Version, v2.PolicyID, v1.PolicyID)
	}

	current, err := s.GetCurrentTrackingPolicy(ctx, it.ItemID)
	if err != nil {
		t.Fatalf("GetCurrentTrackingPolicy failed: %v", err)
	}
	if !current.RequiresLotTracking || current.Version != 2 {
		t.Fatalf("expected current policy to be v2 with lot tracking required, got %+v", current)
	}
}

// TestPgStore_GetProfileAsOf_ReturnsHistoricalVersion is the real proof of
// migration 000001's negative path #2, "Historic movement reinterpreted
// using new UOM mapping" (generalized to policy versioning): a query for
// an instant BEFORE the second version's effective_from must still return
// the FIRST version, never the current one.
func TestPgStore_GetProfileAsOf_ReturnsHistoricalVersion(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	it := newTestItem(tenantID, legalEntityID, "SKU-4")
	if err := s.CreateItem(ctx, it); err != nil {
		t.Fatalf("CreateItem failed: %v", err)
	}

	past := time.Now().UTC().Add(-48 * time.Hour)
	v1 := &domain.ValuationPolicy{PolicyVersionID: uuid.New().String(), ItemID: it.ItemID, ValuationMethod: domain.ValuationMethodFIFO, EffectiveFrom: past, CreatedAt: past, CreatedByPrincipalID: "preparer-1"}
	if err := s.SetValuationPolicy(ctx, v1); err != nil {
		t.Fatalf("SetValuationPolicy v1 failed: %v", err)
	}

	future := time.Now().UTC().Add(24 * time.Hour)
	v2 := &domain.ValuationPolicy{PolicyVersionID: uuid.New().String(), ItemID: it.ItemID, ValuationMethod: domain.ValuationMethodWeightedAverage, EffectiveFrom: future, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1"}
	if err := s.SetValuationPolicy(ctx, v2); err != nil {
		t.Fatalf("SetValuationPolicy v2 failed: %v", err)
	}

	// "As of" a moment in between the two effective windows must return v1.
	asOf := time.Now().UTC()
	_, vp, err := s.GetProfileAsOf(ctx, it.ItemID, asOf)
	if err != nil {
		t.Fatalf("GetProfileAsOf failed: %v", err)
	}
	if vp == nil || vp.ValuationMethod != domain.ValuationMethodFIFO {
		t.Fatalf("expected the historic FIFO version to still be returned as-of %v, got %+v", asOf, vp)
	}

	// "As of" a moment after v2 takes effect must return v2.
	_, vpFuture, err := s.GetProfileAsOf(ctx, it.ItemID, future.Add(time.Hour))
	if err != nil {
		t.Fatalf("GetProfileAsOf (future) failed: %v", err)
	}
	if vpFuture == nil || vpFuture.ValuationMethod != domain.ValuationMethodWeightedAverage {
		t.Fatalf("expected WEIGHTED_AVERAGE to govern after its own effective_from, got %+v", vpFuture)
	}
}
