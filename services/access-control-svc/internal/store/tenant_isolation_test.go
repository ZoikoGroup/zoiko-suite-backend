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
	"encoding/json"
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
		// event_outbox.outbox_id is BIGSERIAL. Without USAGE on its sequence
		// every enqueue fails as the app role with "permission denied for
		// sequence" — and because the enqueue shares the write's transaction,
		// the write fails with it. A grant list that covers tables but not
		// sequences is a grant list that breaks the moment a table gets an
		// identity column.
		fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", appRole),
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
// nothing, not everything. NULLIF makes the predicate NULL rather than an empty
// string, and NULL is not true.
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
	_, err := testStore.ListRoles(context.Background(), domain.ListFilter{})
	require.ErrorIs(t, err, domain.ErrIdentityMissing)
}

// TestStoreUpdateCannotCrossTenants -- the write path carries the predicate too.
func TestStoreUpdateCannotCrossTenants(t *testing.T) {
	idB := seedRole(t, tenantB, "UPD_B_"+uuid.NewString()[:8])

	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	_, err := testStore.UpdateRole(ctxA, idB, "Renamed By Other Tenant", "RETIRED", "admin-seed")
	require.ErrorIs(t, err, domain.ErrRoleNotFound,
		"tenant %s retired tenant %s's role", tenantA, tenantB)

	// And the row is untouched.
	var status, name string
	require.NoError(t, ownerPool.QueryRow(context.Background(),
		"SELECT status, role_name FROM role_definitions WHERE role_definition_id = $1", idB).Scan(&status, &name))
	require.Equal(t, "ACTIVE", status)
	require.NotEqual(t, "Renamed By Other Tenant", name)
}

// ── 4. The outbox ────────────────────────────────────────────────────────────

// TestOutboxPolicyAdmitsTheRelayAndNobodyElse pins the one deliberate
// cross-tenant escape hatch in this schema.
//
// The relay drains every tenant's backlog from a single loop, so it cannot run
// under a tenant scope. Running it UNSCOPED would be worse than wrong: under
// FORCE ROW LEVEL SECURITY it would simply select nothing, with no error, and
// present as a relay that publishes nothing while reporting perfect health.
// migration 000004 admits it by a named capability instead, and this asserts
// both halves — that app.outbox_relay opens the table, and that nothing else
// does.
func TestOutboxPolicyAdmitsTheRelayAndNobodyElse(t *testing.T) {
	ctx := context.Background()
	seedOutbox(t, tenantA, "role.updated")
	seedOutbox(t, tenantB, "role.updated")

	// Scoped to tenant A: only tenant A's row is visible.
	scoped := countOutbox(t, "SELECT set_config('app.tenant_id', '"+tenantA+"', true)")
	require.Equal(t, 1, scoped, "a tenant-scoped reader saw another tenant's outbox rows")

	// Unscoped: nothing at all. Fail-closed by SQL semantics.
	require.Equal(t, 0, countOutbox(t, "SELECT 1"),
		"an unscoped reader saw outbox rows — the policy is not fail-closed")

	// As the relay: everything.
	require.GreaterOrEqual(t, countOutbox(t, "SELECT set_config('app.outbox_relay', 'true', true)"), 2,
		"the relay could not see every tenant's backlog — it would publish nothing and report no error")

	// And the store's own relay path agrees.
	pending, _, err := testStore.OutboxDepth(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, pending, int64(2))
}

// TestCreateRoleEnqueuesInTheSameTransaction is the property the outbox exists
// for. The event and the state change are one commit: neither can exist without
// the other.
func TestCreateRoleEnqueuesInTheSameTransaction(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)
	code := "OUTBOX_" + uuid.NewString()[:8]
	role := &domain.RoleDefinition{
		RoleDefinitionID: uuid.NewString(), TenantID: tenantA, RoleCode: code,
		RoleName: "outbox probe", RoleScopeType: "TENANT", Status: domain.RoleStatusActive,
		CreatedByPrincipalID: "admin-seed", CorrelationID: uuid.NewString(),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	created, err := testStore.CreateRole(ctx, role, "admin-seed")
	require.NoError(t, err)
	require.True(t, created)

	var payload []byte
	require.NoError(t, ownerPool.QueryRow(ctx, `
		SELECT payload FROM event_outbox
		WHERE tenant_id = $1 AND event_type = 'role.created' AND aggregate_key = $2`,
		tenantA, role.RoleDefinitionID).Scan(&payload),
		"role.created was not enqueued in the transaction that created the role")

	var env struct {
		EventType string `json:"event_type"`
		Payload   struct {
			RoleID string `json:"role_id"`
		} `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(payload, &env))
	require.Equal(t, "role.created", env.EventType)
	require.Equal(t, role.RoleDefinitionID, env.Payload.RoleID)
}

// TestUpdateRoleEnqueuesRoleUpdatedWithRoleID is the defect this service
// shipped with, pinned at the layer that writes it.
//
// identity-context-svc revokes the sessions of everyone holding a role when it
// sees role.updated, and it reads payload.role_id. The event carried only
// role_definition_id, so the field it reads was always empty and it dropped
// every one. Retiring a role therefore left every session that already held it
// carrying the bundles it used to grant.
func TestUpdateRoleEnqueuesRoleUpdatedWithRoleID(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)
	id := seedRole(t, tenantA, "RETIRE_"+uuid.NewString()[:8])

	_, err := testStore.UpdateRole(ctx, id, "", "RETIRED", "admin-seed")
	require.NoError(t, err)

	var payload []byte
	require.NoError(t, ownerPool.QueryRow(ctx, `
		SELECT payload FROM event_outbox
		WHERE tenant_id = $1 AND event_type = 'role.updated' AND aggregate_key = $2`,
		tenantA, id).Scan(&payload))

	var env struct {
		Payload struct {
			RoleID string `json:"role_id"`
			Status string `json:"status"`
		} `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(payload, &env))
	require.Equal(t, id, env.Payload.RoleID,
		"role.updated with an empty role_id is dropped by identity-context-svc — no session is ever revoked")
	require.Equal(t, "RETIRED", env.Payload.Status)
}

// TestClaimOutboxLeavesRowsOnPublishFailure -- delivery may fail; the fact may
// not be lost. A failed publish must leave published_at NULL so the next tick
// retries, and must record why on the row itself.
func TestClaimOutboxLeavesRowsOnPublishFailure(t *testing.T) {
	ctx := context.Background()
	seedOutbox(t, tenantA, "role.created")

	before := pendingCount(t)
	require.Greater(t, before, int64(0))

	err := testStore.ClaimOutbox(ctx, 100, func([]store.OutboxRecord) error {
		return fmt.Errorf("broker unavailable")
	})
	require.Error(t, err)
	require.Equal(t, before, pendingCount(t),
		"a failed publish marked rows delivered — those events are gone")

	var attempts int
	var lastErr *string
	require.NoError(t, ownerPool.QueryRow(ctx,
		"SELECT max(attempts), max(last_error) FROM event_outbox WHERE published_at IS NULL").Scan(&attempts, &lastErr))
	require.GreaterOrEqual(t, attempts, 1, "the failed attempt was not recorded on the row")
	require.NotNil(t, lastErr)

	// And a successful drain clears the backlog.
	require.NoError(t, testStore.ClaimOutbox(ctx, 100, func(recs []store.OutboxRecord) error {
		require.NotEmpty(t, recs)
		return nil
	}))
	require.Equal(t, int64(0), pendingCount(t))
}

// TestMalformedIDsAreNotFoundNotOutages -- role_definition_id is a UUID column,
// so a non-UUID string raises 22P02 from Postgres rather than returning no
// rows. That used to leave as 503 store_unavailable with the raw SQLSTATE in
// the body: a database outage reported for a request that simply named nothing.
func TestMalformedIDsAreNotFoundNotOutages(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)

	_, err := testStore.GetRole(ctx, "not-a-uuid")
	require.ErrorIs(t, err, domain.ErrRoleNotFound)

	_, err = testStore.GetBundle(ctx, "not-a-uuid", "also-not-a-uuid")
	require.ErrorIs(t, err, domain.ErrBundleNotFound)

	_, err = testStore.UpdateRole(ctx, "not-a-uuid", "x", "RETIRED", "admin-seed")
	require.ErrorIs(t, err, domain.ErrRoleNotFound)

	// Collection reads narrow to nothing rather than refusing.
	list, err := testStore.ListBundles(ctx, "not-a-uuid")
	require.NoError(t, err)
	require.Empty(t, list)

	list, err = testStore.ListAllBundles(ctx, domain.BundleListFilter{RoleID: "not-a-uuid"})
	require.NoError(t, err)
	require.Empty(t, list)
}

// TestDuplicateRoleCodeIsAConflictNotAnOutage -- and a replay of the same
// correlation id is neither.
func TestDuplicateRoleCodeIsAConflictNotAnOutage(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)
	code := "DUPE_" + uuid.NewString()[:8]
	correlationID := uuid.NewString()

	first := &domain.RoleDefinition{
		RoleDefinitionID: uuid.NewString(), TenantID: tenantA, RoleCode: code,
		RoleName: "first", RoleScopeType: "TENANT", Status: domain.RoleStatusActive,
		CreatedByPrincipalID: "admin-seed", CorrelationID: correlationID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	created, err := testStore.CreateRole(ctx, first, "admin-seed")
	require.NoError(t, err)
	require.True(t, created)

	// A different intent, same code: a conflict the caller can act on.
	taken, err := testStore.RoleCodeTaken(ctx, code, uuid.NewString())
	require.NoError(t, err)
	require.True(t, taken)

	second := *first
	second.RoleDefinitionID = uuid.NewString()
	second.CorrelationID = uuid.NewString()
	_, err = testStore.CreateRole(ctx, &second, "admin-seed")
	require.ErrorIs(t, err, domain.ErrRoleCodeExists,
		"a duplicate role_code surfaced as something other than a conflict")

	// The SAME intent replayed is not a conflict — that is the whole point of
	// an idempotency key, and an existence check without this exclusion turns
	// every retry into a 409.
	taken, err = testStore.RoleCodeTaken(ctx, code, correlationID)
	require.NoError(t, err)
	require.False(t, taken, "a replay of the original create was reported as a duplicate")

	replay := *first
	replay.RoleDefinitionID = uuid.NewString()
	created, err = testStore.CreateRole(ctx, &replay, "admin-seed")
	require.NoError(t, err)
	require.False(t, created, "a replay should resolve to the original, not create")
	require.Equal(t, first.RoleDefinitionID, replay.RoleDefinitionID)
}

// TestOneBundleCodePerRole -- authorization-svc identifies a bundle by
// (role_id, bundle_code) and its attach endpoint is an upsert-REPLACE on that
// pair. Two local rows sharing a code therefore pointed at ONE remote grant:
// creating the second silently replaced the first's actions, and detaching
// either retired the bundle both of them described.
func TestOneBundleCodePerRole(t *testing.T) {
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)
	roleID := seedRole(t, tenantA, "BUNDLEDUPE_"+uuid.NewString()[:8])
	code := "PO_FULL"

	first := &domain.PermissionBundleDef{
		BundleID: uuid.NewString(), TenantID: tenantA, RoleDefinitionID: roleID,
		BundleCode: code, PermittedActions: []string{"PO_ISSUE"}, ActiveFlag: true,
		CorrelationID: uuid.NewString(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	created, err := testStore.CreateBundle(ctx, first, "admin-seed")
	require.NoError(t, err)
	require.True(t, created)

	second := *first
	second.BundleID = uuid.NewString()
	second.CorrelationID = uuid.NewString()
	second.PermittedActions = []string{"PO_CLOSE"}
	_, err = testStore.CreateBundle(ctx, &second, "admin-seed")
	require.ErrorIs(t, err, domain.ErrBundleCodeExists,
		"two bundles with one code on one role: both display ACTIVE here, one grant exists there")

	// A bundle write enqueues BOTH its own event and a role.updated, because
	// what the role grants has changed and role.updated is the only name the
	// session-revoking consumer dispatches on.
	var roleEvents int
	require.NoError(t, ownerPool.QueryRow(ctx, `
		SELECT count(*) FROM event_outbox
		WHERE tenant_id = $1 AND event_type = 'role.updated' AND aggregate_key = $2`,
		tenantA, roleID).Scan(&roleEvents))
	require.Equal(t, 1, roleEvents,
		"a bundle change that emits no role.updated revokes no session — the old grant stays live")
}

// ── helpers ──────────────────────────────────────────────────────────────────

func seedOutbox(t *testing.T, tenantID, eventType string) {
	t.Helper()
	_, err := ownerPool.Exec(context.Background(), `
		INSERT INTO event_outbox (tenant_id, event_type, aggregate_key, payload)
		VALUES ($1, $2, $3, $4)`,
		tenantID, eventType, uuid.NewString(), []byte(`{"event_type":"`+eventType+`"}`))
	require.NoError(t, err)
}

// countOutbox counts what a reader sees after running setup, through the
// NOBYPASSRLS app pool — the only connection on which the policy actually
// binds.
func countOutbox(t *testing.T, setup string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, setup)
	require.NoError(t, err)

	var n int
	require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM event_outbox").Scan(&n))
	return n
}

func pendingCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, ownerPool.QueryRow(context.Background(),
		"SELECT count(*) FROM event_outbox WHERE published_at IS NULL").Scan(&n))
	return n
}
