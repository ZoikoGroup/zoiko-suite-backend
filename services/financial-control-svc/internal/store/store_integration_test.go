//go:build integration

// Real-Postgres tests for the control store. This pool connects as a superuser
// (like every service here), which bypasses RLS — so the tenant-isolation
// assertions below prove the EXPLICIT tenant_id predicates, not the policy.
//
// Run: go test -v -tags=integration -count=1 -timeout=180s ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	port := uint32(16101 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).Port(port).Database("finctrl_test").
		Username("postgres").Password("postgres"))
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	dsn := fmt.Sprintf("host=localhost port=%d dbname=finctrl_test user=postgres password=postgres sslmode=disable", port)
	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("connect: %v\n", err)
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
		fmt.Printf("postgres not ready: %v\n", err)
		_ = pg.Stop()
		os.Exit(1)
	}
	// Every *.up.sql by glob, never a named list.
	files, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
	for _, f := range files {
		sqlBytes, rerr := os.ReadFile(f)
		if rerr == nil {
			_, rerr = testPool.Exec(ctx, string(sqlBytes))
		}
		if rerr != nil {
			fmt.Printf("migration %s: %v\n", f, rerr)
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

var ctx = context.Background()

func newDef(code string) domain.CreateControlDefinitionRequest {
	return domain.CreateControlDefinitionRequest{
		ControlCode: code, Name: "AR subledger to GL", Domain: "AR", ControlType: "BALANCE",
		Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY", Frequency: "PERIOD_END", CloseGating: true,
		OwnerRole: "AR_CONTROLLER", ReviewerRole: "SR_ACCOUNTANT", CertifierRole: "CONTROLLER",
		SourceSpec:     json.RawMessage(`{"system":"accounts-receivable-svc"}`),
		TargetSpec:     json.RawMessage(`{"system":"general-ledger-svc"}`),
		EvidencePolicy: json.RawMessage(`{"retention_class":"FINANCIAL_7Y"}`),
		InitialLogic:   json.RawMessage(`{"kind":"MATCH"}`), InitialEffectiveFrom: "2026-01-01",
	}
}

// approvedDef creates a definition and gets its v1 approved by a second principal.
func approvedDef(t *testing.T, tenant, code string) *domain.ControlDefinition {
	t.Helper()
	d, err := testStore.CreateDefinition(ctx, tenant, "maker", "corr", newDef(code))
	require.NoError(t, err)
	_, err = testStore.ApproveRuleVersion(ctx, tenant, d.ControlDefinitionID, 1, "checker")
	require.NoError(t, err)
	return d
}

func runReq(defID, entity string) domain.CreateRunRequest {
	return domain.CreateRunRequest{ControlDefinitionID: defID, LegalEntityID: entity, TriggerType: "PERIOD_END", PeriodID: "2026-09"}
}

func TestDefinition_CreateGetAndDuplicateCode(t *testing.T) {
	tenant := uuid.NewString()
	d, err := testStore.CreateDefinition(ctx, tenant, "maker", "c", newDef("FIN-CTRL-001"))
	require.NoError(t, err)
	require.NotNil(t, d.LatestRuleVersion)
	assert.Equal(t, 1, *d.LatestRuleVersion)

	got, err := testStore.GetDefinition(ctx, tenant, d.ControlDefinitionID)
	require.NoError(t, err)
	assert.Equal(t, "FIN-CTRL-001", got.ControlCode)
	assert.Equal(t, []string{"COMPLETENESS", "ACCURACY"}, got.Assertions)

	_, err = testStore.CreateDefinition(ctx, tenant, "maker", "c", newDef("FIN-CTRL-001"))
	assert.ErrorIs(t, err, domain.ErrDuplicate, "one stable id per control code (Invariant 1)")
}

func TestDefinition_TenantIsolation(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	d, err := testStore.CreateDefinition(ctx, a, "maker", "c", newDef("FIN-CTRL-001"))
	require.NoError(t, err)

	_, err = testStore.GetDefinition(ctx, b, d.ControlDefinitionID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "scenario 28: cross-tenant read blocked")
	list, err := testStore.ListDefinitions(ctx, b, 50)
	require.NoError(t, err)
	assert.Empty(t, list)
	_, err = testStore.CreateRuleVersion(ctx, b, d.ControlDefinitionID, "maker",
		domain.CreateRuleVersionRequest{Logic: json.RawMessage(`{"x":1}`), EffectiveFrom: "2026-01-01"})
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Tenant B may reuse the same control code independently.
	_, err = testStore.CreateDefinition(ctx, b, "maker", "c", newDef("FIN-CTRL-001"))
	assert.NoError(t, err)
}

func TestDefinition_ImmutableAtDatabaseLevel(t *testing.T) {
	tenant := uuid.NewString()
	d, err := testStore.CreateDefinition(ctx, tenant, "maker", "c", newDef("FIN-CTRL-001"))
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE control_definitions SET owner_role = 'X' WHERE control_definition_id = $1`, d.ControlDefinitionID)
	assert.Error(t, err, "definitions are append-only")
	_, err = testPool.Exec(ctx, `DELETE FROM control_definitions WHERE control_definition_id = $1`, d.ControlDefinitionID)
	assert.Error(t, err)
}

func TestRuleVersion_ApprovalRules(t *testing.T) {
	tenant := uuid.NewString()
	d, err := testStore.CreateDefinition(ctx, tenant, "maker", "c", newDef("FIN-CTRL-001"))
	require.NoError(t, err)

	_, err = testStore.ApproveRuleVersion(ctx, tenant, d.ControlDefinitionID, 1, "maker")
	assert.ErrorIs(t, err, domain.ErrSelfApproval, "independent approval for key controls")

	v, err := testStore.ApproveRuleVersion(ctx, tenant, d.ControlDefinitionID, 1, "checker")
	require.NoError(t, err)
	require.NotNil(t, v.ApprovedBy)
	assert.Equal(t, "checker", *v.ApprovedBy)

	_, err = testStore.ApproveRuleVersion(ctx, tenant, d.ControlDefinitionID, 1, "another")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition, "an approved version cannot be re-approved")

	_, err = testPool.Exec(ctx, `UPDATE control_rule_versions SET logic = '{"kind":"MATCH","outstanding_days":9}' WHERE control_definition_id = $1`, d.ControlDefinitionID)
	assert.Error(t, err, "approved rule logic is immutable (Invariant 4)")

	v2, err := testStore.CreateRuleVersion(ctx, tenant, d.ControlDefinitionID, "maker",
		domain.CreateRuleVersionRequest{Logic: json.RawMessage(`{"kind":"MATCH","allow_groups":true}`), EffectiveFrom: "2026-01-01"})
	require.NoError(t, err)
	assert.Equal(t, 2, v2.RuleVersion)
	assert.NotEqual(t, v.LogicDigest, v2.LogicDigest)
}

// Scenario 30: definitions change mid-period; existing runs stay pinned.
func TestRun_PinsRuleVersionAndDigest_AndStaysPinned(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")

	r1, created, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k1", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, 1, r1.RuleVersion)
	assert.True(t, domain.ValidDigest(r1.RuleDigest))
	assert.Equal(t, domain.LifecycleScheduled, r1.LifecycleState)
	assert.Equal(t, domain.ResultNotEvaluated, r1.ResultState)
	assert.Equal(t, domain.CertPending, r1.CertificationState, "key control requires certification")

	// New v2 is created but UNAPPROVED: a new run must still pin v1.
	_, err = testStore.CreateRuleVersion(ctx, tenant, d.ControlDefinitionID, "maker",
		domain.CreateRuleVersionRequest{Logic: json.RawMessage(`{"kind":"MATCH","allow_groups":true}`), EffectiveFrom: "2026-01-01"})
	require.NoError(t, err)
	r2, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k2", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	assert.Equal(t, 1, r2.RuleVersion, "unapproved versions are never pinned")

	// Approve v2: new runs pin v2, the existing run stays on v1.
	_, err = testStore.ApproveRuleVersion(ctx, tenant, d.ControlDefinitionID, 2, "checker")
	require.NoError(t, err)
	r3, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k3", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	assert.Equal(t, 2, r3.RuleVersion)
	assert.NotEqual(t, r1.RuleDigest, r3.RuleDigest)
	again, err := testStore.GetRun(ctx, tenant, r1.RunID)
	require.NoError(t, err)
	assert.Equal(t, 1, again.RuleVersion, "existing run stays pinned")
	assert.Equal(t, r1.RuleDigest, again.RuleDigest)
}

func TestRun_RefusedWithoutApprovedRuleVersion(t *testing.T) {
	tenant := uuid.NewString()
	d, err := testStore.CreateDefinition(ctx, tenant, "maker", "c", newDef("FIN-CTRL-001"))
	require.NoError(t, err)
	_, _, err = testStore.CreateRun(ctx, tenant, "prep", "c", "k", runReq(d.ControlDefinitionID, uuid.NewString()))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestRun_IdempotentReplayAndKeyReuse(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")

	r1, created, err := testStore.CreateRun(ctx, tenant, "prep", "c", "same-key", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	require.True(t, created)
	r2, created, err := testStore.CreateRun(ctx, tenant, "prep", "c", "same-key", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, r1.RunID, r2.RunID, "replay returns the original run")

	other := runReq(d.ControlDefinitionID, entity)
	other.PeriodID = "2026-10"
	_, _, err = testStore.CreateRun(ctx, tenant, "prep", "c", "same-key", other)
	assert.ErrorIs(t, err, domain.ErrConflict, "same key with a different body is never a second run")

	var n int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM control_runs WHERE tenant_id = $1`, tenant).Scan(&n))
	assert.Equal(t, 1, n)
}

// Scenario 04: an operator cannot widen tolerance during a run. Policy changes
// are new versions; a run stays pinned to the version it started with.
func TestRun_TolerancePinned_NewVersionOnlyAffectsNewRuns(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")

	t1, err := testStore.CreateTolerancePolicy(ctx, tenant, "maker", domain.CreateTolerancePolicyRequest{
		LegalEntityID: entity, Metric: "FIN-CTRL-001", AbsoluteTolerance: "0.01", Currency: "USD",
		Rationale: "cent rounding", EffectiveFrom: "2026-01-01", ApprovedBy: "policy-owner"})
	require.NoError(t, err)
	assert.Equal(t, 1, t1.ToleranceVersion)
	assert.Equal(t, "0.010000000000", t1.AbsoluteTolerance, "exact decimal, no float artefacts")

	r1, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "a", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	require.NotNil(t, r1.ToleranceID)
	assert.Equal(t, t1.ToleranceID, *r1.ToleranceID)

	// The DB itself refuses in-place widening.
	_, err = testPool.Exec(ctx, `UPDATE tolerance_policies SET absolute_tolerance = 1000 WHERE tolerance_id = $1`, t1.ToleranceID)
	assert.Error(t, err, "tolerance rows are append-only")

	t2, err := testStore.CreateTolerancePolicy(ctx, tenant, "maker", domain.CreateTolerancePolicyRequest{
		LegalEntityID: entity, Metric: "FIN-CTRL-001", AbsoluteTolerance: "50", Currency: "USD",
		Rationale: "widened", EffectiveFrom: "2026-01-01", ApprovedBy: "policy-owner"})
	require.NoError(t, err)
	assert.Equal(t, 2, t2.ToleranceVersion)

	same, err := testStore.GetRun(ctx, tenant, r1.RunID)
	require.NoError(t, err)
	assert.Equal(t, t1.ToleranceID, *same.ToleranceID, "existing run keeps its pinned tolerance")
	r2, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "b", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	assert.Equal(t, t2.ToleranceID, *r2.ToleranceID, "new run resolves the new version")
}

func TestRun_NamedPolicyMustBelongToTheEntity(t *testing.T) {
	tenant, entityA, entityB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")
	tol, err := testStore.CreateTolerancePolicy(ctx, tenant, "maker", domain.CreateTolerancePolicyRequest{
		LegalEntityID: entityA, Metric: "FIN-CTRL-001", AbsoluteTolerance: "1", Currency: "USD",
		Rationale: "r", EffectiveFrom: "2026-01-01", ApprovedBy: "owner"})
	require.NoError(t, err)

	req := runReq(d.ControlDefinitionID, entityB)
	req.ToleranceID = tol.ToleranceID
	_, _, err = testStore.CreateRun(ctx, tenant, "prep", "c", "x", req)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "entity B cannot borrow entity A's tolerance")
}

func TestRun_AdvanceHappyPath_WritesTransitionsAndOutbox(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")
	run, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)

	pass := domain.ResultPass
	certified := domain.CertCertified
	steps := []store.AdvanceParams{
		{ToLifecycle: domain.LifecyclePreparing},
		{ToLifecycle: domain.LifecyclePopulationFroze},
		{ToLifecycle: domain.LifecycleExecuting},
		{ToLifecycle: domain.LifecycleReadyToCertify, ToResult: &pass},
		{ToLifecycle: domain.LifecycleCertified, ToCert: &certified},
	}
	for i, s := range steps {
		s.ExpectedVersion, s.Actor, s.CorrelationID, s.Reason = run.Version, "prep", "c", "step"
		run, err = testStore.AdvanceRun(ctx, tenant, run.RunID, s)
		require.NoError(t, err, "step %d", i)
	}
	assert.Equal(t, domain.LifecycleCertified, run.LifecycleState)
	assert.Equal(t, domain.ResultPass, run.ResultState)
	assert.NotNil(t, run.CompletedAt)
	assert.Equal(t, 6, run.Version)

	trans, err := testStore.ListTransitions(ctx, tenant, run.RunID)
	require.NoError(t, err)
	dims := map[string]int{}
	for _, tr := range trans {
		dims[tr.Dimension]++
	}
	assert.Equal(t, 6, dims["LIFECYCLE"], "created + 5 moves")
	assert.Equal(t, 1, dims["RESULT"])
	assert.Equal(t, 1, dims["CERTIFICATION"])

	rows, err := testPool.Query(ctx, `SELECT event_type FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 ORDER BY created_at, outbox_event_id`, tenant, run.RunID)
	require.NoError(t, err)
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var et string
		require.NoError(t, rows.Scan(&et))
		seen[et] = true
	}
	for _, want := range []string{"control.run.created", "control.population.frozen", "control.execution.completed",
		"control.run.ready_for_certification", "control.run.certified"} {
		assert.True(t, seen[want], "missing event %s", want)
	}

	// A certified run cannot be edited or deleted underneath the API.
	_, err = testPool.Exec(ctx, `DELETE FROM control_run_transitions WHERE run_id = $1`, run.RunID)
	assert.Error(t, err, "history is append-only (Invariant 13)")
}

func TestRun_OptimisticConcurrency(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")
	run, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)

	_, err = testStore.AdvanceRun(ctx, tenant, run.RunID, store.AdvanceParams{
		ExpectedVersion: run.Version, ToLifecycle: domain.LifecyclePreparing, Actor: "a", CorrelationID: "c"})
	require.NoError(t, err)
	_, err = testStore.AdvanceRun(ctx, tenant, run.RunID, store.AdvanceParams{
		ExpectedVersion: run.Version /* stale */, ToLifecycle: domain.LifecycleExpired, Actor: "b", CorrelationID: "c"})
	assert.ErrorIs(t, err, domain.ErrConflict, "stale ETag must be refused")
}

// Scenario 09: control engine outage => Indeterminate/Failed, never Pass.
func TestRun_TechnicalFailureCanNeverBecomePass(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")
	run, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	for _, to := range []domain.LifecycleState{domain.LifecyclePreparing, domain.LifecyclePopulationFroze, domain.LifecycleExecuting} {
		run, err = testStore.AdvanceRun(ctx, tenant, run.RunID, store.AdvanceParams{ExpectedVersion: run.Version, ToLifecycle: to, Actor: "a", CorrelationID: "c"})
		require.NoError(t, err)
	}
	indet := domain.ResultIndeterminate
	failed, err := testStore.AdvanceRun(ctx, tenant, run.RunID, store.AdvanceParams{
		ExpectedVersion: run.Version, ToLifecycle: domain.LifecycleFailed, ToResult: &indet, Actor: "engine", CorrelationID: "c", Reason: "source feed unreachable"})
	require.NoError(t, err)
	assert.Equal(t, domain.ResultIndeterminate, failed.ResultState)

	pass := domain.ResultPass
	_, err = testStore.AdvanceRun(ctx, tenant, run.RunID, store.AdvanceParams{
		ExpectedVersion: failed.Version, ToLifecycle: domain.LifecycleFailed, ToResult: &pass, Actor: "x", CorrelationID: "c"})
	assert.ErrorIs(t, err, domain.ErrInvalidTransition, "a failed run's result is frozen")

	// And straight to READY_TO_CERTIFY without a passing result is refused.
	run2, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k2", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	for _, to := range []domain.LifecycleState{domain.LifecyclePreparing, domain.LifecyclePopulationFroze, domain.LifecycleExecuting} {
		run2, err = testStore.AdvanceRun(ctx, tenant, run2.RunID, store.AdvanceParams{ExpectedVersion: run2.Version, ToLifecycle: to, Actor: "a", CorrelationID: "c"})
		require.NoError(t, err)
	}
	_, err = testStore.AdvanceRun(ctx, tenant, run2.RunID, store.AdvanceParams{
		ExpectedVersion: run2.Version, ToLifecycle: domain.LifecycleReadyToCertify, Actor: "a", CorrelationID: "c"})
	assert.ErrorIs(t, err, domain.ErrInvalidTransition, "NOT_EVALUATED is not certifiable")

	var failedEvents int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'control.run.failed'`, run.RunID).Scan(&failedEvents))
	assert.Equal(t, 1, failedEvents)
}

// Database-level backstop: even a direct write cannot mark a failed run as passed
// or certify without a passing result.
func TestRun_DatabaseCheckConstraintsBackstopTheStateMachine(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")
	run, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k", runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)

	_, err = testPool.Exec(ctx, `UPDATE control_runs SET lifecycle_state='FAILED', result_state='PASS' WHERE run_id = $1`, run.RunID)
	assert.Error(t, err, "FAILED + PASS violates the check constraint")
	_, err = testPool.Exec(ctx, `UPDATE control_runs SET lifecycle_state='CERTIFIED', certification_state='CERTIFIED', result_state='FAIL' WHERE run_id = $1`, run.RunID)
	assert.Error(t, err, "certifying a failed result violates the check constraint")
}

func TestRun_TenantIsolationAndPagination(t *testing.T) {
	a, b, entity := uuid.NewString(), uuid.NewString(), uuid.NewString()
	d := approvedDef(t, a, "FIN-CTRL-001")
	var ids []string
	for i := 0; i < 5; i++ {
		r, _, err := testStore.CreateRun(ctx, a, "prep", "c", fmt.Sprintf("k%d", i), runReq(d.ControlDefinitionID, entity))
		require.NoError(t, err)
		ids = append(ids, r.RunID)
	}
	_, err := testStore.GetRun(ctx, b, ids[0])
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = testStore.AdvanceRun(ctx, b, ids[0], store.AdvanceParams{ExpectedVersion: 1, ToLifecycle: domain.LifecyclePreparing, Actor: "x", CorrelationID: "c"})
	assert.ErrorIs(t, err, domain.ErrNotFound, "another tenant cannot move this run")
	trs, err := testStore.ListTransitions(ctx, b, ids[0])
	require.NoError(t, err)
	assert.Empty(t, trs)
	other, err := testStore.ListRuns(ctx, b, domain.ListRunsFilter{LegalEntityID: entity})
	require.NoError(t, err)
	assert.Empty(t, other)

	// Keyset pagination walks all runs exactly once.
	seen := map[string]bool{}
	f := domain.ListRunsFilter{LegalEntityID: entity, Limit: 2}
	for page := 0; page < 10; page++ {
		items, err := testStore.ListRuns(ctx, a, f)
		require.NoError(t, err)
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			assert.False(t, seen[it.RunID], "run repeated across pages")
			seen[it.RunID] = true
		}
		last := items[len(items)-1]
		f.AfterCreatedAt, f.AfterRunID = &last.CreatedAt, last.RunID
	}
	assert.Len(t, seen, 5)
}

func TestRun_PriorRunMustBelongToTheSameControl(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d1 := approvedDef(t, tenant, "FIN-CTRL-001")
	d2 := approvedDef(t, tenant, "FIN-CTRL-002")
	r1, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k1", runReq(d1.ControlDefinitionID, entity))
	require.NoError(t, err)

	req := runReq(d2.ControlDefinitionID, entity)
	req.PriorRunID, req.Reason = r1.RunID, "rerun"
	_, _, err = testStore.CreateRun(ctx, tenant, "prep", "c", "k2", req)
	assert.True(t, errors.Is(err, domain.ErrInvalidArgument))

	req = runReq(d1.ControlDefinitionID, entity)
	req.TriggerType, req.PriorRunID, req.Reason = "ON_DEMAND", r1.RunID, "audit rerun"
	r2, _, err := testStore.CreateRun(ctx, tenant, "prep", "c", "k3", req)
	require.NoError(t, err)
	require.NotNil(t, r2.PriorRunID)
	assert.Equal(t, r1.RunID, *r2.PriorRunID)
}
