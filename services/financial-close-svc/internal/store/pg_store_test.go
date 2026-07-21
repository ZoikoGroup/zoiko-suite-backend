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

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migration from
// a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other service in this platform.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS close_evidences, fiscal_periods CASCADE;`)

	sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations/000001_initial_schema.up.sql"))
	if err != nil {
		t.Fatalf("failed to read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("failed to apply migration: %v", err)
	}

	return pool
}

// TestPgStore_CreateFiscalPeriod_Retried_IsIdempotent proves the idempotency
// guarantee against a REAL Postgres unique index — this is the exact
// scenario a network-timeout-triggered client retry produces, and it must
// resolve to the original fiscal period, never a duplicate.
func TestPgStore_CreateFiscalPeriod_Retried_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	fp1 := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.New().String(),
		TenantID:       tenantID,
		LegalEntityID:  legalEntityID,
		PeriodName:     "2026-Q3",
		PeriodStart:    time.Now().UTC(),
		PeriodEnd:      time.Now().UTC().Add(90 * 24 * time.Hour),
		CloseStatus:    "OPEN",
	}
	created1, err := s.CreateFiscalPeriod(ctx, fp1)
	if err != nil {
		t.Fatalf("first CreateFiscalPeriod failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	// Simulate a client retry: a fresh period (new FiscalPeriodID, as a real
	// client would generate) but the SAME (legal_entity_id, period_name).
	fp2 := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.New().String(),
		TenantID:       tenantID,
		LegalEntityID:  legalEntityID,
		PeriodName:     "2026-Q3",
		PeriodStart:    time.Now().UTC(),
		PeriodEnd:      time.Now().UTC().Add(90 * 24 * time.Hour),
		CloseStatus:    "OPEN",
	}
	created2, err := s.CreateFiscalPeriod(ctx, fp2)
	if err != nil {
		t.Fatalf("retried CreateFiscalPeriod failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a duplicate-period bug if it's true")
	}
	if fp2.FiscalPeriodID != fp1.FiscalPeriodID {
		t.Fatalf("retried call resolved to a different fiscal_period_id (%s) than the original (%s)", fp2.FiscalPeriodID, fp1.FiscalPeriodID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fiscal_periods WHERE tenant_id = $1 AND legal_entity_id = $2 AND period_name = $3`,
		tenantID, legalEntityID, "2026-Q3").Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("DUPLICATE PERIOD: expected exactly 1 fiscal_periods row, got %d", count)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	fpA := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.New().String(),
		TenantID:       tenantA,
		LegalEntityID:  uuid.New().String(),
		PeriodName:     "2026-Q3",
		PeriodStart:    time.Now().UTC(),
		PeriodEnd:      time.Now().UTC().Add(90 * 24 * time.Hour),
		CloseStatus:    "OPEN",
	}
	if _, err := s.CreateFiscalPeriod(ctxA, fpA); err != nil {
		t.Fatalf("CreateFiscalPeriod (tenant A) failed: %v", err)
	}

	// Query tenant A's period while scoped to tenant B's RLS context — RLS
	// must hide it entirely, proving tenant isolation actually holds, not
	// just that the column exists.
	_, err := s.GetFiscalPeriod(ctxB, fpA.FiscalPeriodID)
	if err == nil {
		t.Fatal("RLS failure: tenant B's session was able to read tenant A's fiscal period")
	}
}
