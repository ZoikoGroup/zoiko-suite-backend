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

	"zoiko.io/retention-registry-svc/internal/domain"
	svcmiddleware "zoiko.io/retention-registry-svc/internal/middleware"
	"zoiko.io/retention-registry-svc/internal/store"
)

// Row-level security tests for retention-registry-svc (migration 000002,
// tracker row 18).
//
// These run as a purpose-created NOSUPERUSER NOBYPASSRLS role.
// TEST_DATABASE_URL points at `postgres`, a SUPERUSER, and a superuser
// bypasses row-level security unconditionally — FORCE included — so an
// isolation assertion made over that connection would prove nothing.
//
// The most important tests here assert the OVER-RESTRICTIVE direction.
// tenant_id IS NULL means "applies platform-wide", and this service answers
// "is it safe to delete/export/migrate this?" for every other service. A
// policy tightened to plain tenant equality hides platform-wide retention
// rules and platform-wide legal holds from every tenant-scoped caller, so
// Resolve reports "no policy, no hold" and the deletion proceeds. Unlike a
// missed kill switch, that outcome cannot be undone.

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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS legal_holds, retention_policies CASCADE;`)

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

func ctxFor(tenantID string) context.Context {
	if tenantID == "" {
		return context.Background()
	}
	return svcmiddleware.WithTenant(context.Background(), tenantID)
}

// addPolicy creates a retention policy. A nil tenantID is platform-wide.
func addPolicy(t *testing.T, s *store.PgStore, ctxTenant, recordClass string, tenantID *string, minDays int) {
	t.Helper()
	err := s.CreateRetentionPolicy(ctxFor(ctxTenant), &domain.RetentionPolicy{
		RetentionPolicyID:    uuid.NewString(),
		RecordClass:          recordClass,
		TenantID:             tenantID,
		MinRetentionDays:     minDays,
		LegalRegulatoryBasis: "test basis",
		PolicyStatus:         "ACTIVE",
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "principal-1",
	})
	if err != nil {
		t.Fatalf("create retention policy (class=%s tenant=%v): %v", recordClass, tenantID, err)
	}
}

// addHold creates an ACTIVE legal hold. A nil tenantID is platform-wide.
func addHold(t *testing.T, s *store.PgStore, ctxTenant, recordClass string, tenantID *string) string {
	t.Helper()
	id := uuid.NewString()
	err := s.CreateLegalHold(ctxFor(ctxTenant), &domain.LegalHold{
		LegalHoldID:          id,
		ScopeDescription:     "litigation hold for " + recordClass,
		CustodiansObjects:    []string{"custodian-1"},
		Authority:            "court order 2026-CV-1234",
		RecordClass:          strp(recordClass),
		TenantID:             tenantID,
		HoldStatus:           "ACTIVE",
		StartedAt:            time.Now().UTC(),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "principal-1",
	})
	if err != nil {
		t.Fatalf("create legal hold (class=%s tenant=%v): %v", recordClass, tenantID, err)
	}
	return id
}

func TestRLS_EnabledAndForced(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	for _, table := range []string{"retention_policies", "legal_holds"} {
		var enabled, forced bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read pg_class for %s: %v", table, err)
		}
		if !enabled {
			t.Errorf("%s: migration 000002 must ENABLE row level security", table)
		}
		if !forced {
			t.Errorf("%s: migration 000002 must FORCE row level security", table)
		}
	}
}

// TestRLS_PlatformWideHoldStaysVisibleToTenants is the most consequential
// test in this service. A platform-wide legal hold must block a
// tenant-scoped resolve. If the policy is tightened to plain tenant
// equality, this fails — and without it, that change would ship a deletion
// path that ignores a legal hold.
func TestRLS_PlatformWideHoldStaysVisibleToTenants(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	// A platform-wide hold, created with no tenant scope at all.
	addHold(t, s, "", "FINANCIAL_LEDGER", nil)

	res, err := s.Resolve(ctxFor(tenantA), "FINANCIAL_LEDGER", nil, strp(tenantA), nil)
	if err != nil {
		t.Fatalf("resolve as tenant A: %v", err)
	}
	if !res.Blocked {
		t.Fatal("SPOLIATION RISK: a platform-wide legal hold did not block a tenant-scoped resolve — " +
			"the policy is hiding tenant_id IS NULL rows, so the deletion path would proceed on " +
			"records under a legal preservation obligation, irreversibly")
	}

	// Tenant B likewise — a platform-wide hold is not one tenant's.
	resB, err := s.Resolve(ctxFor(tenantB), "FINANCIAL_LEDGER", nil, strp(tenantB), nil)
	if err != nil {
		t.Fatalf("resolve as tenant B: %v", err)
	}
	if !resB.Blocked {
		t.Fatal("SPOLIATION RISK: the platform-wide hold did not block tenant B either")
	}
}

// TestRLS_PlatformWideRetentionPolicyStaysVisibleToTenants is the same
// over-restrictive control for the other table. A hidden platform-wide
// minimum-retention rule means Resolve reports no applicable policy, and
// doc7 §J2 forbids automatic destructive deletion of governed records.
func TestRLS_PlatformWideRetentionPolicyStaysVisibleToTenants(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	addPolicy(t, s, "", "OBLIGATION_EVIDENCE", nil, 2555)

	res, err := s.Resolve(ctxFor(tenantA), "OBLIGATION_EVIDENCE", nil, strp(tenantA), nil)
	if err != nil {
		t.Fatalf("resolve as tenant A: %v", err)
	}
	if res.ApplicablePolicy == nil {
		t.Fatal("a platform-wide retention policy must apply to a tenant-scoped resolve — " +
			"hidden, the caller concludes it may delete a governed record")
	}
	if res.ApplicablePolicy.MinRetentionDays != 2555 {
		t.Fatalf("wrong policy resolved: %+v", res.ApplicablePolicy)
	}
}

// TestRLS_MostSpecificPolicyStillWins guards against the opposite mistake:
// the IS NULL branch must ADD platform-wide rows, not override a tenant's
// own more-specific rule.
func TestRLS_MostSpecificPolicyStillWins(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	addPolicy(t, s, "", "COMMERCIAL_SUBSCRIPTION_HISTORY", nil, 365)
	addPolicy(t, s, tenantA, "COMMERCIAL_SUBSCRIPTION_HISTORY", strp(tenantA), 3650)

	res, err := s.Resolve(ctxFor(tenantA), "COMMERCIAL_SUBSCRIPTION_HISTORY", nil, strp(tenantA), nil)
	if err != nil {
		t.Fatalf("resolve as tenant A: %v", err)
	}
	if res.ApplicablePolicy == nil || res.ApplicablePolicy.MinRetentionDays != 3650 {
		t.Fatalf("tenant A's own more-specific policy must win over the platform-wide one, got %+v",
			res.ApplicablePolicy)
	}

	// And tenant B, with no policy of its own, still gets the platform one.
	resB, err := s.Resolve(ctxFor(tenantB), "COMMERCIAL_SUBSCRIPTION_HISTORY", nil, strp(tenantB), nil)
	if err != nil {
		t.Fatalf("resolve as tenant B: %v", err)
	}
	if resB.ApplicablePolicy == nil || resB.ApplicablePolicy.MinRetentionDays != 365 {
		t.Fatalf("tenant B must fall back to the platform-wide policy, got %+v", resB.ApplicablePolicy)
	}
}

// TestRLS_TenantCannotSeeAnotherTenantsHold is the ordinary isolation
// direction. A legal hold is evidence that a tenant is under litigation or
// regulatory investigation, and its scope_description and authority name
// the matter.
func TestRLS_TenantCannotSeeAnotherTenantsHold(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	holdA := addHold(t, s, tenantA, "FINANCIAL_LEDGER", strp(tenantA))

	if got, err := s.FindLegalHoldByID(ctxFor(tenantB), holdA, tenantB); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's legal hold: authority=%q scope=%q",
			got.Authority, got.ScopeDescription)
	}

	// Tenant B's own resolve must not be blocked by tenant A's hold.
	res, err := s.Resolve(ctxFor(tenantB), "FINANCIAL_LEDGER", nil, strp(tenantB), nil)
	if err != nil {
		t.Fatalf("resolve as tenant B: %v", err)
	}
	if res.Blocked {
		t.Fatalf("ISOLATION FAILURE: tenant A's hold blocked tenant B: %+v", res.MatchedHold)
	}

	// Sanity: tenant A still reads its own and is still blocked by it.
	own, err := s.FindLegalHoldByID(ctxFor(tenantA), holdA, tenantA)
	if err != nil {
		t.Fatalf("tenant A must still read its own hold: %v", err)
	}
	if own.HoldStatus != "ACTIVE" {
		t.Fatalf("unexpected hold state for tenant A: %+v", own)
	}
	resA, err := s.Resolve(ctxFor(tenantA), "FINANCIAL_LEDGER", nil, strp(tenantA), nil)
	if err != nil {
		t.Fatalf("resolve as tenant A: %v", err)
	}
	if !resA.Blocked {
		t.Fatal("tenant A's own hold must block tenant A's resolve")
	}
}

// TestRLS_ReleasingAnotherTenantsHold_Refused covers the most legally
// consequential write. Releasing a hold un-blocks deletion of records under
// a preservation obligation.
//
// Note this was NOT an open door before the change: ReleaseLegalHold's
// handler fetches the hold first and authorizes against that hold's own
// tenant, and authorization-svc defaults to DENIED. The store-level UPDATE
// was unscoped, which is a defence-in-depth gap — the store is callable
// in-process and had no policy beneath it. This asserts the boundary now
// holds at the store too, so the guarantee does not depend on handler
// ordering.
func TestRLS_ReleasingAnotherTenantsHold_Refused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	holdA := addHold(t, s, tenantA, "FINANCIAL_LEDGER", strp(tenantA))

	if _, err := s.ReleaseLegalHold(ctxFor(tenantB), holdA, tenantB, "principal-b", "approver-b"); err == nil {
		t.Fatal("tenant B must not be able to release tenant A's legal hold")
	}

	// And it must still be ACTIVE — the refusal has to mean "nothing
	// happened", not just "the response said no".
	still, err := s.FindLegalHoldByID(ctxFor(tenantA), holdA, tenantA)
	if err != nil {
		t.Fatalf("read tenant A's hold: %v", err)
	}
	if still.HoldStatus != "ACTIVE" {
		t.Fatalf("tenant B's refused release still changed tenant A's hold to %s", still.HoldStatus)
	}
	if still.ReleasedAt != nil {
		t.Fatalf("tenant A's hold has a released_at set after a refused release: %+v", still.ReleasedAt)
	}
}

// TestRLS_PlatformLevelResolveIgnoresTenantSpecificRules covers the
// tenant-less caller: "" matches no tenant-specific row but still matches
// every IS NULL row, which is the correct answer to the platform-level
// question and why this service's middleware stays permissive.
func TestRLS_PlatformLevelResolveIgnoresTenantSpecificRules(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	addHold(t, s, tenantA, "IMPORT_SYNC", strp(tenantA))

	res, err := s.Resolve(context.Background(), "IMPORT_SYNC", nil, nil, nil)
	if err != nil {
		t.Fatalf("platform-level resolve: %v", err)
	}
	if res.Blocked {
		t.Fatalf("one tenant's hold must not block the platform-level question, got %+v", res.MatchedHold)
	}
}

// TestRLS_WithCheckRefusesForeignTenantWrite covers the write side for a
// TENANT-SCOPED row.
//
// Deliberately not asserted: a platform-wide (tenant_id NULL) insert is
// permitted for any caller, because WITH CHECK must keep the IS NULL
// branch. For THIS service that is the safe direction — an unauthorized
// extra hold blocks a deletion, it never permits one — and creating one
// still requires a platform-scope grant at the handler. See migration
// 000002's header.
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
		INSERT INTO legal_holds
			(legal_hold_id, scope_description, custodians_objects, authority,
			 record_class, tenant_id, created_by_principal_id)
		VALUES ($1, 'forged hold', '[]'::jsonb, 'forged authority',
			'FINANCIAL_LEDGER', $2, 'forger')`,
		uuid.NewString(), tenantA)
	if err == nil {
		t.Fatal("WITH CHECK must refuse a legal hold attributed to another tenant")
	}
}
