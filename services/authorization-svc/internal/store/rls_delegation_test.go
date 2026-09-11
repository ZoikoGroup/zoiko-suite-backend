package store_test

// Row-level-security tests for the evaluation path, run as an ORDINARY
// database role.
//
// ── WHY A SEPARATE ROLE, AND WHY THIS FILE EXISTS ───────────────────────────
//
// Every other test in this package connects with TEST_DATABASE_URL, which is
// the migration user — on the local stack that is `postgres`, a SUPERUSER.
// Postgres exempts superusers and BYPASSRLS holders from the row security
// system entirely, so a query that never installs app.tenant_id still returns
// every row, and a test asserting it returned the right rows passes.
//
// That is exactly how PgStore.FindDelegatedActions shipped reading
// delegated_authorities on the bare pool — outside both withRLS and
// withPlatformScope — after 000006 had given that table a policy with no
// platform-scope hatch. Under any role the policy actually binds (compose:
// zoiko_app, NOSUPERUSER NOBYPASSRLS; Supabase: app_authorization) the query
// matched ZERO rows, so layer 2 of /v1/authorize granted nothing at all, on
// every request, silently — a delegate was simply denied `no_grant`,
// indistinguishable from having no delegation. TestPgStore_
// FindDelegatedActions_ResolvesViaDelegator passed throughout.
//
// So these tests create a purpose-built NOSUPERUSER NOBYPASSRLS role and run
// through it. A test that only uses the migration connection proves nothing
// about row security, which is the lesson 000005's own comment already
// recorded and no test had yet acted on.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/store"
)

const (
	rlsRoleName     = "authz_rls_test_role"
	rlsRolePassword = "authz_rls_test_password"
)

// getRLSPool returns a pool connected as a NOSUPERUSER NOBYPASSRLS role with
// DML on this schema and nothing more.
//
// Called AFTER setupTestDB, always: it grants on the tables that exist at the
// time it runs, and setupTestDB drops and recreates them. Granting first would
// leave the role with privileges on tables that no longer exist and none on
// the ones the test uses — which surfaces as "permission denied", not as the
// RLS behaviour under test.
func getRLSPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	if _, err := admin.Exec(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+rlsRoleName+`') THEN
				CREATE ROLE `+rlsRoleName+` LOGIN PASSWORD '`+rlsRolePassword+`'
					NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
			ELSE
				ALTER ROLE `+rlsRoleName+` LOGIN PASSWORD '`+rlsRolePassword+`'
					NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
			END IF;
		END
		$$;`); err != nil {
		t.Skipf("cannot create an ordinary role on this instance (%v) — skipping RLS tests", err)
	}

	// Parent-table grants only, deliberately: access_decision_log is
	// partitioned since 000009, and this asserts in passing what
	// create-app-roles.sh relies on — Postgres checks privileges on the
	// partitioned parent when a row is routed through it, so the app role
	// needs no grant on each month's partition.
	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA public TO ` + rlsRoleName,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + rlsRoleName,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + rlsRoleName,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant to rls role (%s): %v", stmt, err)
		}
	}

	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.User = rlsRoleName
	cfg.ConnConfig.Password = rlsRolePassword
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", rlsRoleName, err)
	}

	// Proves the role really is bound by row security. Without this a
	// misconfigured instance (the role created as BYPASSRLS, or the DSN
	// silently falling back to the superuser) would make every assertion
	// below vacuously pass — which is the whole failure mode this file exists
	// to stop repeating.
	var isSuper, bypass bool
	if err := pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&isSuper, &bypass); err != nil {
		t.Fatalf("read own role attributes: %v", err)
	}
	if isSuper || bypass {
		t.Fatalf("test role %s is superuser=%v bypassrls=%v — row security would not apply and these tests would prove nothing",
			rlsRoleName, isSuper, bypass)
	}

	return pool
}

// TestPgStore_RLS_FindDelegatedActions_ResolvesUnderOrdinaryRole is the
// regression test for the bug described in this file's header: delegated
// access granting nothing at all on any deployment where RLS binds.
//
// Asserted for BOTH tenant modes, because they take different code paths and
// only one of them was fixable in Go:
//
//	tenant supplied  -> withRLS installs app.tenant_id; the 000006 policy
//	                    matches.
//	no tenant        -> withPlatformScope sets app.platform_scope, which the
//	                    000006 policy did not honour at all. That half needed
//	                    migration 000008; routing the query through the helper
//	                    was not enough on its own, and this case is what proves
//	                    the migration is doing work.
//
// The no-tenant case is NOT reachable over HTTP on a default deployment — the
// canonical input-contract middleware answers 401 to a POST with no
// X-Tenant-Id before the handler runs. It is asserted here anyway, at the
// store level where it lives, because it IS reachable in observe mode and
// because FindDelegatedActions documents an empty tenant as evaluating across
// tenants. A documented contract that silently returns nothing is how the
// original defect survived review, so it is pinned rather than trusted.
func TestPgStore_RLS_FindDelegatedActions_ResolvesUnderOrdinaryRole(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)

	adminStore := store.New(admin, zap.NewNop())
	ctx := context.Background()

	const tenantID = "00000000-0000-0000-0000-0000000000a1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"

	setupRoleWithGrant(t, adminStore, tenantID, "boss-1", legalEntityID, "FINANCE_APPROVER", []string{"PAYMENT_APPROVE"})
	if _, err := adminStore.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID:             tenantID,
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-1", ScopeType: "FULL",
		LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	rlsPool := getRLSPool(t, admin)
	defer rlsPool.Close()
	rlsStore := store.New(rlsPool, zap.NewNop())

	t.Run("with a verified tenant", func(t *testing.T) {
		actions, basis, err := rlsStore.FindDelegatedActions(ctx, "assistant-1", legalEntityID, tenantID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" {
			t.Fatalf("delegated access granted nothing under an ordinary role: got %v, want [PAYMENT_APPROVE]", actions)
		}
		if basis != "delegated:from=boss-1" {
			t.Errorf("basis = %q, want delegated:from=boss-1", basis)
		}
	})

	t.Run("with no tenant — reachable in observe mode, and the documented contract", func(t *testing.T) {
		actions, _, err := rlsStore.FindDelegatedActions(ctx, "assistant-1", legalEntityID, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" {
			t.Fatalf("delegated access granted nothing for a tenantless caller: got %v, want [PAYMENT_APPROVE]. "+
				"This is the case migration 000008's platform-scope hatch exists for — reachable in observe mode, "+
				"and the contract FindDelegatedActions documents either way.", actions)
		}
	})
}

// TestPgStore_RLS_PlatformScopeStillResolvesGrants is the test
// 000007_add_rls_bundles_assignments.up.sql's own comment cites by name
// ("It is proven by test, not assumed: see
// TestPgStore_RLS_PlatformScopeStillResolvesGrants") and which did not exist.
//
// What it pins: FindGrantedActions is the core /v1/authorize read and joins
// roles, permission_bundles and principal_role_assignments — all three with
// RLS. Called with no tenant it runs under app.platform_scope, and all three
// policies have to honour that flag or the join matches zero rows and the
// service answers DENIED `no_grant` platform-wide, with no error to find it by.
func TestPgStore_RLS_PlatformScopeStillResolvesGrants(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)

	adminStore := store.New(admin, zap.NewNop())
	const tenantID = "00000000-0000-0000-0000-0000000000a1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	setupRoleWithGrant(t, adminStore, tenantID, "user-1", legalEntityID, "FINANCE_APPROVER", []string{"PAYMENT_APPROVE"})

	rlsPool := getRLSPool(t, admin)
	defer rlsPool.Close()
	rlsStore := store.New(rlsPool, zap.NewNop())

	// No tenant: the platform-scope path.
	actions, basis, err := rlsStore.FindGrantedActions(context.Background(), "user-1", legalEntityID, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" {
		t.Fatalf("platform-scope grant resolution returned %v, want [PAYMENT_APPROVE] — "+
			"one of the three policies is not honouring app.platform_scope, which fails authorization closed platform-wide", actions)
	}
	if basis != "rbac:role=FINANCE_APPROVER" {
		t.Errorf("basis = %q, want rbac:role=FINANCE_APPROVER", basis)
	}

	// And the tenant-scoped path still resolves, so the fix is not "platform
	// scope for everything".
	actions, _, err = rlsStore.FindGrantedActions(context.Background(), "user-1", legalEntityID, tenantID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("tenant-scoped grant resolution returned %v, want [PAYMENT_APPROVE]", actions)
	}
}

// TestPgStore_FindDelegatedActions_HonoursActionSubset pins the over-grant
// that delegated_actions (000008) fixes.
//
// scope_type has always accepted 'ACTION_SUBSET' and nothing ever read it, so
// a delegation recorded as a subset conferred the delegator's ENTIRE grant
// set. The delegator here holds three actions and delegates one.
func TestPgStore_FindDelegatedActions_HonoursActionSubset(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	const tenantID = "00000000-0000-0000-0000-0000000000a1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	setupRoleWithGrant(t, s, tenantID, "boss-1", legalEntityID, "FINANCE_LEAD",
		[]string{"PAYMENT_APPROVE", "PAYMENT_INITIATE", "PAYROLL_RUN_FINALIZE"})

	if _, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID:             tenantID,
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-1", ScopeType: "ACTION_SUBSET",
		LegalEntityID:    &legalEntityID,
		DelegatedActions: []string{"PAYMENT_APPROVE"},
		EffectiveFrom:    time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	actions, _, err := s.FindDelegatedActions(ctx, "assistant-1", legalEntityID, tenantID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" {
		t.Fatalf("subset delegation conferred %v, want only [PAYMENT_APPROVE] — "+
			"an ACTION_SUBSET delegation must not confer the delegator's whole grant set", actions)
	}
}

// TestPgStore_FindDelegatedActions_SubsetCannotExceedDelegator pins the other
// half of the intersection: a delegation naming an action the delegator does
// NOT hold confers nothing. The subset narrows the delegator's authority; it
// cannot be a grant in its own right.
func TestPgStore_FindDelegatedActions_SubsetCannotExceedDelegator(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	const tenantID = "00000000-0000-0000-0000-0000000000a1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	setupRoleWithGrant(t, s, tenantID, "boss-1", legalEntityID, "VIEWER", []string{"REPORT_VIEW"})

	if _, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID:             tenantID,
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-1", ScopeType: "ACTION_SUBSET",
		LegalEntityID: &legalEntityID,
		// boss-1 does not hold this.
		DelegatedActions: []string{"PAYROLL_RUN_FINALIZE"},
		EffectiveFrom:    time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	actions, _, err := s.FindDelegatedActions(ctx, "assistant-1", legalEntityID, tenantID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("delegation conferred %v — a delegation must never confer an action its delegator does not hold", actions)
	}
}

// TestPgStore_FindDelegatedActions_DoesNotCrossTenantsWithNoTenantScope pins
// the `r.tenant_id = da.tenant_id` predicate.
//
// On the tenantless path the whole query runs under app.platform_scope, so
// roles and delegations from EVERY tenant are visible. Without that predicate,
// a delegation made in tenant A would resolve against the delegator's roles in
// tenant B — the same cross-tenant escalation FindGrantedActions' own comment
// documents, arriving by a different route. The old implementation had exactly
// this shape: it passed tenantID="" straight through to a nested
// FindGrantedActions.
func TestPgStore_FindDelegatedActions_DoesNotCrossTenantsWithNoTenantScope(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	const tenantA = "00000000-0000-0000-0000-0000000000a1"
	const tenantB = "00000000-0000-0000-0000-0000000000b2"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"

	// The delegator holds a harmless action in tenant A (where the delegation
	// is made) and a dangerous one in tenant B (where it is not).
	setupRoleWithGrant(t, s, tenantA, "boss-1", legalEntityID, "A_VIEWER", []string{"REPORT_VIEW"})
	setupRoleWithGrant(t, s, tenantB, "boss-1", legalEntityID, "B_ADMIN", []string{"PAYROLL_RUN_FINALIZE"})

	if _, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID:             tenantA,
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-1", ScopeType: "FULL",
		LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create delegation: %v", err)
	}

	// Tenantless: the platform-scope path, where both tenants' rows are visible.
	actions, _, err := s.FindDelegatedActions(ctx, "assistant-1", legalEntityID, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range actions {
		if a == "PAYROLL_RUN_FINALIZE" {
			t.Fatalf("a delegation made in tenant A conferred the delegator's tenant B authority (%v) — "+
				"the delegator's roles must be bound to the delegation's own tenant", actions)
		}
	}
	if len(actions) != 1 || actions[0] != "REPORT_VIEW" {
		t.Fatalf("got %v, want only tenant A's [REPORT_VIEW]", actions)
	}
}

// TestPgStore_ProjectDelegation_IsIdempotentOnRedelivery pins the property the
// event consumer depends on: Kafka redelivers, and an INSERT per delivery
// would turn one upstream delegation into several rows that
// FindDelegatedActions would then union into a duplicate grant.
func TestPgStore_ProjectDelegation_IsIdempotentOnRedelivery(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	const tenantID = "00000000-0000-0000-0000-0000000000a1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	setupRoleWithGrant(t, s, tenantID, "boss-1", legalEntityID, "FINANCE_LEAD",
		[]string{"PAYMENT_APPROVE", "PAYMENT_INITIATE"})

	params := domain.ProjectDelegationParams{
		SourceService:        "delegated-authority-svc",
		SourceDelegationID:   "upstream-delegation-1",
		TenantID:             tenantID,
		DelegatorPrincipalID: "boss-1",
		DelegatePrincipalID:  "assistant-1",
		LegalEntityID:        &legalEntityID,
		DelegatedActions:     []string{"PAYMENT_APPROVE"},
		EffectiveFrom:        time.Now().Add(-time.Hour),
	}

	first, err := s.ProjectDelegation(ctx, params)
	if err != nil {
		t.Fatalf("first projection: %v", err)
	}
	second, err := s.ProjectDelegation(ctx, params)
	if err != nil {
		t.Fatalf("redelivered projection: %v", err)
	}
	if first.DelegatedAuthorityID != second.DelegatedAuthorityID {
		t.Fatalf("redelivery created a second row (%s then %s) — one upstream delegation must be one row",
			first.DelegatedAuthorityID, second.DelegatedAuthorityID)
	}
	if second.ScopeType != "ACTION_SUBSET" {
		t.Errorf("scope_type = %q, want ACTION_SUBSET for a projection naming one action", second.ScopeType)
	}

	// And it grants exactly the one action upstream authorised.
	actions, _, err := s.FindDelegatedActions(ctx, "assistant-1", legalEntityID, tenantID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" {
		t.Fatalf("projected delegation conferred %v, want only [PAYMENT_APPROVE]", actions)
	}

	// An upstream revocation ends it.
	if _, err := s.RevokeProjectedDelegation(ctx, params.SourceService, params.SourceDelegationID, tenantID); err != nil {
		t.Fatalf("revoke projected delegation: %v", err)
	}
	actions, _, err = s.FindDelegatedActions(ctx, "assistant-1", legalEntityID, tenantID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("revoked projection still confers %v", actions)
	}
}

// TestPgStore_RevokeProjectedDelegation_LeavesLocalRowsAlone pins that an
// upstream id can never end a locally-authored delegation, however it
// collides. Otherwise one service's event could revoke another's grant.
func TestPgStore_RevokeProjectedDelegation_LeavesLocalRowsAlone(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	const tenantID = "00000000-0000-0000-0000-0000000000a1"
	legalEntityID := "00000000-0000-0000-0000-0000000000e1"
	setupRoleWithGrant(t, s, tenantID, "boss-1", legalEntityID, "FINANCE_APPROVER", []string{"PAYMENT_APPROVE"})

	local, err := s.CreateDelegatedAuthority(ctx, domain.CreateDelegatedAuthorityParams{
		TenantID:             tenantID,
		DelegatorPrincipalID: "boss-1", DelegatePrincipalID: "assistant-1", ScopeType: "FULL",
		LegalEntityID: &legalEntityID, EffectiveFrom: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("create local delegation: %v", err)
	}

	// An upstream event naming the LOCAL row's id must find nothing.
	if _, err := s.RevokeProjectedDelegation(ctx, "delegated-authority-svc", local.DelegatedAuthorityID, tenantID); err == nil {
		t.Fatal("an upstream revocation matched a locally-authored delegation")
	}

	still, err := s.FindDelegatedAuthorityByID(ctx, local.DelegatedAuthorityID, tenantID)
	if err != nil {
		t.Fatalf("re-read local delegation: %v", err)
	}
	if still.RevocationStatus != "ACTIVE" {
		t.Fatalf("local delegation is %s — an upstream event revoked a delegation it does not own", still.RevocationStatus)
	}
}
