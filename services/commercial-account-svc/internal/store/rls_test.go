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

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// Row-level security tests for commercial-account-svc (migration 000005,
// tracker row 11b).
//
// These run as a purpose-created NOSUPERUSER NOBYPASSRLS role.
// TEST_DATABASE_URL points at `postgres`, a SUPERUSER, and a superuser
// bypasses row-level security unconditionally — FORCE included — so an
// isolation assertion made over that connection would prove nothing about
// the policy. That is not a hypothetical here: this service's store package
// previously justified having NO RLS on exactly that observation, having
// drawn the conclusion from the test DSN rather than from production, where
// the runtime role is a non-owner.

const (
	orgA = "11111111-1111-1111-1111-111111111111"
	orgB = "22222222-2222-2222-2222-222222222222"
)

func openAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `
		DROP TABLE IF EXISTS outbox_events, subscription_status_events, billing_source_transfers,
			subscription_change_requests, commercial_usage_meter_events,
			contract_entitlement_overlays, evaluation_programs, commercial_subscriptions,
			entitlement_limits, plans, price_catalogs, memberships, commercial_accounts CASCADE;`)

	// Every migration in filename order, never one hardcoded name — a suite
	// that names migrations individually silently skips new ones, which
	// would leave the migration under test unapplied and the run green for
	// the wrong reason.
	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var migs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			migs = append(migs, e.Name())
		}
	}
	if len(migs) == 0 {
		t.Fatalf("no migrations found in %s", migDir)
	}
	sort.Strings(migs)
	for _, name := range migs {
		sqlBytes, err := os.ReadFile(filepath.Join(migDir, name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	return pool
}

func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	const appRole = "zoiko_app_test"
	const appPassword = "zoiko_app_test_pw"

	if _, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+appRole+`') THEN
			CREATE ROLE `+appRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, stmt := range []string{
		`ALTER ROLE ` + appRole + ` WITH LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + appRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appRole,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant (%s): %v", stmt, err)
		}
	}

	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.User = url.UserPassword(appRole, appPassword)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", appRole, err)
	}
	t.Cleanup(pool.Close)

	var isSuper, bypassRLS bool
	if err := pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &bypassRLS); err != nil {
		t.Fatalf("verify privileges: %v", err)
	}
	if isSuper || bypassRLS {
		t.Fatalf("%s must be NOSUPERUSER and NOBYPASSRLS, got rolsuper=%v rolbypassrls=%v", appRole, isSuper, bypassRLS)
	}
	return pool
}

// seedAccountAndSubscription creates one organization's account plus a
// subscription and one status event, so the derived-table policies have
// something to isolate. Catalog rows are shared: they are platform scope.
func seedAccountAndSubscription(t *testing.T, s *store.PgStore, admin *pgxpool.Pool, organizationID string) (accountID, subscriptionID string) {
	t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), organizationID)

	acct := &domain.CommercialAccount{
		CommercialAccountID:  uuid.NewString(),
		OrganizationID:       organizationID,
		LegalCustomerName:    "Customer " + organizationID[:8],
		BillingCurrencyCode:  "USD",
		ContactEmail:         "billing@" + organizationID[:8] + ".example",
		Status:               domain.CommercialAccountStatusActive,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "principal-seed",
	}
	if err := s.CreateCommercialAccount(ctx, acct); err != nil {
		t.Fatalf("seed account for %s: %v", organizationID, err)
	}

	// Catalog + plan are platform scope (no policy), so seed them directly
	// and share them across both organizations — which is exactly the point
	// of them having no tenant column.
	catalogID := uuid.NewString()
	planID := uuid.NewString()
	if _, err := admin.Exec(context.Background(), `
		INSERT INTO price_catalogs (catalog_version_id, catalog_code, status, effective_from, created_by_principal_id)
		VALUES ($1, $2, 'APPROVED', NOW(), 'principal-seed')`, catalogID, "cat-"+catalogID[:8]); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if _, err := admin.Exec(context.Background(), `
		INSERT INTO plans (plan_id, catalog_version_id, plan_code, display_name, billing_interval,
			base_price_amount, base_price_currency_code, created_by_principal_id)
		VALUES ($1, $2, $3, 'Standard', 'MONTHLY', 100.00, 'USD', 'principal-seed')`,
		planID, catalogID, "plan-"+planID[:8]); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	sub := &domain.CommercialSubscription{
		SubscriptionID:       uuid.NewString(),
		CommercialAccountID:  acct.CommercialAccountID,
		PlanID:               planID,
		CatalogVersionID:     catalogID,
		BillingInterval:      "MONTHLY",
		Status:               domain.SubscriptionStatusActive,
		BillingSource:        domain.BillingSourceDirect,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "principal-seed",
	}
	if err := s.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("seed subscription for %s: %v", organizationID, err)
	}
	return acct.CommercialAccountID, sub.SubscriptionID
}

func TestRLS_EnabledAndForced(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	for _, table := range []string{
		// direct
		"commercial_accounts", "memberships",
		// derived via commercial_account_id
		"commercial_subscriptions", "contract_entitlement_overlays", "billing_source_transfers",
		// derived via subscription_id
		"evaluation_programs", "commercial_usage_meter_events",
		"subscription_change_requests", "subscription_status_events",
	} {
		var enabled, forced bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read pg_class for %s: %v", table, err)
		}
		if !enabled {
			t.Errorf("%s: migration 000005 must ENABLE row level security", table)
		}
		if !forced {
			t.Errorf("%s: migration 000005 must FORCE row level security", table)
		}
	}
}

// TestRLS_PlatformTablesHaveNoPolicy pins a deliberate asymmetry.
//
// price_catalogs, plans and entitlement_limits have no tenant column and
// must not get one: doc7 §U1 makes the published catalog platform scope —
// every tenant reads the same approved version. Enabling RLS on them would
// break catalog reads for everybody, and "make the schema consistent" is
// exactly the well-meant change that would do it. This test is here so that
// change has to argue with a failing build.
func TestRLS_PlatformTablesHaveNoPolicy(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	for _, table := range []string{"price_catalogs", "plans", "entitlement_limits"} {
		var enabled bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled); err != nil {
			t.Fatalf("read pg_class for %s: %v", table, err)
		}
		if enabled {
			t.Errorf("%s is platform scope (doc7 §U1) and must NOT have RLS enabled — "+
				"every tenant reads the same approved catalog, so a tenant policy would hide it from all of them", table)
		}
		var policies int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM pg_policies WHERE tablename = $1`, table,
		).Scan(&policies); err != nil {
			t.Fatalf("read pg_policies for %s: %v", table, err)
		}
		if policies != 0 {
			t.Errorf("%s must have no policy, found %d", table, policies)
		}
	}
}

// TestRLS_DirectTables_TenantIsolation covers the two tables that carry
// organization_id themselves.
func TestRLS_DirectTables_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	accountA, _ := seedAccountAndSubscription(t, s, admin, orgA)
	_, _ = seedAccountAndSubscription(t, s, admin, orgB)

	ctxB := svcmiddleware.WithTenant(context.Background(), orgB)
	if got, err := s.GetCommercialAccount(ctxB, accountA); err == nil {
		t.Fatalf("ISOLATION FAILURE: org B read org A's commercial account: %+v", got)
	}

	ctxA := svcmiddleware.WithTenant(context.Background(), orgA)
	own, err := s.GetCommercialAccount(ctxA, accountA)
	if err != nil {
		t.Fatalf("org A must still read its own account: %v", err)
	}
	if own.OrganizationID != orgA {
		t.Fatalf("unexpected account returned for org A: %+v", own)
	}
}

// TestRLS_DerivedTables_TenantIsolation is the interesting half: neither
// commercial_subscriptions (one FK hop from the organization) nor
// subscription_status_events (two hops) has a tenant column, so their
// isolation rests entirely on the subquery policies resolving through the
// parent chain.
func TestRLS_DerivedTables_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	_, subA := seedAccountAndSubscription(t, s, admin, orgA)
	_, subB := seedAccountAndSubscription(t, s, admin, orgB)

	ctxA := svcmiddleware.WithTenant(context.Background(), orgA)
	ctxB := svcmiddleware.WithTenant(context.Background(), orgB)

	// Level 2: commercial_subscriptions, via commercial_account_id.
	if got, err := s.GetSubscription(ctxB, subA); err == nil {
		t.Fatalf("ISOLATION FAILURE: org B read org A's subscription: %+v", got)
	}
	if _, err := s.GetSubscription(ctxA, subA); err != nil {
		t.Fatalf("org A must still read its own subscription: %v", err)
	}

	// Level 3: subscription_status_events, via subscription_id →
	// commercial_subscriptions → commercial_accounts. Give org A an event to
	// find by transitioning its subscription.
	reason := "dunning escalation"
	if err := s.TransitionSubscriptionStatus(ctxA, subA, domain.SubscriptionStatusPastDue,
		[]domain.SubscriptionStatus{domain.SubscriptionStatusActive}, &reason, "principal-a"); err != nil {
		t.Fatalf("transition org A's subscription: %v", err)
	}

	eventsA, err := s.ListStatusEventsBySubscription(ctxA, subA)
	if err != nil {
		t.Fatalf("org A list its own status events: %v", err)
	}
	if len(eventsA) == 0 {
		t.Fatal("org A must see its own status event — the two-level policy chain has hidden a row it owns")
	}

	// Org B asking for org A's subscription id must get nothing.
	eventsB, err := s.ListStatusEventsBySubscription(ctxB, subA)
	if err != nil {
		t.Fatalf("org B list status events: %v", err)
	}
	if len(eventsB) != 0 {
		t.Fatalf("ISOLATION FAILURE: org B read %d of org A's status events: %+v", len(eventsB), eventsB)
	}

	// And org B's own subscription must still work, so the above is
	// isolation and not simply a broken query.
	if _, err := s.GetSubscription(ctxB, subB); err != nil {
		t.Fatalf("org B must still read its own subscription: %v", err)
	}
}

// TestRLS_TenantlessContext_SeesNothing is the test row 11a provably could
// not write.
//
// 11a added a handler guard that refuses a request with no X-Tenant-Id, and
// that guard makes the store's own tenant-less behaviour unreachable over
// HTTP — so no handler test can demonstrate it. The store is still callable
// in-process with a tenant-less context, which is where the old
// self-disabling predicate did its damage: it matched when the tenant
// parameter was the empty string OR equalled the column, so an absent
// tenant satisfied the first branch and the boundary vanished. (Spelling
// that out in words rather than SQL on purpose — gofmt's doc-comment
// formatter rewrites a doubled single-quote into a curly quote and mangles
// the snippet.) This asserts the store fails closed on its own.
func TestRLS_TenantlessContext_SeesNothing(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	accountA, subA := seedAccountAndSubscription(t, s, admin, orgA)

	// A bare context: no tenant, exactly what TenantFromContext returns ""
	// for. NULLIF makes "" match no organization_id.
	bare := context.Background()

	if got, err := s.GetCommercialAccount(bare, accountA); err == nil {
		t.Fatalf("FAIL-OPEN: a tenant-less context read an account: %+v", got)
	}
	if got, err := s.GetSubscription(bare, subA); err == nil {
		t.Fatalf("FAIL-OPEN: a tenant-less context read a subscription: %+v", got)
	}
	list, err := s.ListMemberships(bare)
	if err != nil {
		t.Fatalf("tenant-less ListMemberships should return empty, not error: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("FAIL-OPEN: a tenant-less context listed %d memberships", len(list))
	}
}

// TestRLS_WithCheckRefusesForeignTenantWrite covers the write side on a
// direct table: USING alone governs visibility, so without WITH CHECK a
// caller could insert a row attributed to another organization that it then
// cannot read back.
func TestRLS_WithCheckRefusesForeignTenantWrite(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)

	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", orgB); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO commercial_accounts
			(commercial_account_id, organization_id, legal_customer_name, billing_currency_code,
			 contact_email, status, created_by_principal_id)
		VALUES ($1, $2, 'Forged Co', 'USD', 'x@example.com', 'ACTIVE', 'principal-forger')`,
		uuid.NewString(), orgA)
	if err == nil {
		t.Fatal("WITH CHECK must refuse an insert attributed to another organization")
	}
}

// TestRLS_DerivedWithCheckRefusesForeignParent is the write side of the
// subquery policy: an insert naming another organization's
// commercial_account_id must be refused, not merely invisible afterwards.
func TestRLS_DerivedWithCheckRefusesForeignParent(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	accountA, _ := seedAccountAndSubscription(t, s, admin, orgA)
	_, _ = seedAccountAndSubscription(t, s, admin, orgB)

	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", orgB); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO contract_entitlement_overlays
			(overlay_id, commercial_account_id, metric_type, override_limit_value,
			 effective_from, created_by_principal_id)
		VALUES ($1, $2, 'SEATS', 999999, NOW(), 'principal-forger')`,
		uuid.NewString(), accountA)
	if err == nil {
		t.Fatal("WITH CHECK must refuse an overlay attached to another organization's account — " +
			"an entitlement overlay raises a tenant's limits, so forging one is a billing-integrity issue")
	}
}

// TestRLS_ParentPolicyCoupling pins how the seven derived tables depend on
// commercial_accounts' policy — and it corrects a wrong prediction.
//
// I expected dropping the parent POLICY to widen the children, on the
// reasoning that their subqueries would then match rows they previously
// could not see. That is wrong, and this test is what caught it: Postgres
// treats "RLS enabled with no policy applicable" as DENY ALL, so dropping
// the parent policy makes commercial_accounts invisible, the subquery
// returns the empty set, and the children become MORE restrictive, not
// less. Fail-closed.
//
// The actual widening path is DISABLE ROW LEVEL SECURITY on the parent —
// then the subquery sees every account and all seven derived tables open up
// at once. Both directions are asserted below, because the distinction is
// the whole point: one way of "removing the parent policy" is safe and the
// other is a seven-table breach, and they look equally innocuous in a diff.
func TestRLS_ParentPolicyCoupling(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	_, subA := seedAccountAndSubscription(t, s, admin, orgA)
	_, _ = seedAccountAndSubscription(t, s, admin, orgB)

	ctxA := svcmiddleware.WithTenant(context.Background(), orgA)
	ctxB := svcmiddleware.WithTenant(context.Background(), orgB)

	// Baseline: the boundary holds, and org A can see its own row.
	if _, err := s.GetSubscription(ctxB, subA); err == nil {
		t.Fatal("precondition failed: org B could already read org A's subscription")
	}
	if _, err := s.GetSubscription(ctxA, subA); err != nil {
		t.Fatalf("precondition failed: org A cannot read its own subscription: %v", err)
	}

	restorePolicy := func() {
		_, _ = admin.Exec(ctx, `
			CREATE POLICY tenant_isolation_policy ON commercial_accounts
				FOR ALL
				USING (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
				WITH CHECK (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))`)
	}

	t.Run("dropping the parent policy fails CLOSED", func(t *testing.T) {
		if _, err := admin.Exec(ctx, `DROP POLICY tenant_isolation_policy ON commercial_accounts`); err != nil {
			t.Fatalf("drop parent policy: %v", err)
		}
		defer restorePolicy()

		// RLS is still ENABLED on the parent with no policy left, which
		// Postgres reads as deny-all — so even the OWNING organization
		// loses its child rows.
		if got, err := s.GetSubscription(ctxA, subA); err == nil {
			t.Fatalf("expected deny-all with the parent policy dropped, but org A still read %+v", got)
		}
		if got, err := s.GetSubscription(ctxB, subA); err == nil {
			t.Fatalf("ISOLATION FAILURE: org B read org A's subscription with the parent policy dropped: %+v", got)
		}
	})

	t.Run("disabling parent RLS widens all derived tables", func(t *testing.T) {
		if _, err := admin.Exec(ctx, `ALTER TABLE commercial_accounts DISABLE ROW LEVEL SECURITY`); err != nil {
			t.Fatalf("disable parent RLS: %v", err)
		}
		defer func() {
			_, _ = admin.Exec(ctx, `ALTER TABLE commercial_accounts ENABLE ROW LEVEL SECURITY`)
			_, _ = admin.Exec(ctx, `ALTER TABLE commercial_accounts FORCE ROW LEVEL SECURITY`)
		}()

		got, err := s.GetSubscription(ctxB, subA)
		if err != nil {
			t.Fatalf("expected the child read to widen once parent RLS was disabled, got: %v", err)
		}
		t.Logf("confirmed the real coupling: disabling RLS on commercial_accounts alone exposed "+
			"org A's subscription %s to org B — one ALTER TABLE widens all seven derived tables, "+
			"because none of them carries a tenant column of its own", got.SubscriptionID)
	})

	// Restored state must isolate again, so a failure above cannot leave a
	// later test passing against a weakened schema.
	if _, err := s.GetSubscription(ctxB, subA); err == nil {
		t.Fatal("cleanup failed: org B can still read org A's subscription after restore")
	}
}
