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

	"zoiko.io/payroll-tax-svc/internal/domain"
	"zoiko.io/payroll-tax-svc/internal/middleware"
	"zoiko.io/payroll-tax-svc/internal/store"
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS tax_basis_audits, tax_calculation_records, tax_jurisdiction_profiles CASCADE;`)

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

func newTestCalc(tenantID, correlationID string) (*domain.TaxCalculationRecord, *domain.TaxBasisAudit) {
	calcID := uuid.New().String()
	now := time.Now().UTC()
	calc := &domain.TaxCalculationRecord{
		CalculationID:         calcID,
		TenantID:              tenantID,
		PayrollRunID:          uuid.New().String(),
		EmployeeID:            uuid.New().String(),
		JurisdictionCode:      "US-NY",
		GrossTaxableAmount:    10000,
		PreTaxDeductionAmount: 1000,
		TaxableBasis:          9000,
		TotalTaxAmount:        1980,
		TaxBreakdown: []domain.TaxComponent{
			{TaxName: "Income Tax", TaxType: "STATE_FEDERAL", RatePct: 15, TaxAmount: 1350},
		},
		EngineType:      "STANDARD_ENGINE",
		RuleVersionUsed: "2026.1",
		Status:          "CALCULATED",
		CorrelationID:   correlationID,
		CreatedAt:       now,
	}
	audit := &domain.TaxBasisAudit{
		AuditID:              uuid.New().String(),
		TenantID:             tenantID,
		CalculationID:        calcID,
		EmployeeID:           calc.EmployeeID,
		RuleBasisJSON:        `{"rule_version":"2026.1"}`,
		ProviderMetadataJSON: `{"engine_type":"STANDARD_ENGINE"}`,
		AuditedAt:            now,
	}
	return calc, audit
}

// TestPgStore_CreateCalculationWithAudit_RetriedCorrelationID_IsIdempotent
// proves a retried CalculateTax call resolves to the original calculation
// instead of creating a duplicate calculation and a duplicate audit entry.
func TestPgStore_CreateCalculationWithAudit_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	calc1, audit1 := newTestCalc(tenantID, "corr-calc-retry")
	created1, err := s.CreateCalculationWithAudit(ctx, calc1, audit1)
	if err != nil {
		t.Fatalf("first CreateCalculationWithAudit failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	calc2, audit2 := newTestCalc(tenantID, "corr-calc-retry")
	created2, err := s.CreateCalculationWithAudit(ctx, calc2, audit2)
	if err != nil {
		t.Fatalf("retried CreateCalculationWithAudit failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call")
	}
	if calc2.CalculationID != calc1.CalculationID {
		t.Fatalf("retried call resolved to a different calculation_id (%s) than the original (%s)", calc2.CalculationID, calc1.CalculationID)
	}

	var calcCount, auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tax_calculation_records WHERE tenant_id = $1`, tenantID).Scan(&calcCount); err != nil {
		t.Fatalf("calc count query failed: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tax_basis_audits WHERE tenant_id = $1`, tenantID).Scan(&auditCount); err != nil {
		t.Fatalf("audit count query failed: %v", err)
	}
	if calcCount != 1 {
		t.Fatalf("expected exactly 1 tax_calculation_records row, got %d", calcCount)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 tax_basis_audits row (no duplicate audit on replay), got %d", auditCount)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	calcA, auditA := newTestCalc(tenantA, "corr-tenant-a")
	if _, err := s.CreateCalculationWithAudit(ctxA, calcA, auditA); err != nil {
		t.Fatalf("CreateCalculationWithAudit (tenant A) failed: %v", err)
	}

	listB, err := s.ListCalculations(ctxB, "", "")
	if err != nil {
		t.Fatalf("ListCalculations under tenant B's context failed: %v", err)
	}
	for _, c := range listB {
		if c.CalculationID == calcA.CalculationID {
			t.Fatal("ISOLATION FAILURE: tenant B was able to see tenant A's tax calculation")
		}
	}

	if _, err := s.GetCalculation(ctxB, calcA.CalculationID); err != domain.ErrCalculationNotFound {
		t.Fatalf("expected ErrCalculationNotFound when tenant B looks up tenant A's calculation, got %v", err)
	}
}
