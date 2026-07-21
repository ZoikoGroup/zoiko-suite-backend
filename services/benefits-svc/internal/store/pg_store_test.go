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

	"zoiko.io/benefits-svc/internal/domain"
	"zoiko.io/benefits-svc/internal/middleware"
	"zoiko.io/benefits-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migrations
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS benefit_elections, benefit_plans CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_fix_lookup_and_idempotency.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", migration))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", migration, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", migration, err)
		}
	}

	return pool
}

func newTestPlan(tenantID, correlationID string) *domain.BenefitPlan {
	now := time.Now().UTC()
	return &domain.BenefitPlan{
		PlanID:                     uuid.New().String(),
		TenantID:                   tenantID,
		LegalEntityID:              "le-us",
		Name:                       "Gold Health Plan",
		PlanType:                   "HEALTH_INSURANCE",
		ProviderName:               "Aetna",
		DeductionTaxTreatment:      "PRE_TAX",
		EmployerContributionPct:    50.0,
		EmployeeContributionAmount: 200.0,
		Currency:                   "USD",
		Status:                     "ACTIVE",
		CorrelationID:              correlationID,
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
}

// TestPgStore_CreatePlan_RetriedCorrelationID_IsIdempotent proves a retried
// CreatePlan call resolves to the original plan instead of creating a
// duplicate row.
func TestPgStore_CreatePlan_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p1 := newTestPlan(tenantID, "corr-plan-retry")
	created1, err := s.CreatePlan(ctx, p1)
	if err != nil {
		t.Fatalf("first CreatePlan failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	p2 := newTestPlan(tenantID, "corr-plan-retry")
	created2, err := s.CreatePlan(ctx, p2)
	if err != nil {
		t.Fatalf("retried CreatePlan failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call")
	}
	if p2.PlanID != p1.PlanID {
		t.Fatalf("retried call resolved to a different plan_id (%s) than the original (%s)", p2.PlanID, p1.PlanID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM benefit_plans WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 benefit_plan row, got %d", count)
	}
}

// TestPgStore_CreateElection_DateColumns_ScanIntoGoString proves the
// effective_from/effective_to DATE columns can be scanned into the domain's
// string fields — this failed before the ::text casts were added, with
// "cannot scan date (OID 1082) in binary format into *string".
func TestPgStore_CreateElection_DateColumns_ScanIntoGoString(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	plan := newTestPlan(tenantID, "corr-plan-for-election")
	if _, err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("CreatePlan failed: %v", err)
	}

	now := time.Now().UTC()
	election := &domain.BenefitElection{
		ElectionID:                 uuid.New().String(),
		TenantID:                   tenantID,
		EmployeeID:                 uuid.New().String(),
		PlanID:                     plan.PlanID,
		CoverageLevel:              "EMPLOYEE_ONLY",
		EmployeeContributionAmount: 200.0,
		EmployerContributionAmount: 100.0,
		EffectiveFrom:              "2026-01-01",
		Status:                     "ACTIVE",
		CorrelationID:              "corr-election-date",
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
	if _, err := s.CreateElection(ctx, election); err != nil {
		t.Fatalf("CreateElection failed: %v", err)
	}

	fetched, err := s.GetElection(ctx, election.ElectionID)
	if err != nil {
		t.Fatalf("GetElection failed: %v", err)
	}
	if fetched.EffectiveFrom != "2026-01-01" {
		t.Errorf("expected effective_from '2026-01-01', got %q", fetched.EffectiveFrom)
	}
}

// TestPgStore_GetElection_UsedByUpdateAndCancel proves GetElection resolves
// an election by its primary key with a bound tenant_id — the query path
// that replaced the old ListElectionsByEmployee(ctx, "", status) call, which
// bound an empty string to employee_id's UUID NOT NULL column and errored on
// every UpdateElection/CancelElection request in production.
func TestPgStore_GetElection_UsedByUpdateAndCancel(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	plan := newTestPlan(tenantID, "corr-plan-get-election")
	if _, err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("CreatePlan failed: %v", err)
	}

	now := time.Now().UTC()
	election := &domain.BenefitElection{
		ElectionID:                 uuid.New().String(),
		TenantID:                   tenantID,
		EmployeeID:                 uuid.New().String(),
		PlanID:                     plan.PlanID,
		CoverageLevel:              "EMPLOYEE_ONLY",
		EmployeeContributionAmount: 200.0,
		EmployerContributionAmount: 100.0,
		EffectiveFrom:              "2026-01-01",
		Status:                     "ACTIVE",
		CorrelationID:              "corr-election-get",
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
	if _, err := s.CreateElection(ctx, election); err != nil {
		t.Fatalf("CreateElection failed: %v", err)
	}

	fetched, err := s.GetElection(ctx, election.ElectionID)
	if err != nil {
		t.Fatalf("GetElection failed: %v", err)
	}
	if fetched.EmployeeID != election.EmployeeID {
		t.Errorf("expected employee_id %q, got %q", election.EmployeeID, fetched.EmployeeID)
	}

	if _, err := s.GetElection(ctx, uuid.New().String()); err != domain.ErrElectionNotFound {
		t.Errorf("expected ErrElectionNotFound for unknown election_id, got %v", err)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	planA := newTestPlan(tenantA, "corr-plan-tenant-a")
	if _, err := s.CreatePlan(ctxA, planA); err != nil {
		t.Fatalf("CreatePlan (tenant A) failed: %v", err)
	}

	listB, err := s.ListPlans(ctxB, "", "")
	if err != nil {
		t.Fatalf("ListPlans under tenant B's context failed: %v", err)
	}
	for _, p := range listB {
		if p.PlanID == planA.PlanID {
			t.Fatal("ISOLATION FAILURE: tenant B was able to see tenant A's benefit plan")
		}
	}

	if _, err := s.GetPlan(ctxB, planA.PlanID); err != domain.ErrPlanNotFound {
		t.Fatalf("expected ErrPlanNotFound when tenant B looks up tenant A's plan, got %v", err)
	}
}

func TestPgStore_CancelElection_DoubleCancel_ReturnsAlreadyCancelled(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	plan := newTestPlan(tenantID, "corr-plan-double-cancel")
	if _, err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("CreatePlan failed: %v", err)
	}

	now := time.Now().UTC()
	election := &domain.BenefitElection{
		ElectionID:                 uuid.New().String(),
		TenantID:                   tenantID,
		EmployeeID:                 uuid.New().String(),
		PlanID:                     plan.PlanID,
		CoverageLevel:              "EMPLOYEE_ONLY",
		EmployeeContributionAmount: 200.0,
		EmployerContributionAmount: 100.0,
		EffectiveFrom:              "2026-01-01",
		Status:                     "ACTIVE",
		CorrelationID:              "corr-election-double-cancel",
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
	if _, err := s.CreateElection(ctx, election); err != nil {
		t.Fatalf("CreateElection failed: %v", err)
	}

	if err := s.CancelElection(ctx, election.ElectionID, "2026-02-01"); err != nil {
		t.Fatalf("first CancelElection failed: %v", err)
	}
	if err := s.CancelElection(ctx, election.ElectionID, "2026-02-01"); err != domain.ErrElectionAlreadyCancelled {
		t.Fatalf("expected ErrElectionAlreadyCancelled on second cancel, got %v", err)
	}
}
