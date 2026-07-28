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

	"zoiko.io/employment-contracts-svc/internal/domain"
	"zoiko.io/employment-contracts-svc/internal/middleware"
	"zoiko.io/employment-contracts-svc/internal/store"
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS contract_amendments, employment_contracts CASCADE;`)

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

func newTestContract(tenantID, correlationID string) *domain.EmploymentContract {
	now := time.Now().UTC()
	return &domain.EmploymentContract{
		ContractID:       uuid.New().String(),
		TenantID:         tenantID,
		LegalEntityID:    "le-us",
		EmployeeID:       uuid.New().String(),
		ContractNumber:   "CTR-" + uuid.New().String()[:8],
		Version:          1,
		ContractType:     "FULL_TIME",
		Status:           "ACTIVE",
		Title:            "Software Engineer",
		BaseSalaryAmount: 100000,
		Currency:         "USD",
		PayFrequency:     "MONTHLY",
		EffectiveFrom:    "2026-01-01",
		CorrelationID:    correlationID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// TestPgStore_IssueContract_DateColumns_ScanIntoGoString proves the
// effective_from/effective_to DATE columns can be scanned into the domain's
// string fields. This failed on every query in this service before the
// ::text casts were added: "cannot scan date (OID 1082) in binary format
// into *string" — invisible under stub-store-only tests, since this
// service previously had no real-Postgres store test at all.
func TestPgStore_IssueContract_DateColumns_ScanIntoGoString(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	c := newTestContract(tenantID, "corr-date-check")
	if _, err := s.IssueContract(ctx, c); err != nil {
		t.Fatalf("IssueContract failed: %v", err)
	}

	fetched, err := s.GetContract(ctx, c.ContractID)
	if err != nil {
		t.Fatalf("GetContract failed: %v", err)
	}
	if fetched.EffectiveFrom != "2026-01-01" {
		t.Errorf("expected effective_from '2026-01-01', got %q", fetched.EffectiveFrom)
	}

	if _, err := s.ListContracts(ctx, "le-us", "", ""); err != nil {
		t.Fatalf("ListContracts failed: %v", err)
	}
	if _, err := s.GetActiveContractByEmployee(ctx, c.EmployeeID); err != nil {
		t.Fatalf("GetActiveContractByEmployee failed: %v", err)
	}
	if _, err := s.GetContractVersionHistory(ctx, c.ContractNumber); err != nil {
		t.Fatalf("GetContractVersionHistory failed: %v", err)
	}
}

// TestPgStore_IssueContract_RetriedCorrelationID_IsIdempotent proves a
// retried IssueContract call resolves to the original contract instead of
// creating a duplicate — contract_number alone was never unique.
func TestPgStore_IssueContract_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	c1 := newTestContract(tenantID, "corr-contract-retry")
	created1, err := s.IssueContract(ctx, c1)
	if err != nil {
		t.Fatalf("first IssueContract failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	c2 := newTestContract(tenantID, "corr-contract-retry")
	created2, err := s.IssueContract(ctx, c2)
	if err != nil {
		t.Fatalf("retried IssueContract failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call")
	}
	if c2.ContractID != c1.ContractID {
		t.Fatalf("retried call resolved to a different contract_id (%s) than the original (%s)", c2.ContractID, c1.ContractID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM employment_contracts WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 employment_contracts row, got %d", count)
	}
}

func TestPgStore_AmendContract_AtomicSupersede(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	c := newTestContract(tenantID, "corr-amend-base")
	if _, err := s.IssueContract(ctx, c); err != nil {
		t.Fatalf("IssueContract failed: %v", err)
	}

	now := time.Now().UTC()
	newContract := &domain.EmploymentContract{
		ContractID: uuid.New().String(), TenantID: tenantID, LegalEntityID: c.LegalEntityID,
		EmployeeID: c.EmployeeID, ContractNumber: c.ContractNumber, Version: 2,
		ContractType: c.ContractType, Status: "ACTIVE", Title: "Senior Software Engineer",
		BaseSalaryAmount: 130000, Currency: c.Currency, PayFrequency: c.PayFrequency,
		EffectiveFrom: "2026-06-01", CreatedAt: now, UpdatedAt: now,
	}
	amendment := &domain.ContractAmendment{
		AmendmentID: uuid.New().String(), TenantID: tenantID, ContractID: newContract.ContractID,
		FromVersion: 1, ToVersion: 2, AmendmentReason: "promotion", AmendedBy: "hr-admin",
		EffectiveFrom: "2026-06-01", CreatedAt: now,
	}
	if err := s.AmendContract(ctx, c.ContractID, newContract, amendment); err != nil {
		t.Fatalf("AmendContract failed: %v", err)
	}

	old, err := s.GetContract(ctx, c.ContractID)
	if err != nil {
		t.Fatalf("GetContract (old) failed: %v", err)
	}
	if old.Status != "SUPERSEDED" {
		t.Fatalf("expected old contract status SUPERSEDED, got %q", old.Status)
	}

	// A second amend attempt against the now-superseded contract must fail.
	if err := s.AmendContract(ctx, c.ContractID, newContract, amendment); err != domain.ErrContractAlreadyTerminated {
		t.Fatalf("expected ErrContractAlreadyTerminated on re-amend of superseded contract, got %v", err)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	cA := newTestContract(tenantA, "corr-tenant-a")
	if _, err := s.IssueContract(ctxA, cA); err != nil {
		t.Fatalf("IssueContract (tenant A) failed: %v", err)
	}

	listB, err := s.ListContracts(ctxB, "", "", "")
	if err != nil {
		t.Fatalf("ListContracts under tenant B's context failed: %v", err)
	}
	for _, c := range listB {
		if c.ContractID == cA.ContractID {
			t.Fatal("ISOLATION FAILURE: tenant B was able to see tenant A's employment contract")
		}
	}

	if _, err := s.GetContract(ctxB, cA.ContractID); err != domain.ErrContractNotFound {
		t.Fatalf("expected ErrContractNotFound when tenant B looks up tenant A's contract, got %v", err)
	}
}
