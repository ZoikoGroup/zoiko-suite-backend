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

	"zoiko.io/payroll-exceptions-svc/internal/domain"
	"zoiko.io/payroll-exceptions-svc/internal/middleware"
	"zoiko.io/payroll-exceptions-svc/internal/store"
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS payroll_exceptions CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_idempotency.up.sql",
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

func newTestException(tenantID, correlationID string) *domain.PayrollException {
	return &domain.PayrollException{
		ExceptionID:   uuid.New().String(),
		TenantID:      tenantID,
		PayrollRunID:  uuid.New().String(),
		ExceptionCode: "NEGATIVE_NET_PAY",
		Severity:      "BLOCKER",
		Description:   "Net pay calculated as negative",
		DetailsJSON:   "{}",
		Status:        "OPEN",
		CorrelationID: correlationID,
		CreatedAt:     time.Now().UTC(),
	}
}

// TestPgStore_CreateException_RetriedCorrelationID_IsIdempotent proves a
// retried RaiseException call resolves to the original exception instead
// of creating a duplicate exception and re-flagging an already-open blocker.
func TestPgStore_CreateException_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	exc1 := newTestException(tenantID, "corr-exc-retry")
	created1, err := s.CreateException(ctx, exc1)
	if err != nil {
		t.Fatalf("first CreateException failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	exc2 := newTestException(tenantID, "corr-exc-retry")
	created2, err := s.CreateException(ctx, exc2)
	if err != nil {
		t.Fatalf("retried CreateException failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call")
	}
	if exc2.ExceptionID != exc1.ExceptionID {
		t.Fatalf("retried call resolved to a different exception_id (%s) than the original (%s)", exc2.ExceptionID, exc1.ExceptionID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM payroll_exceptions WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 payroll_exceptions row, got %d", count)
	}
}

// TestPgStore_ResolveException_AtomicTransition proves ResolveException's
// conditional UPDATE (WHERE status IN ('OPEN','IN_REVIEW')) rejects a
// resolve/waive call on an already-resolved exception rather than silently
// overwriting the resolution.
func TestPgStore_ResolveException_AtomicTransition(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	exc := newTestException(tenantID, "corr-exc-resolve")
	if _, err := s.CreateException(ctx, exc); err != nil {
		t.Fatalf("CreateException failed: %v", err)
	}

	if err := s.ResolveException(ctx, exc.ExceptionID, "fixed", "payroll-admin", "RESOLVED"); err != nil {
		t.Fatalf("first ResolveException failed: %v", err)
	}
	if err := s.ResolveException(ctx, exc.ExceptionID, "fixed again", "payroll-admin", "WAIVED"); err != domain.ErrAlreadyResolved {
		t.Fatalf("expected ErrAlreadyResolved on double-resolve, got %v", err)
	}

	fetched, err := s.GetException(ctx, exc.ExceptionID)
	if err != nil {
		t.Fatalf("GetException failed: %v", err)
	}
	if fetched.Status != "RESOLVED" {
		t.Fatalf("expected status to remain RESOLVED after rejected double-resolve, got %q", fetched.Status)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	excA := newTestException(tenantA, "corr-tenant-a")
	if _, err := s.CreateException(ctxA, excA); err != nil {
		t.Fatalf("CreateException (tenant A) failed: %v", err)
	}

	listB, err := s.ListExceptions(ctxB, "", "", "", "")
	if err != nil {
		t.Fatalf("ListExceptions under tenant B's context failed: %v", err)
	}
	for _, e := range listB {
		if e.ExceptionID == excA.ExceptionID {
			t.Fatal("ISOLATION FAILURE: tenant B was able to see tenant A's payroll exception")
		}
	}

	if _, err := s.GetException(ctxB, excA.ExceptionID); err != domain.ErrExceptionNotFound {
		t.Fatalf("expected ErrExceptionNotFound when tenant B looks up tenant A's exception, got %v", err)
	}
}
