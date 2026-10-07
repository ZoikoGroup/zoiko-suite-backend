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

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
	"zoiko.io/project-accounting-svc/internal/store"
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
		project_profitability_snapshots, project_profitability_projections,
		project_recognition_runs, project_recognition_estimates,
		project_cost_certifications, project_cost_entries,
		project_financial_profiles, project_work_packages, projects
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

func newTestProject(tenantID, legalEntityID, code string) *domain.Project {
	return &domain.Project{
		ProjectID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		ProjectCode: code, Name: "Test Project",
		Status: domain.ProjectStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "creator-1",
	}
}

func TestPgStore_CreateProject_And_GetProject(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	p := newTestProject(tenantID, legalEntityID, "PRJ-1")
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}
	got, err := s.GetProject(ctx, p.ProjectID)
	if err != nil {
		t.Fatalf("GetProject failed: %v", err)
	}
	if got.ProjectCode != "PRJ-1" || got.Status != domain.ProjectStatusDraft {
		t.Fatalf("unexpected project: %+v", got)
	}
}

func TestPgStore_CreateProject_DuplicateCode_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first := newTestProject(tenantID, legalEntityID, "PRJ-DUP")
	if err := s.CreateProject(ctx, first); err != nil {
		t.Fatalf("first CreateProject failed: %v", err)
	}
	second := newTestProject(tenantID, legalEntityID, "PRJ-DUP")
	if err := s.CreateProject(ctx, second); err != domain.ErrDuplicateProjectCode {
		t.Fatalf("expected ErrDuplicateProjectCode, got %v", err)
	}
}

func TestPgStore_ReopenProjectControlled_SelfReopenRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	p := newTestProject(tenantID, legalEntityID, "PRJ-2")
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ApproveProject(ctx, p.ProjectID, "approver-1", now); err != nil {
		t.Fatalf("ApproveProject failed: %v", err)
	}
	if err := s.ActivateProject(ctx, p.ProjectID, now); err != nil {
		t.Fatalf("ActivateProject failed: %v", err)
	}
	if err := s.CloseProject(ctx, p.ProjectID, "closer-1", "done", now); err != nil {
		t.Fatalf("CloseProject failed: %v", err)
	}
	if err := s.ReopenProjectControlled(ctx, p.ProjectID, "closer-1", "need more work", now); err != domain.ErrSelfReopenNotPermitted {
		t.Fatalf("expected ErrSelfReopenNotPermitted, got %v", err)
	}
	if err := s.ReopenProjectControlled(ctx, p.ProjectID, "reviewer-2", "need more work", now); err != nil {
		t.Fatalf("expected reopen by a different principal to succeed, got %v", err)
	}
	got, err := s.GetProject(ctx, p.ProjectID)
	if err != nil {
		t.Fatalf("GetProject failed: %v", err)
	}
	if got.Status != domain.ProjectStatusActive {
		t.Fatalf("expected ACTIVE after reopen, got %q", got.Status)
	}
}

// TestPgStore_AddWorkPackage_Immutable is the real proof of negative path
// #4, "WBS deletion orphans historical cost."
func TestPgStore_AddWorkPackage_Immutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	p := newTestProject(tenantID, legalEntityID, "PRJ-3")
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}
	now := time.Now().UTC()
	wbs := &domain.WorkPackage{
		WBSID: uuid.New().String(), ProjectID: p.ProjectID, WBSCode: "WBS-1",
		EffectiveFrom: now, CreatedAt: now, CreatedByPrincipalID: "manager-1",
	}
	if err := s.AddWorkPackage(ctx, wbs); err != nil {
		t.Fatalf("AddWorkPackage failed: %v", err)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE project_work_packages SET description = 'tampered' WHERE wbs_id = $1`, wbs.WBSID); err == nil {
		t.Fatalf("expected the reject-mutation trigger to refuse an UPDATE, got no error")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM project_work_packages WHERE wbs_id = $1`, wbs.WBSID); err == nil {
		t.Fatalf("expected the reject-mutation trigger to refuse a DELETE, got no error")
	}
}

// TestPgStore_AmendFinancialProfile_Versions proves financial-profile
// changes are versioned, never mutated in place.
func TestPgStore_AmendFinancialProfile_Versions(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	p := newTestProject(tenantID, legalEntityID, "PRJ-4")
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}

	now := time.Now().UTC()
	v1 := &domain.FinancialProfile{
		ProfileVersionID: uuid.New().String(), ProjectID: p.ProjectID, RecognitionMethod: domain.RecognitionMethodTimeAndMaterials,
		BillingType: domain.BillingTypeTimeAndMaterials, Currency: "USD", EffectiveFrom: now, CreatedAt: now, CreatedByPrincipalID: "manager-1",
	}
	if err := s.AmendFinancialProfile(ctx, p.ProjectID, v1); err != nil {
		t.Fatalf("AmendFinancialProfile v1 failed: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("expected version 1, got %d", v1.Version)
	}

	later := now.Add(time.Hour)
	v2 := &domain.FinancialProfile{
		ProfileVersionID: uuid.New().String(), ProjectID: p.ProjectID, RecognitionMethod: domain.RecognitionMethodPercentageOfCompletion,
		BillingType: domain.BillingTypeFixedPrice, Currency: "USD", EffectiveFrom: later, CreatedAt: now, CreatedByPrincipalID: "manager-1",
	}
	if err := s.AmendFinancialProfile(ctx, p.ProjectID, v2); err != nil {
		t.Fatalf("AmendFinancialProfile v2 failed: %v", err)
	}
	if v2.Version != 2 || v2.ProfileID != v1.ProfileID {
		t.Fatalf("expected v2 to be version 2 of the same logical profile, got version=%d profile_id=%s (v1=%s)", v2.Version, v2.ProfileID, v1.ProfileID)
	}

	current, err := s.GetCurrentFinancialProfile(ctx, p.ProjectID)
	if err != nil {
		t.Fatalf("GetCurrentFinancialProfile failed: %v", err)
	}
	if current.RecognitionMethod != domain.RecognitionMethodPercentageOfCompletion || current.Version != 2 {
		t.Fatalf("expected current profile to be v2, got %+v", current)
	}

	// As-of a moment BEFORE v2 takes effect must return v1.
	asOf, err := s.GetFinancialProfileAsOf(ctx, p.ProjectID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("GetFinancialProfileAsOf failed: %v", err)
	}
	if asOf == nil || asOf.RecognitionMethod != domain.RecognitionMethodTimeAndMaterials {
		t.Fatalf("expected the historic TIME_AND_MATERIALS version to still be returned as-of before v2, got %+v", asOf)
	}
}
