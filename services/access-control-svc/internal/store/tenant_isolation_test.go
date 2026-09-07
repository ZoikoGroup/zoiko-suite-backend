//go:build integration

// Package store_test proves the tenant isolation of access-control-svc against
// a real Postgres, at both layers that are supposed to provide it.
//
// WHY BOTH LAYERS. This estate has been caught twice by testing only one.
// Every store query carries an explicit `tenant_id = $1` predicate AND the
// tables carry a tenant_isolation policy, and those are independent controls:
// the predicate holds whatever role connects, the policy holds whatever the
// query forgot. A suite that only exercises the predicate passes on a schema
// whose policies have never run — which is precisely the state the whole estate
// was in until the policies were made load-bearing.
//
// The catch, documented in purchase-order-svc's equivalent suite: embedded
// Postgres runs as its own superuser, and a superuser bypasses row security
// unconditionally. A naive isolation test therefore proves nothing about the
// policy no matter how correct the policy is. So this suite creates an ordinary
// NOSUPERUSER NOBYPASSRLS role — the same shape as
// deployments/scripts/create-app-roles.sh provisions — and drives the policy
// assertions through a second pool connected as that role. Without it, every
// assertion below would pass against a table with no policy at all.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=180s ./internal/store/
package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/store"
)

const (
	tenantA = "tenant-aaa"
	tenantB = "tenant-bbb"

	// appRole mirrors what create-app-roles.sh provisions in a real deployment:
	// login, DML only, and critically NOSUPERUSER NOBYPASSRLS so the policies
	// actually apply to it.
	appRole = "app_access_control_test"
	appPass = "app_access_control_test_pw"
)

var (
	ownerPool *pgxpool.Pool // superuser: runs migrations, seeds fixtures
	appPool   *pgxpool.Pool // NOSUPERUSER NOBYPASSRLS: RLS applies
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	dbPort := uint32(16601 + uint32(os.Getpid()%499))
	dbName := "access_control_isolation_test"

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V16).
			Port(dbPort).
			Database(dbName).
			Username("postgres").
			Password("postgres").
			// Isolated so this suite cannot collide with another service's
			// embedded instance over the shared default extraction directory.
			RuntimePath(filepath.Join(os.TempDir(), fmt.Sprintf("epg-access-control-%d", dbPort))),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("embedded postgres failed to start: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = pg.Stop() }()

	ctx := context.Background()
	ownerDSN := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/%s?sslmode=disable", dbPort, dbName)

	var err error
	ownerPool, err = pgxpool.New(ctx, ownerDSN)
	if err != nil {
		fmt.Printf("owner pool: %v\n", err)
		os.Exit(1)
	}

	if err := waitReady(ctx, ownerPool); err != nil {
		fmt.Printf("postgres never became ready: %v\n", err)
		os.Exit(1)
	}
	if err := applyMigrations(ctx, ownerPool); err != nil {
		fmt.Printf("migrations: %v\n", err)
		os.Exit(1)
	}
	if err := createAppRole(ctx, ownerPool, dbName); err != nil {
		fmt.Printf("app role: %v\n", err)
		os.Exit(1)
	}

	appDSN := fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", appRole, appPass, dbPort, dbName)
	appPool, err = pgxpool.New(ctx, appDSN)
	if err != nil {
		fmt.Printf("app pool: %v\n", err)
		os.Exit(1)
	}

	testStore = store.New(ownerPool)

	code := m.Run()

	appPool.Close()
	ownerPool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

func waitReady(ctx context.Context, pool *pgxpool.Pool) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if pool.Ping(ctx) == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("ping never succeeded")
}

// applyMigrations applies EVERY *.up.sql in filename order, by glob.
//
// Deliberately not a hardcoded list. Seven suites in this estate named their
// migrations explicitly and every one of them had fallen behind — each was
// missing its force_rls migration, so each ran against a schema no deployment
// has, and the RLS they claimed to test was not present. A glob cannot fall
// behind.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	paths, err := filepath.Glob(filepath.Join("..", "..", "deployments", "migrations", "*.up.sql"))
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no *.up.sql found — the suite would test an empty schema")
	}
	sort.Strings(paths)

	for _, p := range paths {
		sqlBytes, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

func createAppRole(ctx context.Context, pool *pgxpool.Pool, dbName string) error {
	stmts := []string{
		fmt.Sprintf("DROP ROLE IF EXISTS %s", appRole),
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS", appRole, appPass),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", dbName, appRole),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", appRole),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s", appRole),
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

func seedRole(t *testing.T, tenantID, roleCode string) string {
	t.Helper()
	id := uuid.NewString()
	now := time.Now().UTC()
	_, err := ownerPool.Exec(context.Background(), `
		INSERT INTO role_definitions (
			role_definition_id, tenant_id, role_code, role_name, role_scope_type,
			status, created_by_principal_id, correlation_id, created_at, updated_at
		) VALUES ($1,$2,$3,$4,'LEGAL_ENTITY','ACTIVE','admin-seed',$5,$6,$6)`,
		id, tenantID, roleCode, roleCode+" name", uuid.NewString(), now)
	require.NoError(t, err, "seeding a role via the owner pool")
	return id
}

// ── 1. The migration actually ran, and left the posture it claims ─────────────

// TestForceRowLevelSecurityIsOn is the assertion the rest of this file depends
// on. ENABLE alone exempts the table owner; only FORCE binds it. If this fails,
// every policy assertion below is meaningless even when it passes.
func TestForceRowLevelSecurityIsOn(t *testing.T) {
	for _, table := range []string{"role_definitions", "permission_bundle_defs"} {
		var enabled, forced bool
		err := ownerPool.QueryRow(context.Background(),
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).
			Scan(&enabled, &forced)
		require.NoError(t, err)
		require.True(t, enabled, "%s: row security not ENABLEd", table)
		require.True(t, forced,
			"%s: row security is ENABLE but not FORCE, so the table owner bypasses the policy and the isolation is only as good as the WHERE clause", table)
	}
}

// TestTenantPolicyCarriesWithCheck proves the write side is governed, not just
// the read side. A USING-only policy lets a caller INSERT a row into a tenant
// it cannot then read.
func TestTenantPolicyCarriesWithCheck(t *testing.T) {
	for _, table := range []string{"role_definitions", "permission_bundle_defs"} {
		var qual, withCheck *string
		err := ownerPool.QueryRow(context.Background(),
			`SELECT qual, with_check FROM pg_policies WHERE tablename = $1 AND policyname = 'tenant_isolation_policy'`, table).
			Scan(&qual, &withCheck)
		require.NoError(t, err, "%s: no tenant_isolation_policy found", table)
		require.NotNil(t, qual, "%s: policy has no USING clause", table)
		require.NotNil(t, withCheck,
			"%s: policy has USING but no WITH CHECK -- a caller can write a row into a tenant it cannot read", table)
	}
}

// TestStatusCheckConstraintRejectsUnknownStatus -- status was a bare
// VARCHAR(20) with its vocabulary in a comment, so any string persisted.
func TestStatusCheckConstraintRejectsUnknownStatus(t *testing.T) {
	now := time.Now().UTC()
	_, err := ownerPool.Exec(context.Background(), `
		INSERT INTO role_definitions (
			role_definition_id, tenant_id, role_code, role_name, role_scope_type,
			status, created_by_principal_id, correlation_id, created_at, updated_at
		) VALUES ($1,$2,'BAD_STATUS_ROLE','Bad','LEGAL_ENTITY','BANANA','admin',$3,$4,$4)`,
		uuid.NewString(), tenantA, uuid.NewString(), now)
	require.Error(t, err, "an unknown status was accepted; the CHECK constraint is not present")
	require.Contains(t, strings.ToLower(err.Error()), "role_definitions_status_check")
}

// ── 2. The policy binds an ordinary role ─────────────────────────────────────

// TestPolicyHidesOtherTenantsRows drives the read through the NOSUPERUSER pool
// with NO tenant_id predicate at all, so the only thing that can filter the
// result is the policy. As a superuser this returns every row.
func TestPolicyHidesOtherTenantsRows(t *testing.T) {
	seedRole(t, tenantA, "POLICY_READ_A_"+uuid.NewString()[:8])
	seedRole(t, tenantB, "POLICY_READ_B_"+uuid.NewString()[:8])

	ctx := context.Background()
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantA)
	require.NoError(t, err)

	var leaked int
	err = tx.QueryRow(ctx, "SELECT count(*) FROM role_definitions WHERE tenant_id = $1", tenantB).Scan(&leaked)
	require.NoError(t, err)
	require.Zero(t, leaked,
		"scoped to %s, an unfiltered read returned %d of %s's roles -- the policy is not binding this role", tenantA, leaked, tenantB)
}

// TestPolicyRefusesCrossTenantWrite is the WITH CHECK assertion, end to end:
// scoped to tenantA, inserting a row that claims tenantB must be refused by the
// database rather than accepted and hidden.
func TestPolicyRefusesCrossTenantWrite(t *testing.T) {
	ctx := context.Background()
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantA)
	require.NoError(t, err)

	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		INSERT INTO role_definitions (
			role_definition_id, tenant_id, role_code, role_name, role_scope_type,
			status, created_by_principal_id, correlation_id, created_at, updated_at
		) VALUES ($1,$2,$3,'Smuggled','LEGAL_ENTITY','ACTIVE','attacker',$4,$5,$5)`,
		uuid.NewString(), tenantB, "SMUGGLED_"+uuid.NewString()[:8], uuid.NewString(), now)

	require.Error(t, err,
		"scoped to %s, a row claiming %s was written -- WITH CHECK is absent, so a caller can author into a tenant it cannot read", tenantA, tenantB)
}

// TestPolicyFailsClosedWithNoTenantInstalled -- an unscoped connection must see
// nothing, not everything. NULLIF makes the predicate NULL rather than '',
// and NULL is not true.
func TestPolicyFailsClosedWithNoTenantInstalled(t *testing.T) {
	seedRole(t, tenantA, "FAILCLOSED_"+uuid.NewString()[:8])

	ctx := context.Background()
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	var visible int
	err = tx.QueryRow(ctx, "SELECT count(*) FROM role_definitions").Scan(&visible)
	require.NoError(t, err, "an unscoped read errored rather than returning nothing")
	require.Zero(t, visible, "with no tenant installed the connection saw %d rows; the policy fails open", visible)
}

// ── 3. The store's own predicate, independent of the policy ──────────────────

// TestStorePredicateScopesReads runs through PgStore on the OWNER pool, where
// RLS does not apply at all. Anything that isolates here is the explicit
// `tenant_id = $1` predicate and nothing else -- the control that survives a
// misconfigured role.
func TestStorePredicateScopesReads(t *testing.T) {
	idA := seedRole(t, tenantA, "PRED_A_"+uuid.NewString()[:8])
	idB := seedRole(t, tenantB, "PRED_B_"+uuid.NewString()[:8])

	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)

	got, err := testStore.GetRole(ctxA, idA)
	require.NoError(t, err)
	require.Equal(t, tenantA, got.TenantID)

	_, err = testStore.GetRole(ctxA, idB)
	require.ErrorIs(t, err, domain.ErrRoleNotFound,
		"tenant %s read tenant %s's role definition by id", tenantA, tenantB)
}

// TestStoreRefusesWithoutTenant -- a missing tenant is an error, not an
// unscoped query. Defaulting it would make a dropped header look like an empty
// catalogue.
func TestStoreRefusesWithoutTenant(t *testing.T) {
	_, err := testStore.ListRoles(context.Background(), "")
	require.ErrorIs(t, err, domain.ErrIdentityMissing)
}

// TestStoreUpdateCannotCrossTenants -- the write path carries the predicate too.
func TestStoreUpdateCannotCrossTenants(t *testing.T) {
	idB := seedRole(t, tenantB, "UPD_B_"+uuid.NewString()[:8])

	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	_, err := testStore.UpdateRole(ctxA, idB, "Renamed By Other Tenant", "RETIRED")
	require.ErrorIs(t, err, domain.ErrRoleNotFound,
		"tenant %s retired tenant %s's role", tenantA, tenantB)

	// And the row is untouched.
	var status, name string
	require.NoError(t, ownerPool.QueryRow(context.Background(),
		"SELECT status, role_name FROM role_definitions WHERE role_definition_id = $1", idB).Scan(&status, &name))
	require.Equal(t, "ACTIVE", status)
	require.NotEqual(t, "Renamed By Other Tenant", name)
}
