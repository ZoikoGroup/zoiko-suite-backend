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

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/middleware"
	"zoiko.io/org-structure-svc/internal/store"
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS org_assignments, positions, departments CASCADE;`)

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

func newTestDepartment(tenantID, correlationID string) *domain.Department {
	now := time.Now().UTC()
	return &domain.Department{
		DepartmentID:   uuid.New().String(),
		TenantID:       tenantID,
		LegalEntityID:  "le-us",
		Name:           "Engineering",
		Code:           "ENG",
		CostCenterCode: "CC-101",
		Status:         "ACTIVE",
		CorrelationID:  correlationID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func newTestPosition(tenantID, departmentID, correlationID string) *domain.Position {
	now := time.Now().UTC()
	return &domain.Position{
		PositionID:       uuid.New().String(),
		TenantID:         tenantID,
		LegalEntityID:    "le-us",
		DepartmentID:     departmentID,
		Title:            "Senior Backend Engineer",
		Code:             "ENG-SR-BE",
		JobLevel:         "L5",
		MaxHeadcount:     5,
		CurrentHeadcount: 0,
		Status:           "ACTIVE",
		CorrelationID:    correlationID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// TestPgStore_CreateDepartment_RetriedCorrelationID_IsIdempotent proves a
// retried CreateDepartment call resolves to the original department instead
// of creating a duplicate row.
func TestPgStore_CreateDepartment_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	d1 := newTestDepartment(tenantID, "corr-dept-retry")
	created1, err := s.CreateDepartment(ctx, d1)
	if err != nil {
		t.Fatalf("first CreateDepartment failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	d2 := newTestDepartment(tenantID, "corr-dept-retry")
	created2, err := s.CreateDepartment(ctx, d2)
	if err != nil {
		t.Fatalf("retried CreateDepartment failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call")
	}
	if d2.DepartmentID != d1.DepartmentID {
		t.Fatalf("retried call resolved to a different department_id (%s) than the original (%s)", d2.DepartmentID, d1.DepartmentID)
	}
}

// TestPgStore_AssignEmployee_DateColumns_ScanIntoGoString proves the
// effective_from/effective_to DATE columns on org_assignments can be
// scanned into the domain's string fields — this failed before the ::text
// casts were added, with "cannot scan date (OID 1082) in binary format
// into *string". This service had no real-Postgres store test before now.
func TestPgStore_AssignEmployee_DateColumns_ScanIntoGoString(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	dept := newTestDepartment(tenantID, "corr-dept-for-assign")
	if _, err := s.CreateDepartment(ctx, dept); err != nil {
		t.Fatalf("CreateDepartment failed: %v", err)
	}
	pos := newTestPosition(tenantID, dept.DepartmentID, "corr-pos-for-assign")
	if _, err := s.CreatePosition(ctx, pos); err != nil {
		t.Fatalf("CreatePosition failed: %v", err)
	}

	employeeID := uuid.New().String()
	oa, err := s.AssignEmployee(ctx, &domain.AssignEmployeeRequest{
		EmployeeID: employeeID, DepartmentID: dept.DepartmentID, PositionID: pos.PositionID,
		EffectiveFrom: "2026-01-01", CorrelationID: "corr-assign-date",
	})
	if err != nil {
		t.Fatalf("AssignEmployee failed: %v", err)
	}
	if oa.EffectiveFrom != "2026-01-01" {
		t.Errorf("expected effective_from '2026-01-01', got %q", oa.EffectiveFrom)
	}
	if oa.LegalEntityID != "le-us" {
		t.Errorf("expected legal_entity_id 'le-us', got %q", oa.LegalEntityID)
	}

	fetched, err := s.GetEmployeeAssignment(ctx, employeeID)
	if err != nil {
		t.Fatalf("GetEmployeeAssignment failed: %v", err)
	}
	if fetched.EffectiveFrom != "2026-01-01" {
		t.Errorf("expected effective_from '2026-01-01' on GetEmployeeAssignment, got %q", fetched.EffectiveFrom)
	}
}

// TestPgStore_AssignEmployee_RetriedCorrelationID_IsIdempotent proves a
// retried AssignEmployee call resolves to the original assignment instead
// of superseding it again and double-incrementing the position's headcount.
func TestPgStore_AssignEmployee_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	dept := newTestDepartment(tenantID, "corr-dept-for-retry")
	if _, err := s.CreateDepartment(ctx, dept); err != nil {
		t.Fatalf("CreateDepartment failed: %v", err)
	}
	pos := newTestPosition(tenantID, dept.DepartmentID, "corr-pos-for-retry")
	if _, err := s.CreatePosition(ctx, pos); err != nil {
		t.Fatalf("CreatePosition failed: %v", err)
	}

	req := &domain.AssignEmployeeRequest{
		EmployeeID: uuid.New().String(), DepartmentID: dept.DepartmentID, PositionID: pos.PositionID,
		EffectiveFrom: "2026-01-01", CorrelationID: "corr-assign-retry",
	}

	oa1, err := s.AssignEmployee(ctx, req)
	if err != nil {
		t.Fatalf("first AssignEmployee failed: %v", err)
	}
	oa2, err := s.AssignEmployee(ctx, req)
	if err != nil {
		t.Fatalf("retried AssignEmployee failed: %v", err)
	}
	if oa2.AssignmentID != oa1.AssignmentID {
		t.Fatalf("retried call resolved to a different assignment_id (%s) than the original (%s)", oa2.AssignmentID, oa1.AssignmentID)
	}

	fetchedPos, err := s.GetPosition(ctx, pos.PositionID)
	if err != nil {
		t.Fatalf("GetPosition failed: %v", err)
	}
	if fetchedPos.CurrentHeadcount != 1 {
		t.Fatalf("expected current_headcount=1 after replay (no double-increment), got %d", fetchedPos.CurrentHeadcount)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	deptA := newTestDepartment(tenantA, "corr-tenant-a")
	if _, err := s.CreateDepartment(ctxA, deptA); err != nil {
		t.Fatalf("CreateDepartment (tenant A) failed: %v", err)
	}

	listB, err := s.ListDepartments(ctxB, "")
	if err != nil {
		t.Fatalf("ListDepartments under tenant B's context failed: %v", err)
	}
	for _, d := range listB {
		if d.DepartmentID == deptA.DepartmentID {
			t.Fatal("ISOLATION FAILURE: tenant B was able to see tenant A's department")
		}
	}

	if _, err := s.GetDepartment(ctxB, deptA.DepartmentID); err != domain.ErrDepartmentNotFound {
		t.Fatalf("expected ErrDepartmentNotFound when tenant B looks up tenant A's department, got %v", err)
	}
}
