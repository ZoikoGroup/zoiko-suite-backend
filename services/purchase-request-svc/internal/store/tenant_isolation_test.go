//go:build integration

// Package store_test contains cross-tenant isolation tests for PgStore.
//
// Services in this platform connect as the Postgres superuser
// (DB_USER=postgres). Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy text — set_config('app.tenant_id', …) has no
// effect because RLS never runs. The only real isolation guarantee is an
// explicit AND tenant_id = $N in every query's WHERE clause — this file
// proves that guarantee actually holds for every tenant-scoped method in
// this service, mirroring general-ledger-svc's and
// tenant-entity-registry-svc's isolation test suites (both found real,
// live-reproducible bugs this exact pattern was designed to catch).
//
// Each subtest:
//  1. Creates two independent tenants (A and B), each with their own request.
//  2. Executes the method under test with TENANT B's context but TENANT A's IDs.
//  3. Asserts no cross-tenant data is returned/mutated (nil / zero rows affected).
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

	"zoiko.io/purchase-request-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
	"zoiko.io/purchase-request-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	dbPort := uint32(15801 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.PostgresVersion("16.0.0")).
			Port(dbPort).
			Database("pr_isolation_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=pr_isolation_test user=postgres password=postgres sslmode=disable",
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

// requestFixture holds the key IDs for one tenant's seeded purchase request.
type requestFixture struct {
	tenantID  string
	entityID  string
	requestID string
}

func setupIsolationFixture(t *testing.T, tenantLabel string) requestFixture {
	t.Helper()
	ctx := context.Background()

	f := requestFixture{
		tenantID:  uuid.New().String(),
		entityID:  uuid.New().String(),
		requestID: uuid.New().String(),
	}
	tctx := svcmiddleware.WithTenant(ctx, f.tenantID)

	require.NoError(t, testStore.CreateRequest(tctx, &domain.PurchaseRequest{
		RequestID:              f.requestID,
		TenantID:               f.tenantID,
		LegalEntityID:          f.entityID,
		RequestedByPrincipalID: "test-" + tenantLabel,
		Description:            tenantLabel + " request",
		Amount:                 1000,
		CurrencyCode:           "USD",
		Status:                 domain.RequestStatusPending,
		CorrelationID:          "corr-" + tenantLabel,
	}))

	return f
}

func TestPgStore_TenantIsolation_GetRequest(t *testing.T) {
	a := setupIsolationFixture(t, "A-GetRequest")
	b := setupIsolationFixture(t, "B-GetRequest")

	// Probe: tenant B's context, tenant A's request ID.
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	got, err := testStore.GetRequest(ctxB, a.requestID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetRequest returned Tenant A's row under Tenant B's context")

	// Sanity: tenant B can still read its own request.
	gotOwn, err := testStore.GetRequest(ctxB, b.requestID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.requestID, gotOwn.RequestID)
}

func TestPgStore_TenantIsolation_TransitionRequest_Approve(t *testing.T) {
	a := setupIsolationFixture(t, "A-Approve")
	b := setupIsolationFixture(t, "B-Approve")

	// Tenant B attempts to approve Tenant A's request, using tenant B's own
	// tenantID as the scope argument — exactly what a handler bug would look
	// like if TenantID were taken from the request body instead of the
	// caller's real context.
	err := testStore.TransitionRequest(context.Background(), b.tenantID, a.requestID, domain.RequestStatusApproved, "attacker", nil)
	assert.ErrorIs(t, err, domain.ErrInvalidTransition,
		"ISOLATION FAILURE: tenant B was able to approve tenant A's request")

	// Verify tenant A's request is still PENDING.
	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetRequest(ctxA, a.requestID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.RequestStatusPending, got.Status,
		"ISOLATION FAILURE: tenant A's request status was mutated by tenant B")

	// Sanity: tenant B can still approve its OWN request.
	err = testStore.TransitionRequest(context.Background(), b.tenantID, b.requestID, domain.RequestStatusApproved, "b-admin", nil)
	require.NoError(t, err)
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	gotB, err := testStore.GetRequest(ctxB, b.requestID)
	require.NoError(t, err)
	assert.Equal(t, domain.RequestStatusApproved, gotB.Status)
}

func TestPgStore_TenantIsolation_TransitionRequest_Reject(t *testing.T) {
	a := setupIsolationFixture(t, "A-Reject")
	b := setupIsolationFixture(t, "B-Reject")

	reason := "attacker-supplied reason"
	err := testStore.TransitionRequest(context.Background(), b.tenantID, a.requestID, domain.RequestStatusRejected, "attacker", &reason)
	assert.ErrorIs(t, err, domain.ErrInvalidTransition,
		"ISOLATION FAILURE: tenant B was able to reject tenant A's request")

	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetRequest(ctxA, a.requestID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.RequestStatusPending, got.Status,
		"ISOLATION FAILURE: tenant A's request status was mutated by tenant B's reject attempt")
}

func TestPgStore_TenantIsolation_ListRequests(t *testing.T) {
	a := setupIsolationFixture(t, "A-List")
	_ = setupIsolationFixture(t, "B-List")

	// ListRequests requires tenant_id as a mandatory filter argument (not
	// derived from context), so it's structurally safe by construction — this
	// test proves that holds, not just assumes it.
	list, err := testStore.ListRequests(context.Background(), domain.ListRequestsFilter{TenantID: a.tenantID})
	require.NoError(t, err)
	for _, r := range list {
		assert.Equal(t, a.tenantID, r.TenantID, "ISOLATION FAILURE: ListRequests returned another tenant's row")
	}
}
