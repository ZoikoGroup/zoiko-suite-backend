package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/compensation-svc/internal/domain"
	"zoiko.io/compensation-svc/internal/middleware"
	"zoiko.io/compensation-svc/internal/store"
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS bonus_grants, wage_revisions, compensation_structures CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_fix_race_and_idempotency.up.sql",
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

// TestPgStore_CreateWageRevision_ConcurrentRevisions_NeverLeavesTwoActive
// proves the unique partial index added in 000002 actually prevents two
// concurrent ReviseWage calls for the same employee from both committing
// an ACTIVE row — a prior version had no such guard, and GetActiveWageRevision
// (LIMIT 1, no ORDER BY) would then return an arbitrary one of the two rows.
func TestPgStore_CreateWageRevision_ConcurrentRevisions_NeverLeavesTwoActive(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	employeeID := uuid.New().String()

	newRev := func(amount float64, correlationID string) *domain.WageRevision {
		return &domain.WageRevision{
			RevisionID:    uuid.New().String(),
			TenantID:      tenantID,
			EmployeeID:    employeeID,
			PayType:       "SALARY",
			Amount:        amount,
			Currency:      "USD",
			EffectiveFrom: "2026-01-01",
			Reason:        "concurrent test",
			RevisedBy:     "hr-admin",
			Status:        "ACTIVE",
			CorrelationID: correlationID,
			CreatedAt:     time.Now().UTC(),
		}
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, results[0] = s.CreateWageRevision(ctx, newRev(90000, "corr-race-1"))
	}()
	go func() {
		defer wg.Done()
		_, results[1] = s.CreateWageRevision(ctx, newRev(95000, "corr-race-2"))
	}()
	wg.Wait()

	succeeded := 0
	concurrentRejections := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if err == domain.ErrConcurrentWageRevision {
			concurrentRejections++
		} else {
			t.Fatalf("unexpected error from concurrent CreateWageRevision: %v", err)
		}
	}
	// Both may succeed sequentially (the second supersedes the first
	// cleanly) or one may lose the race and get ErrConcurrentWageRevision
	// — either outcome is correct. What must NEVER happen is two ACTIVE
	// rows surviving.
	if succeeded == 0 {
		t.Fatal("expected at least one of the two concurrent revisions to succeed")
	}

	var activeCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM wage_revisions WHERE tenant_id = $1 AND employee_id = $2 AND status = 'ACTIVE'`,
		tenantID, employeeID).Scan(&activeCount); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("RACE CONDITION: expected exactly 1 ACTIVE wage revision after concurrent calls, got %d", activeCount)
	}
	_ = concurrentRejections
}

// TestPgStore_CreateWageRevision_RetriedCorrelationID_IsIdempotent proves a
// retried request resolves to the original revision instead of creating a
// duplicate (and instead of erroring on the one-active-per-employee index).
func TestPgStore_CreateWageRevision_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	employeeID := uuid.New().String()

	rev1 := &domain.WageRevision{
		RevisionID: uuid.New().String(), TenantID: tenantID, EmployeeID: employeeID,
		PayType: "SALARY", Amount: 90000, Currency: "USD", EffectiveFrom: "2026-01-01",
		Reason: "initial", RevisedBy: "hr-admin", Status: "ACTIVE",
		CorrelationID: "corr-retry-1", CreatedAt: time.Now().UTC(),
	}
	created1, err := s.CreateWageRevision(ctx, rev1)
	if err != nil {
		t.Fatalf("first CreateWageRevision failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	rev2 := &domain.WageRevision{
		RevisionID: uuid.New().String(), TenantID: tenantID, EmployeeID: employeeID,
		PayType: "SALARY", Amount: 90000, Currency: "USD", EffectiveFrom: "2026-01-01",
		Reason: "initial", RevisedBy: "hr-admin", Status: "ACTIVE",
		CorrelationID: "corr-retry-1", CreatedAt: time.Now().UTC(),
	}
	created2, err := s.CreateWageRevision(ctx, rev2)
	if err != nil {
		t.Fatalf("retried CreateWageRevision failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call")
	}
	if rev2.RevisionID != rev1.RevisionID {
		t.Fatalf("retried call resolved to a different revision_id (%s) than the original (%s)", rev2.RevisionID, rev1.RevisionID)
	}

	var activeCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM wage_revisions WHERE tenant_id = $1 AND employee_id = $2 AND status = 'ACTIVE'`,
		tenantID, employeeID).Scan(&activeCount); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly 1 ACTIVE wage revision, got %d", activeCount)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	strA := &domain.CompensationStructure{
		StructureID: uuid.New().String(), TenantID: tenantA, LegalEntityID: "le-us",
		Name: "Eng Grade 5", PayType: "SALARY", MinAmount: 80000, MaxAmount: 130000,
		Currency: "USD", OvertimeMultiplier: 1.5, CorrelationID: "corr-a", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, err := s.CreateStructure(ctxA, strA); err != nil {
		t.Fatalf("CreateStructure (tenant A) failed: %v", err)
	}

	listB, err := s.ListStructures(ctxB, "")
	if err != nil {
		t.Fatalf("ListStructures under tenant B's context failed: %v", err)
	}
	for _, s := range listB {
		if s.StructureID == strA.StructureID {
			t.Fatal("ISOLATION FAILURE: tenant B was able to see tenant A's compensation structure")
		}
	}
}
