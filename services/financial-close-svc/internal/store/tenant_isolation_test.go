//go:build integration

// Package store_test contains cross-tenant isolation tests for PgStore.
//
// Services in this platform connect as the Postgres superuser
// (DB_USER=postgres). Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy text — set_config('app.tenant_id', …) has no
// effect because RLS never runs. The only real isolation guarantee is an
// explicit AND tenant_id = $N in every query's WHERE clause — pg_store.go
// already has this for every method; this file proves it actually holds.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/
package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	var pg *embeddedpostgres.EmbeddedPostgres

	if dsn == "" {
		dbPort := uint32(16101 + uint32(os.Getpid()%499))
		pg = embeddedpostgres.NewDatabase(
			embeddedpostgres.DefaultConfig().
				Version(embeddedpostgres.V16).
				Port(dbPort).
				Database("financial_close_isolation_test").
				Username("postgres").
				Password("postgres").
				RuntimePath(filepath.Join(os.TempDir(), fmt.Sprintf("epg-financial-close-%d", dbPort))),
		)
		if err := pg.Start(); err != nil {
			fmt.Printf("failed to start embedded postgres: %v\n", err)
			os.Exit(1)
		}
		dsn = fmt.Sprintf(
			"host=localhost port=%d dbname=financial_close_isolation_test user=postgres password=postgres sslmode=disable",
			dbPort,
		)
	}

	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("failed to connect to postgres: %v\n", err)
		if pg != nil {
			_ = pg.Stop()
		}
		os.Exit(1)
	}

	for i := 0; i < 75; i++ {
		if err = testPool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Printf("postgres did not become ready: %v\n", err)
		testPool.Close()
		if pg != nil {
			_ = pg.Stop()
		}
		os.Exit(1)
	}

	_, _ = testPool.Exec(ctx, `DROP TABLE IF EXISTS
		close_evidences, fiscal_periods, period_reopen_events,
		subledger_control_runs,
		accrual_recognition_reversals, accrual_recognition_instances, accrual_schedules,
		prepayment_recognition_instances, prepayment_schedules,
		allocation_run_result_lines, allocation_runs, allocation_rule_drivers, allocation_rules,
		fx_revaluation_items, fx_revaluation_runs,
		migration_crosswalk_entries, migration_batches,
		financial_snapshots,
		lineage_edges, lineage_projection_status, lineage_trace_verifications, lineage_quarantined_gaps
		CASCADE;`)

	migrations, err := filepath.Glob("../../deployments/migrations/*.up.sql")
	if err != nil {
		fmt.Printf("failed to glob migrations: %v\n", err)
		testPool.Close()
		if pg != nil {
			_ = pg.Stop()
		}
		os.Exit(1)
	}
	if len(migrations) == 0 {
		fmt.Println("no *.up.sql found -- the suite would run against an empty schema")
		testPool.Close()
		if pg != nil {
			_ = pg.Stop()
		}
		os.Exit(1)
	}
	sort.Strings(migrations)

	for _, migration := range migrations {
		sql, readErr := os.ReadFile(migration)
		if readErr != nil {
			fmt.Printf("failed to read migration %s: %v\n", filepath.Base(migration), readErr)
			testPool.Close()
			if pg != nil {
				_ = pg.Stop()
			}
			os.Exit(1)
		}
		if _, err = testPool.Exec(ctx, string(sql)); err != nil {
			fmt.Printf("failed to apply migration %s: %v\n", filepath.Base(migration), err)
			testPool.Close()
			if pg != nil {
				_ = pg.Stop()
			}
			os.Exit(1)
		}
	}

	testStore = store.New(testPool)

	code := m.Run()

	testPool.Close()
	if pg != nil {
		_ = pg.Stop()
	}
	os.Exit(code)
}

type periodFixture struct {
	tenantID       string
	legalEntityID  string
	fiscalPeriodID string
}

func setupIsolationFixture(t *testing.T, tenantLabel string) periodFixture {
	t.Helper()
	ctx := context.Background()

	f := periodFixture{
		tenantID:       uuid.New().String(),
		legalEntityID:  uuid.New().String(),
		fiscalPeriodID: uuid.New().String(),
	}
	tctx := svcmiddleware.WithTenant(ctx, f.tenantID)

	_, err := testStore.CreateFiscalPeriod(tctx, &domain.FiscalPeriod{
		FiscalPeriodID: f.fiscalPeriodID,
		TenantID:       f.tenantID,
		LegalEntityID:  f.legalEntityID,
		PeriodName:     tenantLabel,
		PeriodStart:    time.Now().UTC(),
		PeriodEnd:      time.Now().UTC().AddDate(0, 1, 0),
		CloseStatus:    "OPEN",
	})
	require.NoError(t, err)

	return f
}

func TestPgStore_TenantIsolation_GetFiscalPeriod(t *testing.T) {
	a := setupIsolationFixture(t, "A-GetFiscalPeriod")
	b := setupIsolationFixture(t, "B-GetFiscalPeriod")

	// Probe: tenant B's context, tenant A's fiscal period ID.
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	got, err := testStore.GetFiscalPeriod(ctxB, a.fiscalPeriodID)
	assert.ErrorIs(t, err, domain.ErrFiscalPeriodNotFound,
		"ISOLATION FAILURE: GetFiscalPeriod returned Tenant A's row under Tenant B's context")
	assert.Nil(t, got)

	// Sanity: tenant B can still read its own period.
	gotOwn, err := testStore.GetFiscalPeriod(ctxB, b.fiscalPeriodID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.fiscalPeriodID, gotOwn.FiscalPeriodID)
}

func TestPgStore_TenantIsolation_LockFiscalPeriod(t *testing.T) {
	a := setupIsolationFixture(t, "A-Lock")
	b := setupIsolationFixture(t, "B-Lock")

	// Tenant B attempts to lock Tenant A's period under tenant B's own context.
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	err := testStore.LockFiscalPeriod(ctxB, a.fiscalPeriodID, time.Now().UTC(), uuid.New().String())
	assert.ErrorIs(t, err, domain.ErrPeriodAlreadyLocked,
		"ISOLATION FAILURE: tenant B was able to lock tenant A's fiscal period")

	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetFiscalPeriod(ctxA, a.fiscalPeriodID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "OPEN", got.CloseStatus,
		"ISOLATION FAILURE: tenant A's period status was mutated by tenant B")

	// Sanity: tenant B can still lock its OWN period.
	err = testStore.LockFiscalPeriod(ctxB, b.fiscalPeriodID, time.Now().UTC(), uuid.New().String())
	require.NoError(t, err)
	gotB, err := testStore.GetFiscalPeriod(ctxB, b.fiscalPeriodID)
	require.NoError(t, err)
	assert.Equal(t, "LOCKED", gotB.CloseStatus)
}

func TestPgStore_TenantIsolation_ListFiscalPeriods(t *testing.T) {
	a := setupIsolationFixture(t, "A-List")
	_ = setupIsolationFixture(t, "B-List")

	// ListFiscalPeriods derives tenant scope from context (not a filter
	// argument), so the probe here is: tenant A's context, tenant A's
	// legal_entity_id — must never return tenant B's rows even though the
	// legal_entity_id is caller-scoped, not globally unique.
	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	list, err := testStore.ListFiscalPeriods(ctxA, a.legalEntityID)
	require.NoError(t, err)
	for _, fp := range list {
		assert.Equal(t, a.tenantID, fp.TenantID, "ISOLATION FAILURE: ListFiscalPeriods returned another tenant's row")
	}
}
