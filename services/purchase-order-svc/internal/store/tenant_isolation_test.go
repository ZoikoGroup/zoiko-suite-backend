//go:build integration

// Package store_test contains cross-tenant isolation tests for PgStore.
//
// This file proves the explicit `AND tenant_id = $N` predicate holds for every
// tenant-scoped method in this service, mirroring purchase-request-svc's and
// general-ledger-svc's isolation suites (both found real, live-reproducible
// bugs this exact pattern was designed to catch).
//
// It used to say that predicate was the ONLY isolation guarantee, because every
// service connected as the Postgres superuser and a superuser bypasses
// row-level security unconditionally — so the policies were inert and
// set_config('app.tenant_id', …) did nothing. That is no longer true: these
// services now connect as an ordinary NOSUPERUSER NOBYPASSRLS role
// (deployments/scripts/create-app-roles.sh) and the policy is enforced.
//
// The predicate is still the guarantee this file tests, and still worth
// testing: it is the control that does not depend on the role being right.
// But note what this suite CANNOT tell you — it runs an embedded Postgres as
// that server's own superuser, so RLS never applies here no matter what the
// deployment does. The policy is exercised instead by running the
// TEST_DATABASE_URL suites against an ordinary role.
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
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
	"zoiko.io/purchase-order-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	dbPort := uint32(15901 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			// Version pinned explicitly — see the doc comment on
			// embeddedpostgres.DefaultConfig() in audit-event-store-svc's
			// main_integration_test.go for why: the unpinned default floats
			// to whatever major the library calls "latest," and that patch
			// build can stop resolving from the remote binary repo with no
			// code change on our side (this is what broke PR #105's CI).
			Version(embeddedpostgres.V16).
			Port(dbPort).
			Database("po_isolation_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=po_isolation_test user=postgres password=postgres sslmode=disable",
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

	// EVERY *.up.sql, BY GLOB, NOT ONE NAMED FILE.
	//
	// This applied 000001 alone, so 000002_force_rls and
	// 000003_policy_empty_tenant_is_null were never present -- a suite whose
	// whole purpose is tenant isolation ran against a schema where the isolation
	// had not been applied. A named file only stays correct while somebody
	// remembers to edit it, and nobody did.
	migrations, err := filepath.Glob("../../deployments/migrations/*.up.sql")
	if err != nil || len(migrations) == 0 {
		// Fatal, not a quiet skip: no migrations means no schema, and every
		// assertion below would then fail for an unrelated reason.
		fmt.Printf("no *.up.sql under deployments/migrations: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}
	sort.Strings(migrations)

	for _, migration := range migrations {
		sql, readErr := os.ReadFile(migration)
		if readErr != nil {
			fmt.Printf("failed to read migration %s: %v\n", filepath.Base(migration), readErr)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
		if _, err = testPool.Exec(ctx, string(sql)); err != nil {
			fmt.Printf("failed to apply migration %s: %v\n", filepath.Base(migration), err)
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

// orderFixture holds the key IDs for one tenant's seeded purchase order.
type orderFixture struct {
	tenantID string
	entityID string
	orderID  string
}

func setupIsolationFixture(t *testing.T, tenantLabel string) orderFixture {
	t.Helper()
	ctx := context.Background()

	f := orderFixture{
		tenantID: uuid.New().String(),
		entityID: uuid.New().String(),
		orderID:  uuid.New().String(),
	}
	tctx := svcmiddleware.WithTenant(ctx, f.tenantID)

	o := &domain.PurchaseOrder{
		PurchaseOrderID:     f.orderID,
		TenantID:            f.tenantID,
		LegalEntityID:       f.entityID,
		IssuedByPrincipalID: "test-" + tenantLabel,
		TotalAmount:         1000,
		CurrencyCode:        "USD",
		CorrelationID:       "corr-" + tenantLabel,
	}
	created, err := testStore.CreateOrder(tctx, o)
	require.NoError(t, err)
	require.True(t, created)

	return f
}

func TestPgStore_TenantIsolation_GetOrder(t *testing.T) {
	a := setupIsolationFixture(t, "A-GetOrder")
	b := setupIsolationFixture(t, "B-GetOrder")

	// Probe: tenant B's context, tenant A's order ID.
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	got, err := testStore.GetOrder(ctxB, a.orderID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetOrder returned Tenant A's row under Tenant B's context")

	// Sanity: tenant B can still read its own order.
	gotOwn, err := testStore.GetOrder(ctxB, b.orderID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.orderID, gotOwn.PurchaseOrderID)
}

func TestPgStore_TenantIsolation_AmendOrder(t *testing.T) {
	a := setupIsolationFixture(t, "A-Amend")
	b := setupIsolationFixture(t, "B-Amend")

	// Tenant B attempts to amend Tenant A's order, using tenant B's own
	// tenantID as the scope argument — exactly what a handler bug would
	// look like if TenantID were taken from the request body instead of
	// the caller's real context.
	_, err := testStore.AmendOrder(context.Background(), b.tenantID, a.orderID, 9999, "attacker amend", "attacker")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition,
		"ISOLATION FAILURE: tenant B was able to amend tenant A's order")

	// Verify tenant A's order is unchanged.
	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetOrder(ctxA, a.orderID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, float64(1000), got.TotalAmount,
		"ISOLATION FAILURE: tenant A's order total_amount was mutated by tenant B")
	assert.Equal(t, 1, got.Version)

	// Sanity: tenant B can still amend its OWN order.
	updated, err := testStore.AmendOrder(context.Background(), b.tenantID, b.orderID, 2000, "legit amend", "b-admin")
	require.NoError(t, err)
	assert.Equal(t, float64(2000), updated.TotalAmount)
	assert.Equal(t, 2, updated.Version)
}

func TestPgStore_TenantIsolation_CloseOrder(t *testing.T) {
	a := setupIsolationFixture(t, "A-Close")
	b := setupIsolationFixture(t, "B-Close")

	_, err := testStore.CloseOrder(context.Background(), b.tenantID, a.orderID, "attacker")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition,
		"ISOLATION FAILURE: tenant B was able to close tenant A's order")

	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetOrder(ctxA, a.orderID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.OrderStatusIssued, got.Status,
		"ISOLATION FAILURE: tenant A's order status was mutated by tenant B's close attempt")

	// Sanity: tenant B can still close its OWN order.
	updated, err := testStore.CloseOrder(context.Background(), b.tenantID, b.orderID, "b-admin")
	require.NoError(t, err)
	assert.Equal(t, domain.OrderStatusClosed, updated.Status)
}

func TestPgStore_TenantIsolation_ListOrders(t *testing.T) {
	a := setupIsolationFixture(t, "A-List")
	_ = setupIsolationFixture(t, "B-List")

	// ListOrders requires tenant_id as a mandatory filter argument (not
	// derived from context), so it's structurally safe by construction —
	// this test proves that holds, not just assumes it.
	list, err := testStore.ListOrders(context.Background(), domain.ListOrdersFilter{TenantID: a.tenantID})
	require.NoError(t, err)
	for _, o := range list {
		assert.Equal(t, a.tenantID, o.TenantID, "ISOLATION FAILURE: ListOrders returned another tenant's row")
	}
}
