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

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_idempotency_index.up.sql",
		"000003_add_gl_cash_account_code.up.sql",
	} {
		sql, err := os.ReadFile("../../deployments/migrations/" + migration)
		if err != nil {
			fmt.Printf("failed to read migration %s: %v\n", migration, err)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
		if _, err = testPool.Exec(ctx, string(sql)); err != nil {
			fmt.Printf("failed to apply migration %s: %v\n", migration, err)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
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

	_, err := testStore.CreateStatementLine(tctx, &domain.StatementLine{
		StatementLineID: f.statementLineID,
		TenantID:        f.tenantID,
		LegalEntityID:   f.entityID,
		BankAccountID:   f.bankAccountID,
		StatementDate:   time.Now().UTC().Truncate(24 * time.Hour),
		Amount:          1000,
		CurrencyCode:    "USD",
		BankReference:   "ACH-" + tenantLabel,
		Status:          domain.StatementLineStatusUnmatched,
		CorrelationID:   "corr-" + tenantLabel + "-" + f.statementLineID,
	})
	require.NoError(t, err)

	return f
}

// TestPgStore_CreateStatementLine_RetriedCorrelationID_IsIdempotent proves
// the idempotency guarantee against a REAL Postgres unique index — this is
// the exact scenario a network-timeout-triggered client retry produces, and
// it must resolve to the original line, never a duplicate.
func TestPgStore_CreateStatementLine_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	line1 := &domain.StatementLine{
		StatementLineID: uuid.New().String(),
		TenantID:        tenantID,
		LegalEntityID:   uuid.New().String(),
		BankAccountID:   uuid.New().String(),
		StatementDate:   time.Now().UTC().Truncate(24 * time.Hour),
		Amount:          1000,
		CurrencyCode:    "USD",
		BankReference:   "ACH-retry",
		Status:          domain.StatementLineStatusUnmatched,
		CorrelationID:   "corr-retry-1",
	}
	created1, err := testStore.CreateStatementLine(ctx, line1)
	require.NoError(t, err)
	assert.True(t, created1, "expected created=true on the first call")

	line2 := &domain.StatementLine{
		StatementLineID: uuid.New().String(),
		TenantID:        tenantID,
		LegalEntityID:   uuid.New().String(),
		BankAccountID:   uuid.New().String(),
		StatementDate:   time.Now().UTC().Truncate(24 * time.Hour),
		Amount:          1000,
		CurrencyCode:    "USD",
		BankReference:   "ACH-retry",
		Status:          domain.StatementLineStatusUnmatched,
		CorrelationID:   "corr-retry-1",
	}
	created2, err := testStore.CreateStatementLine(ctx, line2)
	require.NoError(t, err)
	assert.False(t, created2, "expected created=false on the retried call — this is a duplicate-line bug if it's true")
	assert.Equal(t, line1.StatementLineID, line2.StatementLineID, "retried call must resolve to the original statement_line_id")

	var count int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT COUNT(*) FROM statement_lines WHERE tenant_id = $1 AND correlation_id = $2`,
		tenantID, "corr-retry-1").Scan(&count))
	assert.Equal(t, 1, count, "DUPLICATE LINE: expected exactly 1 statement_lines row for this correlation_id")
}

// TestPgStore_CreateStatementLine_WritesContextTenantNotStructTenant proves
// the cross-tenant WRITE is closed.
//
// The INSERT used to take its tenant_id from the struct — which the handler
// filled from the request body — while only the RLS scope came from the
// verified context. This pool connects as a superuser, so RLS never runs and
// nothing else stood in the way: a caller could put a row in another tenant's
// register. The assertion is on the DATABASE, not the returned struct, since
// the struct is exactly what was trusted before.
func TestPgStore_CreateStatementLine_WritesContextTenantNotStructTenant(t *testing.T) {
	verified := uuid.New().String()
	victim := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), verified)

	lineID := uuid.New().String()
	_, err := testStore.CreateStatementLine(ctx, &domain.StatementLine{
		StatementLineID: lineID,
		TenantID:        victim, // what a hostile body would carry
		LegalEntityID:   uuid.New().String(),
		BankAccountID:   uuid.New().String(),
		StatementDate:   time.Now().UTC().Truncate(24 * time.Hour),
		Amount:          1000,
		CurrencyCode:    "USD",
		BankReference:   "ACH-crosstenant",
		Status:          domain.StatementLineStatusUnmatched,
		CorrelationID:   "corr-crosstenant-" + lineID,
	})
	require.NoError(t, err)

	var stored string
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT tenant_id::text FROM statement_lines WHERE statement_line_id = $1`, lineID).Scan(&stored))
	assert.Equal(t, verified, stored,
		"CROSS-TENANT WRITE: the row landed in the tenant named by the caller's data, not the caller's verified scope")

	var victimRows int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM statement_lines WHERE tenant_id = $1`, victim).Scan(&victimRows))
	assert.Zero(t, victimRows, "a row was planted in another tenant's register")
}

// StatementLegalEntities is what binds CompleteStatement's authorization
// check to the resource it authorizes, so it must not see across tenants
// either.
func TestPgStore_TenantIsolation_StatementLegalEntities(t *testing.T) {
	a := setupIsolationFixture(t, "A-LegalEntities")
	b := setupIsolationFixture(t, "B-LegalEntities")

	date := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02")
	bctx := svcmiddleware.WithTenant(context.Background(), b.tenantID)

	// Tenant B asking about tenant A's bank account learns nothing.
	got, err := testStore.StatementLegalEntities(bctx, b.tenantID, a.bankAccountID, date)
	require.NoError(t, err)
	assert.Empty(t, got, "CROSS-TENANT READ: another tenant's legal entities were returned")

	// ...but B can still see its own.
	own, err := testStore.StatementLegalEntities(bctx, b.tenantID, b.bankAccountID, date)
	require.NoError(t, err)
	assert.Equal(t, []string{b.entityID}, own, "the fix must not over-restrict")
}

// A malformed uuid or date is the caller's mistake, not an outage. Without
// the 22P02/22007 mapping these surfaced as 503 store_unavailable.
func TestPgStore_MalformedIdentifiers_MapToInvalidIdentifier(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	_, err := testStore.GetStatementLine(ctx, "not-a-uuid")
	assert.ErrorIs(t, err, domain.ErrInvalidIdentifier, "a malformed statement_line_id must not read as a store outage")

	_, err = testStore.CountUnmatched(ctx, tenantID, "not-a-uuid", "2026-07-01")
	assert.ErrorIs(t, err, domain.ErrInvalidIdentifier, "a malformed bank_account_id must not read as a store outage")

	_, err = testStore.CountUnmatched(ctx, tenantID, uuid.New().String(), "not-a-date")
	assert.ErrorIs(t, err, domain.ErrInvalidIdentifier, "a malformed statement_date must not read as a store outage")
}

// The register was unbounded. Reads are also capped at MaxListLimit so a
// caller cannot ask for the whole history by naming a large number.
func TestPgStore_ListStatementLines_IsBounded(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	entityID := uuid.New().String()
	bankAccountID := uuid.New().String()

	for i := 0; i < 5; i++ {
		id := uuid.New().String()
		_, err := testStore.CreateStatementLine(ctx, &domain.StatementLine{
			StatementLineID: id,
			TenantID:        tenantID,
			LegalEntityID:   entityID,
			BankAccountID:   bankAccountID,
			StatementDate:   time.Now().UTC().Truncate(24 * time.Hour),
			Amount:          float64(100 + i),
			CurrencyCode:    "USD",
			BankReference:   "ACH-bounded",
			Status:          domain.StatementLineStatusUnmatched,
			CorrelationID:   "corr-bounded-" + id,
		})
		require.NoError(t, err)
	}

	got, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{TenantID: tenantID, Limit: 2})
	require.NoError(t, err)
	assert.Len(t, got, 2, "LIMIT was not applied")

	// An empty result is an empty slice, never nil — otherwise it encodes as
	// JSON null and every consumer has to special-case it.
	empty, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{TenantID: uuid.New().String()})
	require.NoError(t, err)
	assert.NotNil(t, empty, "an empty register must be [] and not JSON null")
	assert.Empty(t, empty)
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
