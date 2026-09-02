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

	"zoiko.io/kill-switch-registry-svc/internal/domain"
	svcmiddleware "zoiko.io/kill-switch-registry-svc/internal/middleware"
	"zoiko.io/kill-switch-registry-svc/internal/store"
)

// Row-level security tests for kill-switch-registry-svc (migration 000002,
// tracker row 17).
//
// These run as a purpose-created NOSUPERUSER NOBYPASSRLS role.
// TEST_DATABASE_URL points at `postgres`, a SUPERUSER, and a superuser
// bypasses row-level security unconditionally — FORCE included — so an
// isolation assertion made over that connection would prove nothing.
//
// The most important test in this file is
// TestRLS_PlatformWideSwitchStaysVisibleToTenants, and it asserts the
// OVER-RESTRICTIVE direction. tenant_id IS NULL means "platform-wide kill
// switch", so a policy tightened to a plain tenant equality would hide
// every platform-wide switch from every tenant-scoped resolution —
// ResolveKillSwitch would answer "not engaged" for an action class that has
// been globally stopped. That is a silent safety bypass, not a leak, and it
// is the failure mode a reviewer is most likely to introduce while
// "hardening" this policy.

const (
	tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	tenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func strp(s string) *string { return &s }

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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS kill_switch_events CASCADE;`)

	// Every migration in filename order, never one hardcoded name.
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
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + appRole,
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

// engage appends an ENGAGE event at the given scope. A nil tenantID is a
// platform-wide switch.
func engage(t *testing.T, s *store.PgStore, ctxTenant string, domainName string, tenantID *string) {
	t.Helper()
	ctx := context.Background()
	if ctxTenant != "" {
		ctx = svcmiddleware.WithTenant(ctx, ctxTenant)
	}
	err := s.AppendEvent(ctx, &domain.KillSwitchEvent{
		KillSwitchEventID:          uuid.NewString(),
		Domain:                     strp(domainName),
		TenantID:                   tenantID,
		Action:                     domain.KillSwitchActionEngage,
		Reason:                     "incident for " + domainName,
		ReconciliationProcedureRef: strp("runbook:test"),
		ApprovedByPrincipalID:      "incident-commander-1",
		CreatedAt:                  time.Now().UTC(),
		CreatedByPrincipalID:       "incident-commander-1",
	})
	if err != nil {
		t.Fatalf("append ENGAGE (domain=%s tenant=%v): %v", domainName, tenantID, err)
	}
}

func TestRLS_EnabledAndForced(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	var enabled, forced bool
	if err := admin.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'kill_switch_events'`,
	).Scan(&enabled, &forced); err != nil {
		t.Fatalf("read pg_class: %v", err)
	}
	if !enabled {
		t.Error("migration 000002 must ENABLE row level security")
	}
	if !forced {
		t.Error("migration 000002 must FORCE row level security")
	}
}

// TestRLS_PlatformWideSwitchStaysVisibleToTenants is the OVER-RESTRICTIVE
// negative control, and the most consequential test in this service.
//
// A platform-wide ENGAGE (tenant_id NULL) must block a tenant-scoped
// resolution. If someone "hardens" the policy to a plain tenant equality,
// this test fails — and without it, that change would ship a kill switch
// that reports "not engaged" while engaged.
func TestRLS_PlatformWideSwitchStaysVisibleToTenants(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	// A platform-wide switch, engaged with no tenant scope at all.
	engage(t, s, "", "IMPORT_SYNC", nil)

	// Tenant A asks about its own scope. It must be blocked.
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	res, err := s.ResolveKillSwitch(ctxA, nil, strp("IMPORT_SYNC"), nil, strp(tenantA))
	if err != nil {
		t.Fatalf("resolve as tenant A: %v", err)
	}
	if !res.Blocked {
		t.Fatal("SAFETY BYPASS: a platform-wide kill switch did not block a tenant-scoped resolution — " +
			"the policy is hiding tenant_id IS NULL rows, so an engaged emergency stop reads as not engaged")
	}

	// And tenant B likewise — a platform-wide switch is not one tenant's.
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	resB, err := s.ResolveKillSwitch(ctxB, nil, strp("IMPORT_SYNC"), nil, strp(tenantB))
	if err != nil {
		t.Fatalf("resolve as tenant B: %v", err)
	}
	if !resB.Blocked {
		t.Fatal("SAFETY BYPASS: the platform-wide switch did not block tenant B either")
	}
}

// TestRLS_PlatformLevelResolutionSeesOnlyPlatformSwitches covers the
// tenant-less caller. Under this policy "" matches no tenant-specific row
// but still matches every IS NULL row — which is the correct answer to the
// platform-level question, and why the middleware here does not refuse
// tenant-less requests the way evidence-manifest-svc's does.
func TestRLS_PlatformLevelResolutionSeesOnlyPlatformSwitches(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	// Only a TENANT-SPECIFIC switch exists.
	engage(t, s, tenantA, "COMMERCIAL_CHARGING", strp(tenantA))

	// A platform-level resolution must NOT be blocked by one tenant's switch.
	res, err := s.ResolveKillSwitch(context.Background(), nil, strp("COMMERCIAL_CHARGING"), nil, nil)
	if err != nil {
		t.Fatalf("platform-level resolve: %v", err)
	}
	if res.Blocked {
		t.Fatalf("one tenant's kill switch must not block the platform-level question, got %+v", res.MatchedEvent)
	}
}

// TestRLS_TenantCannotSeeAnotherTenantsSwitch is the ordinary isolation
// direction. During an incident, another tenant's switch state and reason
// text is a live feed of that customer's operational trouble.
func TestRLS_TenantCannotSeeAnotherTenantsSwitch(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	engage(t, s, tenantA, "AUTOMATION_ACTION", strp(tenantA))

	// Tenant B resolving ITS OWN scope must not be blocked by tenant A's switch.
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	res, err := s.ResolveKillSwitch(ctxB, nil, strp("AUTOMATION_ACTION"), nil, strp(tenantB))
	if err != nil {
		t.Fatalf("resolve as tenant B: %v", err)
	}
	if res.Blocked {
		t.Fatalf("ISOLATION FAILURE: tenant A's switch blocked tenant B: %+v", res.MatchedEvent)
	}

	// Tenant B must not read tenant A's history either.
	hist, err := s.ListHistoryForScope(ctxB, nil, strp("AUTOMATION_ACTION"), nil, strp(tenantA))
	if err != nil {
		t.Fatalf("list history as tenant B: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("ISOLATION FAILURE: tenant B read %d of tenant A's kill switch events (reason=%q)",
			len(hist), hist[0].Reason)
	}

	// Sanity: tenant A still sees its own.
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ownHist, err := s.ListHistoryForScope(ctxA, nil, strp("AUTOMATION_ACTION"), nil, strp(tenantA))
	if err != nil {
		t.Fatalf("list history as tenant A: %v", err)
	}
	if len(ownHist) != 1 {
		t.Fatalf("tenant A must see its own 1 event, got %d", len(ownHist))
	}
}

// TestRLS_ListCurrentStates_ScopedToCaller covers the operations view. It
// took no parameters and had no authz, so it returned EVERY tenant's
// current kill-switch state — a full cross-tenant incident map. It is now
// bounded by the policy: the caller's own rows plus platform-wide ones.
func TestRLS_ListCurrentStates_ScopedToCaller(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	engage(t, s, tenantA, "COMMERCIAL_CHARGING", strp(tenantA))
	engage(t, s, tenantB, "NOTIFICATION_EXPORT", strp(tenantB))
	engage(t, s, "", "IMPORT_SYNC", nil)

	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	states, err := s.ListCurrentStates(ctxA)
	if err != nil {
		t.Fatalf("list current states as tenant A: %v", err)
	}
	for _, st := range states {
		if st.TenantID != nil && *st.TenantID != tenantA {
			t.Fatalf("ISOLATION FAILURE: tenant A's operations view included tenant %q's switch (domain=%v)",
				*st.TenantID, st.Domain)
		}
	}
	// It must still include the platform-wide switch — that is the whole
	// point of the IS NULL branch, and an operations view that hides
	// platform-wide switches is the same safety bypass in a different place.
	var sawPlatformWide bool
	for _, st := range states {
		if st.TenantID == nil {
			sawPlatformWide = true
		}
	}
	if !sawPlatformWide {
		t.Fatal("tenant A's operations view must still show platform-wide switches")
	}
}

// TestRLS_WithCheckRefusesForeignTenantWrite covers the write side for a
// TENANT-SCOPED event.
//
// Note deliberately what this does NOT assert: a platform-wide (tenant_id
// NULL) insert is permitted by the policy for any caller, because WITH
// CHECK must keep the IS NULL branch. The control on that path is the
// handler's authorization at platform scope, not RLS — stated in migration
// 000002's header and repeated here so a reader of the tests does not infer
// a guarantee that is not there.
func TestRLS_WithCheckRefusesForeignTenantWrite(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)

	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenantB); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO kill_switch_events
			(kill_switch_event_id, domain, tenant_id, action, reason,
			 reconciliation_procedure_ref, approved_by_principal_id, created_by_principal_id)
		VALUES ($1, 'COMMERCIAL_CHARGING', $2, 'ENGAGE', 'forged',
			'runbook:forged', 'forger', 'forger')`,
		uuid.NewString(), tenantA)
	if err == nil {
		t.Fatal("WITH CHECK must refuse a kill switch event attributed to another tenant — " +
			"engaging another tenant's COMMERCIAL_CHARGING switch stops their billing")
	}
}
