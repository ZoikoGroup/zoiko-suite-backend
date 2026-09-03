package store_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/store"
)

// ── abac_rules ───────────────────────────────────────────────────────────────

const (
	abacTenantA = "00000000-0000-0000-0000-0000000000a1"
	abacTenantB = "00000000-0000-0000-0000-0000000000b2"
)

func strptr(s string) *string { return &s }

// TestPgStore_ABACRules_ShipsEmpty is the property the whole design rests on:
// migration 000010 declares the mechanism and seeds no policy, so layer 5 of
// /v1/authorize changes nothing until somebody who knows the business declares
// a rule.
//
// If a future migration seeds a rule, this fails — which is the point. A rule
// arriving with a schema change is exactly the "inventing business logic" this
// service has no standing to do.
func TestPgStore_ABACRules_ShipsEmpty(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	rules, err := s.FindABACRules(context.Background(), "PAYMENT_APPROVE", abacTenantA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("abac_rules ships with %d rules — a migration is declaring policy, which is a business decision this service does not own: %+v",
			len(rules), rules)
	}
}

func TestPgStore_CreateABACRule_ValidatesEffectAndOperator(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	base := domain.CreateABACRuleParams{
		TenantID: strptr(abacTenantA), RuleCode: "R1", ActionType: "PAYMENT_APPROVE",
		Effect: domain.EffectRequire, AttributeKey: "amount", Operator: "eq",
		AttributeValue: strptr("1"), CreatedByPrincipalID: "admin-1",
	}

	tests := []struct {
		name    string
		mutate  func(p *domain.CreateABACRuleParams)
		wantErr error
	}{
		{
			name:    "unknown effect",
			mutate:  func(p *domain.CreateABACRuleParams) { p.Effect = "MAYBE" },
			wantErr: domain.ErrUnsupportedABACEffect,
		},
		{
			name:    "unknown operator",
			mutate:  func(p *domain.CreateABACRuleParams) { p.Operator = "approximately" },
			wantErr: domain.ErrUnsupportedABACOperator,
		},
		{
			name:    "comparison operator with no operand",
			mutate:  func(p *domain.CreateABACRuleParams) { p.Operator = "gt"; p.AttributeValue = nil },
			wantErr: domain.ErrABACOperandRequired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			if _, err := s.CreateABACRule(ctx, p); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v — an unevaluable rule denies its action for every principal, so it must not be storable", err, tc.wantErr)
			}
		})
	}

	// exists takes no operand, and must be accepted without one.
	p := base
	p.RuleCode = "R_EXISTS"
	p.Operator = "exists"
	p.AttributeValue = nil
	if _, err := s.CreateABACRule(ctx, p); err != nil {
		t.Fatalf("exists with no operand was refused: %v", err)
	}
}

// TestPgStore_CreateABACRule_RuleCodeIsUniqueWithinItsScope pins both partial
// unique indexes from 000010. A single UNIQUE(tenant_id, rule_code) would not
// do it: in Postgres NULLs are distinct in a unique index, so the same
// platform-wide rule_code could be created any number of times.
func TestPgStore_CreateABACRule_RuleCodeIsUniqueWithinItsScope(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantScoped := domain.CreateABACRuleParams{
		TenantID: strptr(abacTenantA), RuleCode: "DUAL_APPROVAL", ActionType: "PAYMENT_APPROVE",
		Effect: domain.EffectRequire, AttributeKey: "dual_approved", Operator: "eq",
		AttributeValue: strptr("true"), CreatedByPrincipalID: "admin-1",
	}
	if _, err := s.CreateABACRule(ctx, tenantScoped); err != nil {
		t.Fatalf("first tenant-scoped rule: %v", err)
	}
	if _, err := s.CreateABACRule(ctx, tenantScoped); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate tenant-scoped rule_code: err = %v, want ErrConflict", err)
	}

	// Another tenant may reuse the code — the scopes are separate.
	otherTenant := tenantScoped
	otherTenant.TenantID = strptr(abacTenantB)
	if _, err := s.CreateABACRule(ctx, otherTenant); err != nil {
		t.Fatalf("another tenant could not reuse the rule_code: %v", err)
	}

	// And a platform-wide rule may reuse it once, but only once.
	global := tenantScoped
	global.TenantID = nil
	if _, err := s.CreateABACRule(ctx, global); err != nil {
		t.Fatalf("first platform-wide rule: %v", err)
	}
	if _, err := s.CreateABACRule(ctx, global); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate platform-wide rule_code: err = %v, want ErrConflict — "+
			"NULLs are distinct in a Postgres unique index, so this needs its own partial index", err)
	}
}

// TestPgStore_FindABACRules_ScopeAndActiveFlag pins the evaluation read: the
// tenant's own rules plus the platform-wide ones, active only, for one action.
func TestPgStore_FindABACRules_ScopeAndActiveFlag(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	mk := func(code string, tenant *string, action string) *domain.ABACRule {
		r, err := s.CreateABACRule(ctx, domain.CreateABACRuleParams{
			TenantID: tenant, RuleCode: code, ActionType: action,
			Effect: domain.EffectForbid, AttributeKey: "channel", Operator: "eq",
			AttributeValue: strptr("SELF_SERVICE"), CreatedByPrincipalID: "admin-1",
		})
		if err != nil {
			t.Fatalf("create %s: %v", code, err)
		}
		return r
	}

	mk("TENANT_A_RULE", strptr(abacTenantA), "PAYMENT_APPROVE")
	mk("GLOBAL_RULE", nil, "PAYMENT_APPROVE")
	mk("TENANT_B_RULE", strptr(abacTenantB), "PAYMENT_APPROVE")
	mk("OTHER_ACTION_RULE", strptr(abacTenantA), "REPORT_VIEW")
	retired := mk("RETIRED_RULE", strptr(abacTenantA), "PAYMENT_APPROVE")

	if _, err := s.SetABACRuleActive(ctx, retired.ABACRuleID, abacTenantA, false); err != nil {
		t.Fatalf("retire: %v", err)
	}

	rules, err := s.FindABACRules(ctx, "PAYMENT_APPROVE", abacTenantA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := map[string]bool{}
	for _, r := range rules {
		got[r.RuleCode] = true
	}
	if !got["TENANT_A_RULE"] {
		t.Error("the tenant's own rule is missing")
	}
	if !got["GLOBAL_RULE"] {
		t.Error("the platform-wide rule is missing — it binds every tenant")
	}
	if got["TENANT_B_RULE"] {
		t.Error("another tenant's rule leaked into this tenant's evaluation")
	}
	if got["OTHER_ACTION_RULE"] {
		t.Error("a rule for a different action was returned")
	}
	if got["RETIRED_RULE"] {
		t.Error("a retired rule is still being enforced — retiring is how a rule stops denying")
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want exactly 2: %+v", len(rules), rules)
	}
}

// TestPgStore_SetABACRuleActive_CannotRetireAPlatformWideRule. A rule that
// binds every tenant must not be disableable by any one of them — the store's
// predicate is `tenant_id = $3` with no IS NULL branch, so it answers
// not-found and the handler answers 404.
func TestPgStore_SetABACRuleActive_CannotRetireAPlatformWideRule(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	global, err := s.CreateABACRule(ctx, domain.CreateABACRuleParams{
		TenantID: nil, RuleCode: "GLOBAL_RULE", ActionType: "PAYMENT_APPROVE",
		Effect: domain.EffectForbid, AttributeKey: "channel", Operator: "eq",
		AttributeValue: strptr("SELF_SERVICE"), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create platform-wide rule: %v", err)
	}

	if _, err := s.SetABACRuleActive(ctx, global.ABACRuleID, abacTenantA, false); !errors.Is(err, domain.ErrABACRuleNotFound) {
		t.Fatalf("err = %v, want ErrABACRuleNotFound — one tenant must not be able to disable a control binding every tenant", err)
	}

	// It is still enforced.
	rules, err := s.FindABACRules(ctx, "PAYMENT_APPROVE", abacTenantA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("the platform-wide rule was retired from a tenant scope: %+v", rules)
	}
}

// TestPgStore_ABACRules_TenantIsolationUnderOrdinaryRole — abac_rules carries
// RLS, and a table whose policy is only ever exercised as a superuser has an
// untested policy.
func TestPgStore_ABACRules_TenantIsolationUnderOrdinaryRole(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)

	adminStore := store.New(admin, zap.NewNop())
	ctx := context.Background()

	for _, tc := range []struct {
		code   string
		tenant *string
	}{
		{"TENANT_A_RULE", strptr(abacTenantA)},
		{"TENANT_B_RULE", strptr(abacTenantB)},
		{"GLOBAL_RULE", nil},
	} {
		if _, err := adminStore.CreateABACRule(ctx, domain.CreateABACRuleParams{
			TenantID: tc.tenant, RuleCode: tc.code, ActionType: "PAYMENT_APPROVE",
			Effect: domain.EffectForbid, AttributeKey: "channel", Operator: "eq",
			AttributeValue: strptr("SELF_SERVICE"), CreatedByPrincipalID: "admin-1",
		}); err != nil {
			t.Fatalf("create %s: %v", tc.code, err)
		}
	}

	rlsPool := getRLSPool(t, admin)
	defer rlsPool.Close()
	rlsStore := store.New(rlsPool, zap.NewNop())

	rules, err := rlsStore.FindABACRules(ctx, "PAYMENT_APPROVE", abacTenantA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range rules {
		if r.RuleCode == "TENANT_B_RULE" {
			t.Fatalf("tenant B's rule is visible in tenant A's scope under a role the policy binds")
		}
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want tenant A's own plus the platform-wide one: %+v", len(rules), rules)
	}
}

// ── access_decision_log partitioning and retention (000009) ──────────────────

// TestPgStore_AccessDecisionLog_PartitionRoutingAndRetrieval pins that the
// partitioned table behaves exactly as the flat one did for the service: the
// insert routes, RETURNING works (which is what makes the policy's SELECT side
// apply), and the id-only read still finds the row across partitions.
func TestPgStore_AccessDecisionLog_PartitionRoutingAndRetrieval(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	d, err := s.RecordAccessDecision(ctx, domain.RecordAccessDecisionParams{
		PrincipalID: "p-1", LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		ActionType: "PAYMENT_APPROVE", Outcome: "GRANTED", Basis: "rbac:role=X",
		CorrelationID: "corr-1", TenantID: abacTenantA,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if d.AccessDecisionID == "" {
		t.Fatal("no id returned — RETURNING on a partitioned insert is what the policy's SELECT side applies to")
	}

	// Landed in the right month's partition, not the default one. A non-empty
	// default partition means a month was missing when a decision was
	// recorded, which is the symptom the retention view reports.
	var inDefault int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM access_decision_log_default`).Scan(&inDefault); err != nil {
		t.Fatalf("read default partition: %v", err)
	}
	if inDefault != 0 {
		t.Errorf("%d rows landed in the default partition — the current month has no partition", inDefault)
	}

	got, err := s.FindAccessDecisionByID(ctx, d.AccessDecisionID, abacTenantA)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if got.DecisionBasis != "rbac:role=X" {
		t.Errorf("basis = %q", got.DecisionBasis)
	}
}

// TestPgStore_AccessDecisionLog_TenantlessInsertStillSucceeds is the outage
// test. Roughly 57 of the ~60 services calling /v1/authorize send no tenant,
// and RecordAccessDecision is the last step before the endpoint answers — so a
// tenantless insert that fails is a 503 on the platform's hottest path. Both
// 000005's NULL-admitting policy and 000009's partitioning have to preserve
// this.
func TestPgStore_AccessDecisionLog_TenantlessInsertStillSucceeds(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)

	rlsPool := getRLSPool(t, admin)
	defer rlsPool.Close()
	rlsStore := store.New(rlsPool, zap.NewNop())

	d, err := rlsStore.RecordAccessDecision(context.Background(), domain.RecordAccessDecisionParams{
		PrincipalID: "p-1", LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		ActionType: "PAYMENT_APPROVE", Outcome: "DENIED", Basis: "no_grant",
		CorrelationID: "corr-1", TenantID: "",
	})
	if err != nil {
		t.Fatalf("a tenantless decision could not be recorded under an ordinary role: %v — "+
			"this is a 503 on every request from a caller that sends no X-Tenant-Id", err)
	}
	if d.TenantID != nil {
		t.Errorf("tenant_id = %v, want SQL NULL for an unattributed decision", *d.TenantID)
	}
}

// TestPgStore_AccessDecisionLog_RetentionDetachesWholeMonthsOnly exercises
// migration 000009's retention function directly, because nothing in the
// service calls it — it is operational tooling, and untested operational
// tooling on an append-only evidence table is how evidence gets lost.
//
// Asserts the three properties that matter: only months entirely older than
// the cutoff go, the rows SURVIVE in the detached table (DETACH, never
// DELETE), and the default partition is never detached whatever the cutoff.
func TestPgStore_AccessDecisionLog_RetentionDetachesWholeMonthsOnly(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	ctx := context.Background()

	// Six months of decisions, one per month, oldest first. Inserted directly
	// because the store always stamps NOW() — backdating is the point here.
	//
	// monthsAgo is passed as TEXT, not an int: `$1 || ' month'` makes Postgres
	// resolve the parameter's type from the concatenation, which is text, and
	// pgx has no encode plan from int to text.
	for i := 0; i < 6; i++ {
		monthsAgo := strconv.Itoa(i)
		if _, err := pool.Exec(ctx, `
			SELECT create_access_decision_log_partition((CURRENT_DATE - ($1 || ' month')::interval)::date)`, monthsAgo); err != nil {
			t.Fatalf("create partition -%s months: %v", monthsAgo, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO access_decision_log (principal_id, legal_entity_id, action_type, decision_outcome, decision_basis, tenant_id, decided_at)
			VALUES ($1, '00000000-0000-0000-0000-0000000000e1', 'PAYMENT_APPROVE', 'GRANTED', 'rbac:role=X', $2,
			        NOW() - ($3 || ' month')::interval)`,
			fmt.Sprintf("p-backdated-%d", i), abacTenantA, monthsAgo); err != nil {
			t.Fatalf("insert -%s months: %v", monthsAgo, err)
		}
	}

	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM access_decision_log`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 6 {
		t.Fatalf("expected 6 decisions across 6 months, got %d", total)
	}

	// Detach everything ending on or before three months ago.
	rows, err := pool.Query(ctx, `
		SELECT partition_name, row_count
		  FROM detach_access_decision_log_partitions_before((date_trunc('month', CURRENT_DATE) - INTERVAL '3 months')::date)`)
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	detached := map[string]int64{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		detached[name] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(detached) == 0 {
		t.Fatal("nothing was detached — the retention function is a no-op")
	}
	for name := range detached {
		if name == "access_decision_log_default" {
			t.Fatal("the default partition was detached — it holds rows whose dates nobody anticipated, " +
				"so its contents are not 'everything before the cutoff', and detaching it removes the parent's safety net")
		}
	}

	// The rows survive: DETACH, not DELETE. This is what keeps 000001's
	// append-only guarantee true while still bounding the hot table.
	var survived int64
	for name, n := range detached {
		var got int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+name).Scan(&got); err != nil {
			t.Fatalf("count detached %s: %v", name, err)
		}
		if got != n {
			t.Errorf("%s reported %d rows but holds %d", name, n, got)
		}
		survived += got
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM access_decision_log`).Scan(&remaining); err != nil {
		t.Fatalf("count after detach: %v", err)
	}
	if int64(remaining)+survived != int64(total) {
		t.Fatalf("%d rows remain and %d were detached, from %d — evidence was lost, not archived",
			remaining, survived, total)
	}
	if remaining == 0 {
		t.Fatal("every month was detached — a cutoff three months back must leave the recent months attached")
	}
}
