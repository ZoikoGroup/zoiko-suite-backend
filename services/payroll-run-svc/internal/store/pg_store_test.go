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

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
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

	// Reset the whole schema, not a named table list. The list had the same defect
	// the migration list did: it names what existed when it was written, so a
	// migration that adds a table leaves that table behind and the next run fails
	// with "already exists" - or worse, succeeds against stale rows.
	_, _ = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)

	// EVERY *.up.sql, BY GLOB, NOT A HARDCODED LIST. A named list quietly stops
	// applying migrations the moment one is added, so the suite runs green against
	// a schema missing exactly the newest thing anyone wrote. Same failure mode
	// found in leave-absence-svc, notification-svc and evidence-manifest-svc.
	migrations, err := filepath.Glob(filepath.Join(base, "../../deployments/migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("globbing migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no *.up.sql found - these tests would run against an empty schema")
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

func newTestRun(tenantID, correlationID string) *domain.PayrollRun {
	now := time.Now().UTC()
	return &domain.PayrollRun{
		RunID:          uuid.New().String(),
		TenantID:       tenantID,
		LegalEntityID:  "le-us",
		RunNumber:      "PAY-" + uuid.New().String()[:8],
		PayPeriodStart: "2026-01-01",
		PayPeriodEnd:   "2026-01-31",
		PayDate:        "2026-02-05",
		Status:         "INITIATED",
		CorrelationID:  correlationID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// TestPgStore_CreatePayrollRun_RealPostgres proves DATE columns
// (pay_period_start/pay_period_end/pay_date) round-trip correctly — these
// are scanned into Go string fields, which requires an explicit ::text
// cast (pgx cannot decode binary DATE into *string directly).
func TestPgStore_CreatePayrollRun_RealPostgres(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	run := newTestRun(tenantID, "corr-1")
	created, err := s.CreatePayrollRun(ctx, run)
	if err != nil {
		t.Fatalf("CreatePayrollRun failed: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on the first call")
	}

	got, err := s.GetPayrollRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetPayrollRun failed: %v", err)
	}
	if got.PayPeriodStart != "2026-01-01" {
		t.Fatalf("expected pay_period_start 2026-01-01, got %q", got.PayPeriodStart)
	}
	if got.Status != "INITIATED" {
		t.Fatalf("expected status INITIATED, got %s", got.Status)
	}
}

func TestPgStore_CreatePayrollRun_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	run1 := newTestRun(tenantID, "corr-retry-1")
	created1, err := s.CreatePayrollRun(ctx, run1)
	if err != nil {
		t.Fatalf("first CreatePayrollRun failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	run2 := newTestRun(tenantID, "corr-retry-1")
	created2, err := s.CreatePayrollRun(ctx, run2)
	if err != nil {
		t.Fatalf("retried CreatePayrollRun failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a duplicate-payroll-run bug if it's true")
	}
	if run2.RunID != run1.RunID {
		t.Fatalf("retried call resolved to a different run_id (%s) than the original (%s)", run2.RunID, run1.RunID)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	runA := newTestRun(tenantA, "corr-a")
	if _, err := s.CreatePayrollRun(ctxA, runA); err != nil {
		t.Fatalf("CreatePayrollRun (tenant A) failed: %v", err)
	}

	_, err := s.GetPayrollRun(ctxB, runA.RunID)
	if err == nil {
		t.Fatal("ISOLATION FAILURE: tenant B was able to read tenant A's payroll run")
	}
}
