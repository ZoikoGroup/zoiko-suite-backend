//go:build integration

// Package store_test contains cross-tenant isolation tests for PgStore.
//
// Services in this platform connect as the Postgres superuser
// (DB_USER=postgres). Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy text — set_config('app.tenant_id', …) has no
// effect because RLS never runs. The only real isolation guarantee is an
// explicit AND tenant_id = $N in every query's WHERE clause — this file
// proves that guarantee actually holds for every tenant-scoped method in
// this service, mirroring general-ledger-svc's, tenant-entity-registry-svc's,
// and purchase-request-svc's isolation test suites (each found real,
// live-reproducible bugs this exact pattern was designed to catch).
//
// Each subtest:
//  1. Creates two independent tenants (A and B), each with their own line.
//  2. Executes the method under test with TENANT B's context/scope but
//     TENANT A's IDs.
//  3. Asserts no cross-tenant data is returned/mutated (nil / zero rows
//     affected).
//  4. Asserts tenant B can still read/mutate its own data (the fix must not
//     over-restrict).
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/
package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
	"zoiko.io/bank-reconciliation-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	dbPort := uint32(15901 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.PostgresVersion("16.0.0")).
			Port(dbPort).
			Database("bankrec_isolation_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=bankrec_isolation_test user=postgres password=postgres sslmode=disable",
		dbPort,
	)

	ctx := context.Background()
	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("failed to connect to postgres: %v\n", err)
		_ = pg.Stop()
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
		_ = pg.Stop()
		os.Exit(1)
	}

	sql, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	if err != nil {
		fmt.Printf("failed to read migration: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}
	if _, err = testPool.Exec(ctx, string(sql)); err != nil {
		fmt.Printf("failed to apply migration: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}

	testStore = store.New(testPool, zap.NewNop())

	code := m.Run()

	testPool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

// lineFixture holds the key IDs for one tenant's seeded statement line.
type lineFixture struct {
	tenantID        string
	entityID        string
	bankAccountID   string
	statementLineID string
}

func setupIsolationFixture(t *testing.T, tenantLabel string) lineFixture {
	t.Helper()
	ctx := context.Background()

	f := lineFixture{
		tenantID:        uuid.New().String(),
		entityID:        uuid.New().String(),
		bankAccountID:   uuid.New().String(),
		statementLineID: uuid.New().String(),
	}
	tctx := svcmiddleware.WithTenant(ctx, f.tenantID)

	require.NoError(t, testStore.CreateStatementLine(tctx, &domain.StatementLine{
		StatementLineID: f.statementLineID,
		TenantID:        f.tenantID,
		LegalEntityID:   f.entityID,
		BankAccountID:   f.bankAccountID,
		StatementDate:   time.Now().UTC().Truncate(24 * time.Hour),
		Amount:          1000,
		CurrencyCode:    "USD",
		BankReference:   "ACH-" + tenantLabel,
		Status:          domain.StatementLineStatusUnmatched,
		CorrelationID:   "corr-" + tenantLabel,
	}))

	return f
}

func TestPgStore_TenantIsolation_GetStatementLine(t *testing.T) {
	a := setupIsolationFixture(t, "A-GetStatementLine")
	b := setupIsolationFixture(t, "B-GetStatementLine")

	// Probe: tenant B's context, tenant A's line ID.
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	got, err := testStore.GetStatementLine(ctxB, a.statementLineID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetStatementLine returned Tenant A's row under Tenant B's context")

	// Sanity: tenant B can still read its own line.
	gotOwn, err := testStore.GetStatementLine(ctxB, b.statementLineID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.statementLineID, gotOwn.StatementLineID)
}

func TestPgStore_TenantIsolation_MatchStatementLine(t *testing.T) {
	a := setupIsolationFixture(t, "A-Match")
	b := setupIsolationFixture(t, "B-Match")

	// Tenant B attempts to match Tenant A's line, using tenant B's own
	// tenantID as the scope argument — exactly what a handler bug would
	// look like if TenantID were taken from the request body instead of
	// the caller's own record.
	err := testStore.MatchStatementLine(context.Background(), b.tenantID, a.statementLineID, uuid.New().String(), "attacker")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition,
		"ISOLATION FAILURE: tenant B was able to match tenant A's statement line")

	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetStatementLine(ctxA, a.statementLineID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.StatementLineStatusUnmatched, got.Status,
		"ISOLATION FAILURE: tenant A's line status was mutated by tenant B")

	// Sanity: tenant B can still match its OWN line.
	err = testStore.MatchStatementLine(context.Background(), b.tenantID, b.statementLineID, uuid.New().String(), "b-admin")
	require.NoError(t, err)
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	gotB, err := testStore.GetStatementLine(ctxB, b.statementLineID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatementLineStatusMatched, gotB.Status)
}

func TestPgStore_TenantIsolation_FlagException(t *testing.T) {
	a := setupIsolationFixture(t, "A-Flag")
	b := setupIsolationFixture(t, "B-Flag")

	err := testStore.FlagException(context.Background(), b.tenantID, a.statementLineID, "attacker-supplied reason", "attacker")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition,
		"ISOLATION FAILURE: tenant B was able to flag an exception on tenant A's statement line")

	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetStatementLine(ctxA, a.statementLineID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.StatementLineStatusUnmatched, got.Status,
		"ISOLATION FAILURE: tenant A's line status was mutated by tenant B's flag attempt")
}

func TestPgStore_TenantIsolation_ListStatementLines(t *testing.T) {
	a := setupIsolationFixture(t, "A-List")
	_ = setupIsolationFixture(t, "B-List")

	// ListStatementLines requires tenant_id as a mandatory filter argument
	// (not derived from context), so it's structurally safe by
	// construction — this test proves that holds, not just assumes it.
	list, err := testStore.ListStatementLines(context.Background(), domain.ListStatementLinesFilter{TenantID: a.tenantID})
	require.NoError(t, err)
	for _, l := range list {
		assert.Equal(t, a.tenantID, l.TenantID, "ISOLATION FAILURE: ListStatementLines returned another tenant's row")
	}
}

func TestPgStore_TenantIsolation_CountUnmatched(t *testing.T) {
	a := setupIsolationFixture(t, "A-Count")
	b := setupIsolationFixture(t, "B-Count")

	statementDate := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02")

	// Tenant B's count must not include tenant A's UNMATCHED line, even
	// though both share the same statement_date (bank_account_id differs
	// per fixture too, but tenant_id alone must already be sufficient).
	countB, err := testStore.CountUnmatched(context.Background(), b.tenantID, b.bankAccountID, statementDate)
	require.NoError(t, err)
	assert.Equal(t, 1, countB, "ISOLATION FAILURE: CountUnmatched for tenant B did not reflect exactly its own UNMATCHED line")

	countCrossTenant, err := testStore.CountUnmatched(context.Background(), b.tenantID, a.bankAccountID, statementDate)
	require.NoError(t, err)
	assert.Equal(t, 0, countCrossTenant, "ISOLATION FAILURE: CountUnmatched under tenant B's scope counted tenant A's bank account")
}
