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

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/middleware"
	"zoiko.io/ai-governance-svc/internal/store"
)

// Row-level security tests for ai_runs, automation_policies and
// automation_actions (migration 000002_add_rls.up.sql), plus the PgStore
// tenant predicates added alongside it.
//
// These run as a purpose-created NOSUPERUSER NOBYPASSRLS role.
// TEST_DATABASE_URL points at `postgres`, a SUPERUSER, and a superuser
// bypasses row-level security unconditionally — FORCE included — so an
// isolation assertion made over that connection would prove nothing about
// the policy.

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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS policy_change_approvals, model_provider_registrations,
		automation_actions, automation_policies, action_risk_classifications, ai_runs CASCADE;`)

	// Every up migration in filename order, never one hardcoded name — a
	// suite that names migrations individually silently skips new ones,
	// which would leave the migration under test unapplied and the run
	// green for the wrong reason.
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

var (
	tenantA = uuid.NewString()
	tenantB = uuid.NewString()
)

func TestRLS_EnabledAndForced(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	for _, table := range []string{"ai_runs", "automation_policies", "automation_actions"} {
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

// TestRLS_PlatformTablesHaveNoPolicy pins the deliberate asymmetry, so a
// later "make the schema uniform" change has to argue with a failing test
// rather than quietly inventing a tenant boundary doc7 §G3 rules out.
func TestRLS_PlatformTablesHaveNoPolicy(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	for _, table := range []string{
		"action_risk_classifications",
		"model_provider_registrations",
		"policy_change_approvals",
	} {
		var enabled bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled); err != nil {
			t.Fatalf("read pg_class for %s: %v", table, err)
		}
		if enabled {
			t.Errorf("%s has row level security enabled, but it carries no tenant_id: "+
				"doc7 §G3 makes policy/taxonomy/provider governance platform-wide, "+
				"so a tenant policy here would be a boundary the doc rules out", table)
		}
	}
}

func newAIRun(tenantID string) *domain.AIRun {
	return &domain.AIRun{
		AIRunID:              uuid.NewString(),
		TenantID:             tenantID,
		RunType:              domain.AIRunType("RECOMMENDATION"),
		ModelID:              "claude-opus-5",
		PromptVersion:        "v3",
		SourceRefs:           []string{"src-confidential-1"},
		EvidenceRefs:         []string{"ev-confidential-1"},
		UncertaintyState:     domain.UncertaintyNone,
		AuditID:              "audit-1",
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "principal-01",
	}
}

// TestRLS_PgStore_AIRunIsolation: tenant B must not reach tenant A's AI
// run, and tenant A must still reach its own. An AI run carries the
// reasoning behind a governed decision and the evidence it rested on.
func TestRLS_PgStore_AIRunIsolation(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	run := newAIRun(tenantA)
	if err := s.CreateAIRun(ctxA, run); err != nil {
		t.Fatalf("create tenant A's ai run: %v", err)
	}

	if got, err := s.GetAIRun(ctxB, run.AIRunID); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's ai run: %+v", got)
	}

	own, err := s.GetAIRun(ctxA, run.AIRunID)
	if err != nil {
		t.Fatalf("tenant A must still read its own ai run: %v", err)
	}
	if own.ModelID != "claude-opus-5" {
		t.Fatalf("unexpected ai run returned for tenant A: %+v", own)
	}
}

// TestRLS_PgStore_ResolveAutomationPolicy_ForeignTenantFailsClosed is the
// most security-relevant read in the service: doc7 §G7's may-this-run
// check, which also reports kill_switch_engaged. It used to accept a
// caller-supplied ?tenant_id=, so a caller could resolve against another
// tenant's allowlist. With the tenant taken from the verified context,
// resolving as B must fail CLOSED (NOT_ALLOWLISTED) even though a matching
// allowlist row exists for A.
func TestRLS_PgStore_ResolveAutomationPolicy_ForeignTenantFailsClosed(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	p := &domain.AutomationPolicy{
		AutomationPolicyID:   uuid.NewString(),
		TenantID:             tenantA,
		Role:                 "FINANCE_AGENT",
		RiskCategory:         domain.RiskCategoryNone,
		Tool:                 "LEDGER_POST",
		ActionType:           "POST_JOURNAL",
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "principal-01",
	}
	if err := s.CreateAutomationPolicy(ctxA, p); err != nil {
		t.Fatalf("create tenant A's automation policy: %v", err)
	}

	// Tenant A's own resolve: allowed.
	resA, err := s.ResolveAutomationPolicy(ctxA, tenantA, "FINANCE_AGENT", string(domain.RiskCategoryNone), "LEDGER_POST", "POST_JOURNAL")
	if err != nil {
		t.Fatalf("resolve as tenant A: %v", err)
	}
	if !resA.Allowed {
		t.Fatalf("tenant A's own allowlisted action must resolve ALLOWED, got %+v", resA)
	}

	// Tenant B resolving the same action must fail closed, even if it
	// somehow passes tenant A's id as the parameter — RLS is the gate.
	resB, err := s.ResolveAutomationPolicy(ctxB, tenantA, "FINANCE_AGENT", string(domain.RiskCategoryNone), "LEDGER_POST", "POST_JOURNAL")
	if err != nil {
		t.Fatalf("resolve as tenant B: %v", err)
	}
	if resB.Allowed {
		t.Fatal("ISOLATION FAILURE: tenant B resolved ALLOWED against tenant A's autonomy allowlist")
	}
	if resB.ReasonCode != "NOT_ALLOWLISTED" {
		t.Fatalf("expected fail-closed NOT_ALLOWLISTED for tenant B, got %+v", resB)
	}
}

// TestRLS_PgStore_DecideAutomationAction_ForeignTenantRefused covers the
// most consequential write in the service. The UPDATE was `WHERE
// automation_action_id = $5 AND approval_status = 'PENDING'`, so a caller
// holding another tenant's action id could APPROVE that tenant's pending
// autonomous action — granting agentic execution authority inside someone
// else's tenant (doc7 §G2/§G7).
func TestRLS_PgStore_DecideAutomationAction_ForeignTenantRefused(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	action := &domain.AutomationAction{
		AutomationActionID:    uuid.NewString(),
		TenantID:              tenantA,
		ActionType:            "POST_JOURNAL",
		RiskCategory:          domain.RiskCategoryNone,
		IdempotencyKey:        uuid.NewString(),
		PreconditionsMet:      true,
		ApprovalStatus:        domain.ApprovalPending,
		Status:                domain.AutomationActionProposed,
		ProposedByPrincipalID: "principal-a-maker",
		CreatedAt:             time.Now().UTC(),
	}
	if err := s.ProposeAutomationAction(ctxA, action); err != nil {
		t.Fatalf("propose tenant A's action: %v", err)
	}

	// Tenant B tries to approve tenant A's pending autonomous action.
	if got, err := s.DecideAutomationAction(ctxB, action.AutomationActionID,
		string(domain.ApprovalApproved), "principal-b-checker"); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B approved tenant A's autonomous action: %+v", got)
	}

	// And it must still be PENDING for tenant A — the write must not have
	// landed at all, rather than landing and merely erroring on read-back.
	still, err := s.GetAutomationAction(ctxA, action.AutomationActionID)
	if err != nil {
		t.Fatalf("tenant A must still read its own action: %v", err)
	}
	if still.ApprovalStatus != domain.ApprovalPending {
		t.Fatalf("ISOLATION FAILURE: tenant A's action was decided by tenant B: approval_status=%s approved_by=%v",
			still.ApprovalStatus, still.ApprovedByPrincipalID)
	}

	// Sanity: tenant A's own checker can still decide it.
	decided, err := s.DecideAutomationAction(ctxA, action.AutomationActionID,
		string(domain.ApprovalApproved), "principal-a-checker")
	if err != nil {
		t.Fatalf("tenant A must be able to decide its own action: %v", err)
	}
	if decided.ApprovalStatus != domain.ApprovalApproved {
		t.Fatalf("unexpected state after tenant A's decision: %+v", decided)
	}
}

// TestRLS_WithCheckRefusesForeignTenantWrite covers the write side: USING
// alone governs visibility, so without WITH CHECK a caller could insert a
// row attributed to another tenant that it then cannot read back.
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
		INSERT INTO automation_policies
			(tenant_id, role, risk_category, tool, action_type, created_by_principal_id)
		VALUES ($1, 'FORGED_ROLE', 'NONE', 'LEDGER_POST', 'POST_JOURNAL', 'principal-b')`, tenantA)
	if err == nil {
		t.Fatal("WITH CHECK must refuse an allowlist entry attributed to another tenant")
	}
}

// TestRLS_TenantlessQueryReturnsNothing pins the NULLIF guard. tenant_id is
// UUID here, and current_setting with missing_ok returns the empty string
// when app.tenant_id was never set — casting the empty string to uuid
// raises invalid input syntax, so without NULLIF a tenant-less query would
// ERROR rather than return no rows. It must return no rows: fail closed and
// quiet, not fail loud in a way a caller could use to distinguish states.
//
// (The comment describes the predicate in words rather than quoting it:
// gofmt's doc-comment formatter rewrites a doubled single-quote into a
// curly quote, which silently mangles SQL snippets in comments.)
func TestRLS_TenantlessQueryReturnsNothing(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)

	// Seed a row for tenant A through the store.
	s := store.NewPgStore(appPool)
	if err := s.CreateAIRun(middleware.WithTenant(ctx, tenantA), newAIRun(tenantA)); err != nil {
		t.Fatalf("seed tenant A's ai run: %v", err)
	}

	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// No set_config at all — app.tenant_id was never set on this session.
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM ai_runs`).Scan(&count); err != nil {
		t.Fatalf("a tenant-less SELECT must return no rows, not error: %v", err)
	}
	if count != 0 {
		t.Fatalf("a tenant-less SELECT must see nothing, saw %d rows", count)
	}
}
