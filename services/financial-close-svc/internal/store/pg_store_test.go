package store_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS
		close_evidences, fiscal_periods, period_reopen_events,
		subledger_control_runs,
		accrual_recognition_instances, accrual_schedules,
		prepayment_recognition_instances, prepayment_schedules,
		allocation_run_result_lines, allocation_runs, allocation_rule_drivers, allocation_rules,
		fx_revaluation_items, fx_revaluation_runs,
		migration_crosswalk_entries, migration_batches,
		financial_snapshots,
		lineage_edges, lineage_projection_status
		CASCADE;`)

	// The DROP list above must be kept in sync with every CREATE TABLE the
	// glob below picks up: most of those CREATE TABLEs have no IF NOT EXISTS
	// guard (append-only evidence tables never need an idempotent create),
	// so a second test in this package re-running every migration against a
	// table this list forgot fails with "relation already exists" instead of
	// getting a genuinely clean schema.

	// EVERY *.up.sql, BY GLOB, NOT ONE NAMED FILE.
	//
	// This applied 000001 alone and the service has two, so 000002_force_rls was
	// never present. That matters most for TestPgStore_RLS_TenantIsolation in
	// this file: it asserts row-level isolation against a schema where FORCE ROW
	// LEVEL SECURITY had not been applied, so it could pass with the policy
	// bound to nobody.
	migrations, err := filepath.Glob(filepath.Join(base, "../../deployments/migrations/*.up.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("no *.up.sql under deployments/migrations: %v", err)
	}
	sort.Strings(migrations)

	for _, path := range migrations {
		sql, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(path), readErr)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", filepath.Base(path), err)
		}
	}

	return pool
}

// requireThrowawayDatabase refuses to run against anything not recognisably
// disposable. This suite DROPs fiscal_periods and close_evidences, and those
// rows are the record of which periods were sealed and what the books said when
// they were — "we can re-seed it" is not a consolation for close evidence. Only
// the database NAME vouches for it; a password that happens to contain "test"
// must not.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs fiscal_periods and close_evidences. Use "+
			"financial_close_test (or CI's testdb), not financial_close.", dbName)
	}
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
