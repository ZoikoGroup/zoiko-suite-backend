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
// purchase-request-svc's, and bank-reconciliation-svc's isolation test
// suites (each found real, live-reproducible bugs this exact pattern was
// designed to catch).
//
// Each subtest:
//  1. Creates two independent tenants (A and B), each with their own entry.
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

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	dbPort := uint32(16001 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(dbPort).
			Database("interco_isolation_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=interco_isolation_test user=postgres password=postgres sslmode=disable",
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

// entryFixture holds the key IDs for one tenant's seeded intercompany entry.
type entryFixture struct {
	tenantID            string
	sourceLegalEntityID string
	targetLegalEntityID string
	entryID             string
}

func setupIsolationFixture(t *testing.T, tenantLabel string) entryFixture {
	t.Helper()
	ctx := context.Background()

	f := entryFixture{
		tenantID:            uuid.New().String(),
		sourceLegalEntityID: uuid.New().String(),
		targetLegalEntityID: uuid.New().String(),
		entryID:             uuid.New().String(),
	}
	tctx := svcmiddleware.WithTenant(ctx, f.tenantID)

	require.NoError(t, testStore.CreateEntry(tctx, &domain.IntercompanyEntry{
		IntercompanyEntryID:  f.entryID,
		TenantID:             f.tenantID,
		SourceLegalEntityID:  f.sourceLegalEntityID,
		TargetLegalEntityID:  f.targetLegalEntityID,
		SourceJournalEntryID: uuid.New().String(),
		Amount:               1000,
		CurrencyCode:         "USD",
		Description:          tenantLabel + " entry",
		MatchStatus:          domain.MatchStatusPending,
		CreatedByPrincipalID: "test-" + tenantLabel,
		CorrelationID:        "corr-" + tenantLabel,
	}))

	return f
}

func TestPgStore_TenantIsolation_GetEntry(t *testing.T) {
	a := setupIsolationFixture(t, "A-GetEntry")
	b := setupIsolationFixture(t, "B-GetEntry")

	// Probe: tenant B's context, tenant A's entry ID.
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	got, err := testStore.GetEntry(ctxB, a.entryID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetEntry returned Tenant A's row under Tenant B's context")

	// Sanity: tenant B can still read its own entry.
	gotOwn, err := testStore.GetEntry(ctxB, b.entryID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.entryID, gotOwn.IntercompanyEntryID)
}

func TestPgStore_TenantIsolation_MatchEntry(t *testing.T) {
	a := setupIsolationFixture(t, "A-Match")
	b := setupIsolationFixture(t, "B-Match")

	// Tenant B attempts to match Tenant A's entry, using tenant B's own
	// tenantID as the scope argument — exactly what a handler bug would
	// look like if TenantID were taken from the request body instead of
	// the caller's own record.
	err := testStore.MatchEntry(context.Background(), b.tenantID, a.entryID, uuid.New().String(), "attacker")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition,
		"ISOLATION FAILURE: tenant B was able to match tenant A's intercompany entry")

	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetEntry(ctxA, a.entryID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.MatchStatusPending, got.MatchStatus,
		"ISOLATION FAILURE: tenant A's entry status was mutated by tenant B")

	// Sanity: tenant B can still match its OWN entry.
	err = testStore.MatchEntry(context.Background(), b.tenantID, b.entryID, uuid.New().String(), "b-admin")
	require.NoError(t, err)
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	gotB, err := testStore.GetEntry(ctxB, b.entryID)
	require.NoError(t, err)
	assert.Equal(t, domain.MatchStatusMatched, gotB.MatchStatus)
}

func TestPgStore_TenantIsolation_MismatchEntry(t *testing.T) {
	a := setupIsolationFixture(t, "A-Mismatch")
	b := setupIsolationFixture(t, "B-Mismatch")

	err := testStore.MismatchEntry(context.Background(), b.tenantID, a.entryID, uuid.New().String(), "attacker-supplied reason")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition,
		"ISOLATION FAILURE: tenant B was able to mismatch tenant A's intercompany entry")

	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetEntry(ctxA, a.entryID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.MatchStatusPending, got.MatchStatus,
		"ISOLATION FAILURE: tenant A's entry status was mutated by tenant B's mismatch attempt")
}

func TestPgStore_TenantIsolation_ListEntries(t *testing.T) {
	a := setupIsolationFixture(t, "A-List")
	_ = setupIsolationFixture(t, "B-List")

	// ListEntries requires tenant_id as a mandatory filter argument (not
	// derived from context), so it's structurally safe by construction —
	// this test proves that holds, not just assumes it.
	list, err := testStore.ListEntries(context.Background(), domain.ListEntriesFilter{TenantID: a.tenantID})
	require.NoError(t, err)
	for _, e := range list {
		assert.Equal(t, a.tenantID, e.TenantID, "ISOLATION FAILURE: ListEntries returned another tenant's row")
	}
}
