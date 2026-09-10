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

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
	"zoiko.io/asset-management-svc/internal/store"
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
		asset_events,
		depreciation_lines, depreciation_run_population, depreciation_runs, depreciation_schedules,
		asset_book_assignments, asset_components, fixed_assets
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

func newTestAsset(tenantID, legalEntityID string) *domain.FixedAsset {
	now := time.Now().UTC()
	return &domain.FixedAsset{
		AssetID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		AssetCategory: "IT Equipment", Description: "Server rack",
		Status: domain.AssetStatusCandidate, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
}

func TestPgStore_CreateAsset_And_GetAsset(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := newTestAsset(tenantID, "le-1")

	if err := s.CreateAsset(ctx, a); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	got, err := s.GetAsset(ctx, a.AssetID)
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	if got.Status != domain.AssetStatusCandidate || got.Description != "Server rack" {
		t.Fatalf("expected the created asset to round-trip, got %+v", got)
	}
}

func TestPgStore_TransitionAsset_WrongFromStatus_Rejected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := newTestAsset(tenantID, "le-1")
	if err := s.CreateAsset(ctx, a); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}

	// CapitalizeAsset requires REGISTERED, but this asset is still CANDIDATE.
	if err := s.CapitalizeAsset(ctx, a.AssetID, "approver-1", time.Now().UTC()); err == nil {
		t.Fatal("expected CapitalizeAsset to be refused from CANDIDATE")
	}
}

func TestPgStore_FullLifecycle_CandidateToActiveToSuspended(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := newTestAsset(tenantID, "le-1")
	if err := s.CreateAsset(ctx, a); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}

	now := time.Now().UTC()
	if err := s.RegisterAsset(ctx, a.AssetID, "approver-1", now); err != nil {
		t.Fatalf("RegisterAsset: %v", err)
	}
	if err := s.CapitalizeAsset(ctx, a.AssetID, "approver-1", now); err != nil {
		t.Fatalf("CapitalizeAsset: %v", err)
	}
	if err := s.SuspendAsset(ctx, a.AssetID, "preparer-1", "under repair", now); err != nil {
		t.Fatalf("SuspendAsset: %v", err)
	}

	got, err := s.GetAsset(ctx, a.AssetID)
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	if got.Status != domain.AssetStatusSuspended {
		t.Fatalf("expected SUSPENDED, got %q", got.Status)
	}

	if err := s.ReactivateAsset(ctx, a.AssetID); err != nil {
		t.Fatalf("ReactivateAsset: %v", err)
	}
	got, err = s.GetAsset(ctx, a.AssetID)
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	if got.Status != domain.AssetStatusActive {
		t.Fatalf("expected ACTIVE after reactivation, got %q", got.Status)
	}
}

// TestPgStore_AssignBookProfile_Retroactive_Refused proves the real
// UNIQUE(tenant_id, asset_id, book_id) constraint against the actual
// database — the spec's own negative path, "Retroactive book profile
// change rewrites prior depreciation."
func TestPgStore_AssignBookProfile_Retroactive_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := newTestAsset(tenantID, "le-1")
	if err := s.CreateAsset(ctx, a); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}

	b1 := &domain.AssetBookAssignment{
		AssignmentID: uuid.New().String(), AssetID: a.AssetID, BookID: "book-1",
		Status: domain.BookAssignmentStatusActive, AssignedAt: time.Now().UTC(), AssignedByPrincipalID: "preparer-1",
	}
	if err := s.AssignBookProfile(ctx, b1); err != nil {
		t.Fatalf("first AssignBookProfile: %v", err)
	}

	b2 := &domain.AssetBookAssignment{
		AssignmentID: uuid.New().String(), AssetID: a.AssetID, BookID: "book-1",
		Status: domain.BookAssignmentStatusActive, AssignedAt: time.Now().UTC(), AssignedByPrincipalID: "preparer-1",
	}
	if err := s.AssignBookProfile(ctx, b2); err == nil {
		t.Fatal("expected a second assignment for the same (asset, book) to be refused")
	}
}

// TestPgStore_MergeAssets_AcrossLegalEntities_Refused proves the real
// legal-entity check against actual rows, not just the handler's own
// pre-check.
func TestPgStore_MergeAssets_AcrossLegalEntities_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	source := newTestAsset(tenantID, "le-1")
	target := newTestAsset(tenantID, "le-2")
	if err := s.CreateAsset(ctx, source); err != nil {
		t.Fatalf("CreateAsset (source): %v", err)
	}
	if err := s.CreateAsset(ctx, target); err != nil {
		t.Fatalf("CreateAsset (target): %v", err)
	}
	now := time.Now().UTC()
	for _, id := range []string{source.AssetID, target.AssetID} {
		if err := s.RegisterAsset(ctx, id, "approver-1", now); err != nil {
			t.Fatalf("RegisterAsset: %v", err)
		}
		if err := s.CapitalizeAsset(ctx, id, "approver-1", now); err != nil {
			t.Fatalf("CapitalizeAsset: %v", err)
		}
	}

	if err := s.MergeAssets(ctx, source.AssetID, target.AssetID, "preparer-1", now); err == nil {
		t.Fatal("expected MergeAssets to refuse a cross-legal-entity merge")
	}
}

// TestPgStore_SplitAsset_MovesComponents_RealDB proves the component-move
// transaction against the real database.
func TestPgStore_SplitAsset_MovesComponents_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	source := newTestAsset(tenantID, "le-1")
	if err := s.CreateAsset(ctx, source); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	comp := &domain.AssetComponent{
		ComponentID: uuid.New().String(), AssetID: source.AssetID, Description: "GPU card",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.AddComponent(ctx, comp); err != nil {
		t.Fatalf("AddComponent: %v", err)
	}

	newAsset := &domain.FixedAsset{
		AssetID: uuid.New().String(), LegalEntityID: source.LegalEntityID, AssetCategory: source.AssetCategory,
		Description: "Split-off GPU asset", CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.SplitAsset(ctx, newAsset, source.AssetID, []string{comp.ComponentID}); err != nil {
		t.Fatalf("SplitAsset: %v", err)
	}

	got, err := s.GetAsset(ctx, source.AssetID)
	if err != nil {
		t.Fatalf("GetAsset (source): %v", err)
	}
	if len(got.Components) != 1 || got.Components[0].MovedToAssetID == nil || *got.Components[0].MovedToAssetID != newAsset.AssetID {
		t.Fatalf("expected the component marked moved to the new asset, got %+v", got.Components)
	}

	gotNew, err := s.GetAsset(ctx, newAsset.AssetID)
	if err != nil {
		t.Fatalf("GetAsset (new): %v", err)
	}
	if gotNew.SplitFromAssetID == nil || *gotNew.SplitFromAssetID != source.AssetID {
		t.Fatalf("expected the new asset to record split_from_asset_id, got %+v", gotNew.SplitFromAssetID)
	}
}

// TestPgStore_RLS_TenantIsolation proves a superuser-bypassing store still
// filters explicitly by tenant_id — same posture as every other service.
func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	a := newTestAsset(tenantA, "le-1")
	if err := s.CreateAsset(ctxA, a); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}

	if _, err := s.GetAsset(ctxB, a.AssetID); err == nil {
		t.Fatal("expected tenant B to be unable to read tenant A's asset")
	}
}
