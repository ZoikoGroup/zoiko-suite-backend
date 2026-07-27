package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/offboarding-severance-svc/internal/domain"
	"zoiko.io/offboarding-severance-svc/internal/middleware"
	"zoiko.io/offboarding-severance-svc/internal/store"
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS checklist_items, offboarding_checklists, termination_requests CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_fix_tenant_isolation_and_idempotency.up.sql",
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

func newTestRequest(tenantID, correlationID string) *domain.TerminationRequest {
	return &domain.TerminationRequest{
		TenantID:        tenantID,
		LegalEntityID:   "le-us",
		EmployeeID:      uuid.New().String(),
		TerminationType: domain.TerminationTypeResignation,
		ReasonCode:      "PERSONAL",
		LastWorkingDay:  "2026-12-31",
		EffectiveFrom:   "2026-12-31",
		Status:          domain.TerminationStatusInitiated,
		InitiatedBy:     "hr-admin",
		Currency:        "USD",
		CorrelationID:   correlationID,
	}
}

// TestPgStore_CreateTerminationRequest_RealPostgres proves the RLS tenant
// context mechanism actually works against a real Postgres — a prior
// version used "SET LOCAL app.tenant_id = $1", which is invalid syntax
// (SET does not accept bind parameters) and would error on every call.
func TestPgStore_CreateTerminationRequest_RealPostgres(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	req := newTestRequest(tenantID, "corr-1")
	created, err := s.CreateTerminationRequest(ctx, req)
	if err != nil {
		t.Fatalf("CreateTerminationRequest failed: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on the first call")
	}

	got, err := s.GetTerminationRequest(ctx, req.TerminationID)
	if err != nil {
		t.Fatalf("GetTerminationRequest failed: %v", err)
	}
	if got.Status != domain.TerminationStatusInitiated {
		t.Fatalf("expected status INITIATED, got %s", got.Status)
	}
}

// TestPgStore_CreateTerminationRequest_RetriedCorrelationID_IsIdempotent
// proves a retried request resolves to the original row instead of
// creating a duplicate.
func TestPgStore_CreateTerminationRequest_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	req1 := newTestRequest(tenantID, "corr-retry-1")
	created1, err := s.CreateTerminationRequest(ctx, req1)
	if err != nil {
		t.Fatalf("first CreateTerminationRequest failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	req2 := newTestRequest(tenantID, "corr-retry-1")
	created2, err := s.CreateTerminationRequest(ctx, req2)
	if err != nil {
		t.Fatalf("retried CreateTerminationRequest failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a duplicate-termination bug if it's true")
	}
	if req2.TerminationID != req1.TerminationID {
		t.Fatalf("retried call resolved to a different termination_id (%s) than the original (%s)", req2.TerminationID, req1.TerminationID)
	}
}

// TestPgStore_RLS_TenantIsolation proves tenant A's data is invisible
// under tenant B's context — this is the exact property "Tenant Isolation
// Is Sacred" (05-security.md §3.5) requires, and the property that was
// silently broken by the invalid SET LOCAL syntax.
func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	reqA := newTestRequest(tenantA, "corr-a")
	if _, err := s.CreateTerminationRequest(ctxA, reqA); err != nil {
		t.Fatalf("CreateTerminationRequest (tenant A) failed: %v", err)
	}

	_, err := s.GetTerminationRequest(ctxB, reqA.TerminationID)
	if err == nil {
		t.Fatal("ISOLATION FAILURE: tenant B was able to read tenant A's termination request")
	}

	// Approve/Finalize under tenant B's context must not affect tenant A's row.
	if _, err := s.ApproveTerminationRequest(ctxB, reqA.TerminationID, "attacker"); err == nil {
		t.Fatal("ISOLATION FAILURE: tenant B was able to approve tenant A's termination request")
	}
	gotA, err := s.GetTerminationRequest(ctxA, reqA.TerminationID)
	if err != nil {
		t.Fatalf("GetTerminationRequest under tenant A's own context failed: %v", err)
	}
	if gotA.Status != domain.TerminationStatusInitiated {
		t.Fatalf("ISOLATION FAILURE: tenant A's request status was mutated by tenant B, got %s", gotA.Status)
	}
}

// TestPgStore_ApproveTerminationRequest_AtomicTransition proves the
// approve step is a single conditional UPDATE, not a read-then-write with
// a race window: approving twice must succeed once and fail the second
// time with ErrAlreadyApproved, never silently re-approve.
func TestPgStore_ApproveTerminationRequest_AtomicTransition(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	req := newTestRequest(tenantID, "corr-approve-1")
	if _, err := s.CreateTerminationRequest(ctx, req); err != nil {
		t.Fatalf("CreateTerminationRequest failed: %v", err)
	}

	if _, err := s.ApproveTerminationRequest(ctx, req.TerminationID, "hr-admin"); err != nil {
		t.Fatalf("first ApproveTerminationRequest failed: %v", err)
	}

	if _, err := s.ApproveTerminationRequest(ctx, req.TerminationID, "hr-admin"); err != domain.ErrAlreadyApproved {
		t.Fatalf("expected ErrAlreadyApproved on second approve, got %v", err)
	}
}

// TestPgStore_FinalizeEmployeeTermination_RequiresApproval proves a
// termination cannot be finalized before it's been approved — a prior
// version had no status guard at all.
func TestPgStore_FinalizeEmployeeTermination_RequiresApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	req := newTestRequest(tenantID, "corr-finalize-1")
	if _, err := s.CreateTerminationRequest(ctx, req); err != nil {
		t.Fatalf("CreateTerminationRequest failed: %v", err)
	}

	if _, err := s.FinalizeEmployeeTermination(ctx, req.TerminationID); err != domain.ErrNotApproved {
		t.Fatalf("expected ErrNotApproved when finalizing an unapproved request, got %v", err)
	}

	if _, err := s.ApproveTerminationRequest(ctx, req.TerminationID, "hr-admin"); err != nil {
		t.Fatalf("ApproveTerminationRequest failed: %v", err)
	}
	if _, err := s.FinalizeEmployeeTermination(ctx, req.TerminationID); err != nil {
		t.Fatalf("FinalizeEmployeeTermination after approval failed: %v", err)
	}
	if _, err := s.FinalizeEmployeeTermination(ctx, req.TerminationID); err != domain.ErrAlreadyTerminated {
		t.Fatalf("expected ErrAlreadyTerminated on second finalize, got %v", err)
	}
}
