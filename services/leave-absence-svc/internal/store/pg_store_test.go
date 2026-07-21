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

	"zoiko.io/leave-absence-svc/internal/domain"
	"zoiko.io/leave-absence-svc/internal/middleware"
	"zoiko.io/leave-absence-svc/internal/store"
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS leave_requests, leave_balances, leave_types CASCADE;`)

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

func setupLeaveTypeAndBalance(t *testing.T, s *store.PgStore, ctx context.Context, tenantID, employeeID string, allocatedHours float64) string {
	t.Helper()
	lt := &domain.LeaveType{
		LeaveTypeID: uuid.New().String(), TenantID: tenantID, LegalEntityID: "le-us",
		Name: "Vacation", Code: "VACATION", IsPaid: true, AccrualRatePerYear: 120, MaxBalance: 200,
		Status: "ACTIVE", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := s.CreateLeaveType(ctx, lt); err != nil {
		t.Fatalf("CreateLeaveType failed: %v", err)
	}
	if _, err := s.AccrueLeaveBalance(ctx, employeeID, lt.LeaveTypeID, allocatedHours); err != nil {
		t.Fatalf("AccrueLeaveBalance failed: %v", err)
	}
	return lt.LeaveTypeID
}

// TestPgStore_SubmitLeaveRequest_RetriedCorrelationID_IsIdempotent proves a
// retried SubmitLeaveRequest call resolves to the original request instead
// of creating a duplicate request and double-locking pending hours.
func TestPgStore_SubmitLeaveRequest_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	employeeID := uuid.New().String()
	leaveTypeID := setupLeaveTypeAndBalance(t, s, ctx, tenantID, employeeID, 80)

	submit := func() *domain.LeaveRequest {
		lr, err := s.SubmitLeaveRequest(ctx, &domain.SubmitLeaveRequest{
			EmployeeID: employeeID, LeaveTypeID: leaveTypeID, StartDate: "2026-01-01", EndDate: "2026-01-05",
			TotalHours: 40, CorrelationID: "corr-submit-retry",
		})
		if err != nil {
			t.Fatalf("SubmitLeaveRequest failed: %v", err)
		}
		return lr
	}

	lr1 := submit()
	lr2 := submit()
	if lr2.RequestID != lr1.RequestID {
		t.Fatalf("retried call resolved to a different request_id (%s) than the original (%s)", lr2.RequestID, lr1.RequestID)
	}

	balances, err := s.GetLeaveBalances(ctx, employeeID)
	if err != nil {
		t.Fatalf("GetLeaveBalances failed: %v", err)
	}
	if len(balances) != 1 || balances[0].PendingHours != 40 {
		t.Fatalf("expected pending_hours locked exactly once (40), got %+v — replay must not double-lock", balances)
	}
}

// TestPgStore_ApproveLeaveRequest_ConcurrentApproveReject_NeverDoubleMutates
// proves the atomic conditional UPDATE prevents a concurrent approve+reject
// race (or double-click) from both mutating the balance ledger — only one
// of the two calls may succeed, and the balance must reflect exactly one
// transition.
func TestPgStore_ApproveLeaveRequest_ConcurrentApproveReject_NeverDoubleMutates(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	employeeID := uuid.New().String()
	leaveTypeID := setupLeaveTypeAndBalance(t, s, ctx, tenantID, employeeID, 80)

	lr, err := s.SubmitLeaveRequest(ctx, &domain.SubmitLeaveRequest{
		EmployeeID: employeeID, LeaveTypeID: leaveTypeID, StartDate: "2026-01-01", EndDate: "2026-01-05",
		TotalHours: 40, CorrelationID: "corr-race",
	})
	if err != nil {
		t.Fatalf("SubmitLeaveRequest failed: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = s.ApproveLeaveRequest(ctx, lr.RequestID, "manager-1", "approved")
	}()
	go func() {
		defer wg.Done()
		results[1] = s.RejectLeaveRequest(ctx, lr.RequestID, "manager-2", "rejected")
	}()
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if err != domain.ErrInvalidStatusTransition {
			t.Fatalf("unexpected error from concurrent approve/reject: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly one of the two concurrent calls to succeed, got %d", succeeded)
	}

	balances, err := s.GetLeaveBalances(ctx, employeeID)
	if err != nil {
		t.Fatalf("GetLeaveBalances failed: %v", err)
	}
	if len(balances) != 1 {
		t.Fatalf("expected exactly 1 balance row, got %d", len(balances))
	}
	b := balances[0]
	if b.PendingHours != 0 {
		t.Fatalf("RACE CONDITION: expected pending_hours to be fully resolved (0), got %f", b.PendingHours)
	}
	// Either APPROVED (used_hours=40) or REJECTED (used_hours=0) is correct,
	// but never both — used_hours must never exceed the original 40.
	if b.UsedHours != 0 && b.UsedHours != 40 {
		t.Fatalf("RACE CONDITION: unexpected used_hours %f after concurrent approve/reject", b.UsedHours)
	}

	final, err := s.GetLeaveRequest(ctx, lr.RequestID)
	if err != nil {
		t.Fatalf("GetLeaveRequest failed: %v", err)
	}
	if final.Status != "APPROVED" && final.Status != "REJECTED" {
		t.Fatalf("expected final status APPROVED or REJECTED, got %q", final.Status)
	}
}

func TestPgStore_ApproveLeaveRequest_DoubleApprove_SecondFails(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	employeeID := uuid.New().String()
	leaveTypeID := setupLeaveTypeAndBalance(t, s, ctx, tenantID, employeeID, 80)

	lr, err := s.SubmitLeaveRequest(ctx, &domain.SubmitLeaveRequest{
		EmployeeID: employeeID, LeaveTypeID: leaveTypeID, StartDate: "2026-01-01", EndDate: "2026-01-05",
		TotalHours: 40, CorrelationID: "corr-double-approve",
	})
	if err != nil {
		t.Fatalf("SubmitLeaveRequest failed: %v", err)
	}

	if err := s.ApproveLeaveRequest(ctx, lr.RequestID, "manager-1", "approved"); err != nil {
		t.Fatalf("first ApproveLeaveRequest failed: %v", err)
	}
	if err := s.ApproveLeaveRequest(ctx, lr.RequestID, "manager-1", "approved again"); err != domain.ErrInvalidStatusTransition {
		t.Fatalf("expected ErrInvalidStatusTransition on double-approve, got %v", err)
	}

	balances, err := s.GetLeaveBalances(ctx, employeeID)
	if err != nil {
		t.Fatalf("GetLeaveBalances failed: %v", err)
	}
	if balances[0].UsedHours != 40 {
		t.Fatalf("expected used_hours=40 after exactly one approve, got %f — double-approve must not double-mutate", balances[0].UsedHours)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	ltA := &domain.LeaveType{
		LeaveTypeID: uuid.New().String(), TenantID: tenantA, LegalEntityID: "le-us",
		Name: "Vacation", Code: "VACATION", IsPaid: true, Status: "ACTIVE",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := s.CreateLeaveType(ctxA, ltA); err != nil {
		t.Fatalf("CreateLeaveType (tenant A) failed: %v", err)
	}

	listB, err := s.ListLeaveTypes(ctxB, "")
	if err != nil {
		t.Fatalf("ListLeaveTypes under tenant B's context failed: %v", err)
	}
	for _, lt := range listB {
		if lt.LeaveTypeID == ltA.LeaveTypeID {
			t.Fatal("ISOLATION FAILURE: tenant B was able to see tenant A's leave type")
		}
	}
}
