package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
	"zoiko.io/workflow-svc/internal/store"
)

// tenantCtx returns a context carrying the tenant scope the store reads
// from in production (set by svcmiddleware.TenantContext from
// X-Tenant-Id). Required for every store call now that
// workflow_instances carries FORCE ROW LEVEL SECURITY — a bare
// context.Background() correctly matches no rows.
func tenantCtx(tenantID string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenantID)
}

const testTenantID = "00000000-0000-0000-0000-000000000001"

func getTestPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real PostgreSQL integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("failed to connect to TEST_DATABASE_URL: %v", err)
	}
	return pool
}

func setupTestDB(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS outbox_events CASCADE;")
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS form_submission_routes, form_submissions, form_definitions, quality_review_records, sign_offs, review_notes, review_assignments, review_scopes, workpaper_addenda, workpaper_evidence_links, workpaper_cross_references, workpaper_conclusions, workpaper_results, workpaper_procedures, workpapers, planned_procedures, assertion_links, risk_assessments, materiality_records, audit_plan_transitions, audit_plans, audit_engagement_transitions, audit_engagements, workflow_transitions, workflow_stages, workflow_instances CASCADE;")

	// Apply every *.up.sql in order, not two hardcoded filenames — a
	// migration added later must not be silently skipped by these tests
	// (this exact trap already bit jurisdiction-rules-svc once, see
	// docs/architecture/known-gaps.md).
	entries, err := os.ReadDir("../../deployments/migrations")
	if err != nil {
		t.Fatalf("failed to read migrations dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		sql, err := os.ReadFile("../../deployments/migrations/" + name)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to execute migration %s: %v", name, err)
		}
	}
}

func twoStageParams() domain.CreateWorkflowParams {
	return domain.CreateWorkflowParams{
		TenantID: "00000000-0000-0000-0000-000000000001", LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		WorkflowType: "PURCHASE_APPROVAL", InitiatedBy: "requester-1",
		Stages: []domain.CreateWorkflowStageInput{
			{ApproverPrincipalID: "approver-1"},
			{ApproverPrincipalID: "approver-2"},
		},
	}
}

func auditEngagementParams() domain.CreateAuditEngagementParams {
	return domain.CreateAuditEngagementParams{
		TenantID: testTenantID, LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		EngagementCode: "FY26-STAT", EngagementType: "STATUTORY_AUDIT",
		ReportingPeriodStart: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC),
		ReportingPeriodEnd:   time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
		FrameworkProfileID:   "isa", FrameworkProfileVersion: "2025.1",
		MethodologyID: "firm-method", MethodologyVersion: "2026.1",
		ResponsiblePartnerID: "partner-1", ScopeSummary: "Annual statutory audit",
		CreatedByPrincipalID: "manager-1",
	}
}

func TestPgStore_AuditEngagement_PinsEvidenceAndIsIdempotent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	p := auditEngagementParams()
	p.CorrelationID = "audit-create-1"
	created, wasCreated, err := s.CreateAuditEngagement(ctx, p)
	if err != nil || !wasCreated {
		t.Fatalf("create audit engagement: created=%v err=%v", wasCreated, err)
	}

	replayed, wasCreated, err := s.CreateAuditEngagement(ctx, p)
	if err != nil || wasCreated || replayed.EngagementID != created.EngagementID {
		t.Fatalf("create replay must return original engagement without a duplicate: created=%v replay=%+v original=%+v err=%v", wasCreated, replayed, created, err)
	}

	accepted, changed, err := s.SubmitAuditEngagementAcceptance(ctx, domain.SubmitAuditEngagementAcceptanceParams{
		EngagementID: created.EngagementID, TenantID: testTenantID,
		EvidenceDocumentID: "00000000-0000-0000-0000-0000000000d1", EvidenceDocumentVersion: 3,
		ActorPrincipalID: "manager-1", CorrelationID: "audit-acceptance-1",
	})
	if err != nil || !changed {
		t.Fatalf("submit acceptance: changed=%v err=%v", changed, err)
	}
	if accepted.Status != domain.AuditEngagementAcceptanceReview || accepted.AcceptanceDocumentID == nil || *accepted.AcceptanceDocumentID != "00000000-0000-0000-0000-0000000000d1" || accepted.AcceptanceDocumentVersion == nil || *accepted.AcceptanceDocumentVersion != 3 {
		t.Fatalf("acceptance evidence version was not pinned: %+v", accepted)
	}

	replayAcceptance, changed, err := s.SubmitAuditEngagementAcceptance(ctx, domain.SubmitAuditEngagementAcceptanceParams{
		EngagementID: created.EngagementID, TenantID: testTenantID,
		EvidenceDocumentID: "00000000-0000-0000-0000-0000000000d1", EvidenceDocumentVersion: 3,
		ActorPrincipalID: "manager-1", CorrelationID: "audit-acceptance-1",
	})
	if err != nil || changed || replayAcceptance.EngagementID != created.EngagementID {
		t.Fatalf("acceptance replay must be a no-op: changed=%v engagement=%+v err=%v", changed, replayAcceptance, err)
	}
}

// TestPgStore_CreateWorkflow_RetriedCorrelationID_IsIdempotent proves the
// exact scenario a network-timeout-triggered client retry produces: it must
// resolve to the original instance and stages, never create a duplicate
// workflow with its own duplicate stage chain (migration 000003).
func TestPgStore_CreateWorkflow_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	params := twoStageParams()
	params.CorrelationID = "corr-retry-1"

	instance1, stages1, created1, err := s.CreateWorkflow(ctx, params)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !created1 {
		t.Fatalf("expected created=true on the first call")
	}

	instance2, stages2, created2, err := s.CreateWorkflow(ctx, params)
	if err != nil {
		t.Fatalf("retried create: %v", err)
	}
	if created2 {
		t.Fatalf("expected created=false on the retried call — this is a duplicate-workflow bug if it's true")
	}
	if instance2.WorkflowInstanceID != instance1.WorkflowInstanceID {
		t.Fatalf("retried call must resolve to the original workflow_instance_id, got a different one")
	}
	if len(stages2) != len(stages1) {
		t.Fatalf("expected the same %d stages back, got %d", len(stages1), len(stages2))
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workflow_instances WHERE tenant_id = $1 AND correlation_id = $2`,
		params.TenantID, params.CorrelationID).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("DUPLICATE WORKFLOW: expected exactly 1 workflow_instances row for this correlation_id, got %d", count)
	}
}

func TestPgStore_CreateWorkflow_NoStages(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	params := twoStageParams()
	params.Stages = nil
	_, _, _, err := s.CreateWorkflow(tenantCtx(testTenantID), params)
	if !errors.Is(err, domain.ErrNoStages) {
		t.Fatalf("expected ErrNoStages, got %v", err)
	}
}

// appRolePool returns a pool connected as a genuine NOSUPERUSER
// NOBYPASSRLS role, mirroring the platform's real runtime role
// (zoiko_app — see docs/architecture/known-gaps.md's superuser-RLS-bypass
// writeup). This matters: TEST_DATABASE_URL normally points at
// `postgres`, and a SUPERUSER bypasses row-level security
// unconditionally, FORCE or not. A test that asserts isolation while
// connected as the superuser proves only that the application-level
// predicate works, never that the RLS policy does.
func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	// The DSN is rebuilt via net/url rather than string-replaced, and the
	// role's password is (re)set to a constant this test owns. An earlier
	// version of this helper hardcoded the role's password while replacing
	// only the USERNAME in the DSN, so it inherited whatever password
	// TEST_DATABASE_URL happened to carry. That passed locally purely
	// because the throwaway container's password matched the hardcoded one
	// by coincidence, and failed in CI where the password differs — the
	// "works on my machine" shape known-gaps.md documents repeatedly.
	// ALTER (not just CREATE) so a role left over from an earlier run with
	// a different password is corrected rather than reused.
	const appRole = "zoiko_app_test"
	const appPassword = "zoiko_app_test_pw"
	if _, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+appRole+`') THEN
			CREATE ROLE `+appRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`); err != nil {
		t.Fatalf("create role %s: %v", appRole, err)
	}
	for _, stmt := range []string{
		`ALTER ROLE ` + appRole + ` WITH LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + appRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appRole,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + appRole,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant to %s (%s): %v", appRole, stmt, err)
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
	// Prove the role really is RLS-bound before relying on it — a
	// misconfigured role that silently retained BYPASSRLS would make the
	// isolation assertion below pass for the wrong reason.
	var isSuper, bypassRLS bool
	if err := pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &bypassRLS); err != nil {
		t.Fatalf("verify %s privileges: %v", appRole, err)
	}
	if isSuper || bypassRLS {
		t.Fatalf("%s must be NOSUPERUSER and NOBYPASSRLS for this assertion to mean anything, got rolsuper=%v rolbypassrls=%v", appRole, isSuper, bypassRLS)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPgStore_TenantIsolation_FindWorkflowByID proves the real fix in
// this row: FindWorkflowByID is the choke point every other Store method
// routes through, and it previously fell back to an UNSCOPED lookup when
// no tenant was in context — so omitting X-Tenant-Id, the easier request
// to make, was strictly more permissive than supplying a real one (the
// document-vault-svc "filter that disables itself" shape).
//
// The no-tenant probe runs as a NOSUPERUSER NOBYPASSRLS role on purpose:
// as the superuser it passes for the wrong reason (RLS is skipped
// entirely), so it would prove nothing about the policy this row adds.
func TestPgStore_TenantIsolation_FindWorkflowByID(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	tenantA := testTenantID
	tenantB := "00000000-0000-0000-0000-0000000000b1"

	instance, _, _, err := s.CreateWorkflow(tenantCtx(tenantA), twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Probe: tenant B's context, tenant A's workflow ID. The application
	// predicate alone covers this one, so the superuser pool is fine here.
	if got, err := s.FindWorkflowByID(tenantCtx(tenantB), instance.WorkflowInstanceID); !errors.Is(err, domain.ErrWorkflowNotFound) {
		t.Fatalf("ISOLATION FAILURE: FindWorkflowByID returned tenant A's workflow under tenant B's context: %+v (err=%v)", got, err)
	}

	// Probe: NO tenant in context at all — the old unscoped-fallback path,
	// which the application predicate deliberately leaves open ($2 IS NULL)
	// and only the RLS policy closes. Must run as a non-superuser.
	appStore := store.New(appRolePool(t, pool), zap.NewNop())
	if got, err := appStore.FindWorkflowByID(context.Background(), instance.WorkflowInstanceID); !errors.Is(err, domain.ErrWorkflowNotFound) {
		t.Fatalf("ISOLATION FAILURE: FindWorkflowByID returned a workflow with no tenant scope in context: %+v (err=%v)", got, err)
	}

	// Sanity: the same non-superuser role CAN read tenant A's workflow when
	// the tenant scope is present — the policy must not over-restrict.
	own, err := appStore.FindWorkflowByID(tenantCtx(tenantA), instance.WorkflowInstanceID)
	if err != nil || own.WorkflowInstanceID != instance.WorkflowInstanceID {
		t.Fatalf("expected zoiko_app_test to read tenant A's workflow with the scope set, got %+v err=%v", own, err)
	}
}

// TestPgStore_TenantIsolation_MutationsRefuseForeignTenant proves the
// same boundary holds for every mutating path, all of which route
// through FindWorkflowByID first.
func TestPgStore_TenantIsolation_MutationsRefuseForeignTenant(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	tenantA := testTenantID
	tenantB := "00000000-0000-0000-0000-0000000000b1"
	ctxB := tenantCtx(tenantB)

	instance, _, _, err := s.CreateWorkflow(tenantCtx(tenantA), twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := s.SubmitAction(ctxB, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "APPROVE",
	}); !errors.Is(err, domain.ErrWorkflowNotFound) {
		t.Errorf("ISOLATION FAILURE: SubmitAction acted on tenant A's workflow under tenant B, err=%v", err)
	}
	if _, _, err := s.EscalateWorkflow(ctxB, instance.WorkflowInstanceID, "admin-b"); !errors.Is(err, domain.ErrWorkflowNotFound) {
		t.Errorf("ISOLATION FAILURE: EscalateWorkflow acted on tenant A's workflow under tenant B, err=%v", err)
	}
	if _, _, err := s.CancelWorkflow(ctxB, instance.WorkflowInstanceID, "admin-b"); !errors.Is(err, domain.ErrWorkflowNotFound) {
		t.Errorf("ISOLATION FAILURE: CancelWorkflow acted on tenant A's workflow under tenant B, err=%v", err)
	}

	// Verify tenant A's workflow is genuinely untouched: still PENDING at stage 1.
	own, err := s.FindWorkflowByID(tenantCtx(tenantA), instance.WorkflowInstanceID)
	if err != nil {
		t.Fatalf("reload tenant A's workflow: %v", err)
	}
	if own.WorkflowStatus != "PENDING" || own.CurrentStage != 1 {
		t.Fatalf("ISOLATION FAILURE: tenant B mutated tenant A's workflow — status=%s stage=%d", own.WorkflowStatus, own.CurrentStage)
	}
}

func TestPgStore_FullApprovalChain_CompletesWorkflow(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, stages, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if instance.WorkflowStatus != "PENDING" || instance.CurrentStage != 1 {
		t.Fatalf("expected PENDING at stage 1, got status=%s stage=%d", instance.WorkflowStatus, instance.CurrentStage)
	}
	if len(stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(stages))
	}

	current, err := s.FindCurrentStage(ctx, instance.WorkflowInstanceID)
	if err != nil {
		t.Fatalf("find current stage: %v", err)
	}
	if current.ApproverPrincipalID != "approver-1" {
		t.Fatalf("expected approver-1 first, got %s", current.ApproverPrincipalID)
	}

	// Stage 1 approve — advances to stage 2, workflow stays PENDING.
	updatedInstance, updatedStage, transitioned, err := s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("submit stage 1: %v", err)
	}
	if !transitioned || updatedStage.StageStatus != "APPROVED" {
		t.Fatalf("expected stage 1 approved, got %s transitioned=%v", updatedStage.StageStatus, transitioned)
	}
	if updatedInstance.WorkflowStatus != "PENDING" || updatedInstance.CurrentStage != 2 {
		t.Fatalf("expected still PENDING at stage 2, got status=%s stage=%d", updatedInstance.WorkflowStatus, updatedInstance.CurrentStage)
	}

	// Idempotent replay of stage 1 approval — no-op, not an error, not a
	// double-advance (doctrine requirement).
	_, _, transitioned, err = s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("idempotent replay should not error: %v", err)
	}
	if transitioned {
		t.Fatalf("expected idempotent no-op on replay, got transitioned=true")
	}

	// A principal with no stage anywhere in this workflow's chain is a
	// genuine wrong-approver case (distinct from approver-1 replaying
	// their own already-completed stage 1 action above, which is a
	// legitimate idempotent no-op, not an error).
	_, _, _, err = s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "stranger-1", Action: "APPROVE",
	})
	if !errors.Is(err, domain.ErrWrongApprover) {
		t.Fatalf("expected ErrWrongApprover, got %v", err)
	}

	// Stage 2 approve — final stage, completes the workflow.
	finalInstance, _, transitioned, err := s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-2", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("submit stage 2: %v", err)
	}
	if !transitioned || finalInstance.WorkflowStatus != "APPROVED" || finalInstance.CurrentStage != 0 {
		t.Fatalf("expected APPROVED and terminal, got status=%s stage=%d", finalInstance.WorkflowStatus, finalInstance.CurrentStage)
	}
	if finalInstance.CompletedAt == nil {
		t.Errorf("expected completed_at to be stamped")
	}

	// No further action possible on a terminal workflow.
	_, _, _, err = s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-2", Action: "APPROVE",
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition on terminal workflow, got %v", err)
	}
}

func TestPgStore_RejectionEndsWorkflowImmediately(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updatedInstance, updatedStage, transitioned, err := s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "REJECT",
	})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if !transitioned || updatedStage.StageStatus != "REJECTED" {
		t.Fatalf("expected stage rejected, got %s", updatedStage.StageStatus)
	}
	if updatedInstance.WorkflowStatus != "REJECTED" || updatedInstance.CurrentStage != 0 {
		t.Fatalf("expected workflow REJECTED and terminal after stage 1 rejection, got status=%s stage=%d", updatedInstance.WorkflowStatus, updatedInstance.CurrentStage)
	}
}

func TestPgStore_ConflictingResubmission_NotIdempotent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "REJECT",
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// Workflow is now terminal (REJECTED) — any further action, even from
	// the same approver, is an invalid transition, not a "different
	// outcome" idempotency conflict at the stage level (the instance-level
	// terminal check fires first).
	_, _, _, err = s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "APPROVE",
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
}

func TestPgStore_EscalateAndCancel(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	escalated, transitioned, err := s.EscalateWorkflow(ctx, instance.WorkflowInstanceID, "admin-1")
	if err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if !transitioned || escalated.WorkflowStatus != "ESCALATED" {
		t.Fatalf("expected ESCALATED, got %s", escalated.WorkflowStatus)
	}

	// Idempotent replay.
	_, transitioned, err = s.EscalateWorkflow(ctx, instance.WorkflowInstanceID, "admin-1")
	if err != nil {
		t.Fatalf("idempotent escalate: %v", err)
	}
	if transitioned {
		t.Fatalf("expected idempotent no-op on re-escalate")
	}

	cancelled, transitioned, err := s.CancelWorkflow(ctx, instance.WorkflowInstanceID, "admin-1")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !transitioned || cancelled.WorkflowStatus != "CANCELLED" {
		t.Fatalf("expected CANCELLED (from ESCALATED), got %s", cancelled.WorkflowStatus)
	}

	// Cancelling a terminal (CANCELLED) workflow again should be idempotent, not an error.
	_, transitioned, err = s.CancelWorkflow(ctx, instance.WorkflowInstanceID, "admin-1")
	if err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
	if transitioned {
		t.Fatalf("expected idempotent no-op on re-cancel")
	}
}

func TestPgStore_CancelApprovedWorkflow_Illegal(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	params := twoStageParams()
	params.Stages = []domain.CreateWorkflowStageInput{{ApproverPrincipalID: "approver-1"}}
	instance, _, _, err := s.CreateWorkflow(ctx, params)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "APPROVE",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	_, _, err = s.CancelWorkflow(ctx, instance.WorkflowInstanceID, "admin-1")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition cancelling an APPROVED workflow, got %v", err)
	}
}

func makeTestSubjectFingerprint() string {
	b := svcenvelope.NewFingerprintBuilder()
	b.Set("journal_id", "j-101")
	b.SetAmount("total_debit", 4500.00, "USD")
	return b.Build()
}

func TestPgStore_SubjectBinding_Persistence(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	fp := makeTestSubjectFingerprint()
	subjType := "JOURNAL"
	subjID := "j-101"
	subjVer := 2

	p := twoStageParams()
	p.SubjectType = &subjType
	p.SubjectID = &subjID
	p.SubjectVersion = &subjVer
	p.SubjectFingerprint = &fp

	created, _, wasCreated, err := s.CreateWorkflow(ctx, p)
	if err != nil || !wasCreated {
		t.Fatalf("create workflow with subject: wasCreated=%v, err=%v", wasCreated, err)
	}

	loaded, err := s.FindWorkflowByID(ctx, created.WorkflowInstanceID)
	if err != nil {
		t.Fatalf("find workflow: %v", err)
	}

	if loaded.SubjectType == nil || *loaded.SubjectType != subjType {
		t.Errorf("expected subject_type=%s, got %v", subjType, loaded.SubjectType)
	}
	if loaded.SubjectID == nil || *loaded.SubjectID != subjID {
		t.Errorf("expected subject_id=%s, got %v", subjID, loaded.SubjectID)
	}
	if loaded.SubjectVersion == nil || *loaded.SubjectVersion != subjVer {
		t.Errorf("expected subject_version=%d, got %v", subjVer, loaded.SubjectVersion)
	}
	if loaded.SubjectFingerprint == nil || *loaded.SubjectFingerprint != fp {
		t.Errorf("expected subject_fingerprint=%s, got %v", fp, loaded.SubjectFingerprint)
	}
}

func TestPgStore_InvalidateWorkflow_PendingAndApproved(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	// 1. Invalidate while PENDING
	fp := makeTestSubjectFingerprint()
	subjType := "SUPPLIER_INVOICE"
	subjID := "inv-99"
	subjVer := 1

	p := twoStageParams()
	p.SubjectType = &subjType
	p.SubjectID = &subjID
	p.SubjectVersion = &subjVer
	p.SubjectFingerprint = &fp

	instance, _, _, err := s.CreateWorkflow(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	narrative := "Vendor destination account modified"
	invalidated, transitioned, err := s.InvalidateWorkflow(ctx, domain.InvalidateWorkflowParams{
		WorkflowInstanceID: instance.WorkflowInstanceID,
		TenantID:           testTenantID,
		ActorPrincipalID:   "fraud-detector-1",
		ReasonCode:         "CONTROL_FAILURE",
		Narrative:          &narrative,
		EvidenceRefs:       []string{"doc-audit-1"},
	})
	if err != nil || !transitioned {
		t.Fatalf("invalidate: transitioned=%v, err=%v", transitioned, err)
	}
	if invalidated.WorkflowStatus != domain.WorkflowStatusInvalidated {
		t.Fatalf("expected status INVALIDATED, got %s", invalidated.WorkflowStatus)
	}
	if invalidated.InvalidatedAt == nil {
		t.Errorf("expected invalidated_at stamped")
	}
	if invalidated.InvalidationReasonCode == nil || *invalidated.InvalidationReasonCode != "CONTROL_FAILURE" {
		t.Errorf("expected reason code CONTROL_FAILURE, got %v", invalidated.InvalidationReasonCode)
	}
	if invalidated.CurrentStage != 0 {
		t.Errorf("expected current_stage=0 for terminal invalidated workflow, got %d", invalidated.CurrentStage)
	}

	// Idempotent replay of invalidation
	_, transitioned, err = s.InvalidateWorkflow(ctx, domain.InvalidateWorkflowParams{
		WorkflowInstanceID: instance.WorkflowInstanceID,
		TenantID:           testTenantID,
		ActorPrincipalID:   "fraud-detector-1",
		ReasonCode:         "CONTROL_FAILURE",
	})
	if err != nil {
		t.Fatalf("replay invalidate: %v", err)
	}
	if transitioned {
		t.Errorf("expected idempotent no-op on replay, got transitioned=true")
	}

	// 2. Invalidate an already-APPROVED workflow (material edit after approval)
	p2 := twoStageParams()
	p2.Stages = []domain.CreateWorkflowStageInput{{ApproverPrincipalID: "approver-1"}}
	p2.SubjectFingerprint = &fp
	apprInstance, _, _, err := s.CreateWorkflow(ctx, p2)
	if err != nil {
		t.Fatalf("create single-stage: %v", err)
	}

	_, _, _, err = s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: apprInstance.WorkflowInstanceID,
		ActorPrincipalID:   "approver-1",
		Action:             "APPROVE",
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Now invalidate the approved workflow
	invAppr, transitioned, err := s.InvalidateWorkflow(ctx, domain.InvalidateWorkflowParams{
		WorkflowInstanceID: apprInstance.WorkflowInstanceID,
		TenantID:           testTenantID,
		ActorPrincipalID:   "system-policy-evaluator",
		ReasonCode:         "POLICY_NOT_MET",
	})
	if err != nil || !transitioned {
		t.Fatalf("invalidate approved: transitioned=%v, err=%v", transitioned, err)
	}
	if invAppr.WorkflowStatus != domain.WorkflowStatusInvalidated {
		t.Fatalf("expected status INVALIDATED from APPROVED, got %s", invAppr.WorkflowStatus)
	}

	// 3. Cannot invalidate a CANCELLED workflow
	cancInstance, _, _, _ := s.CreateWorkflow(ctx, twoStageParams())
	_, _, _ = s.CancelWorkflow(ctx, cancInstance.WorkflowInstanceID, "admin-1")
	_, _, err = s.InvalidateWorkflow(ctx, domain.InvalidateWorkflowParams{
		WorkflowInstanceID: cancInstance.WorkflowInstanceID,
		TenantID:           testTenantID,
		ActorPrincipalID:   "admin-1",
		ReasonCode:         "CONTROL_FAILURE",
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition invalidating cancelled workflow, got %v", err)
	}
}

func TestPgStore_VerifyRelease_Comprehensive(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	fp := makeTestSubjectFingerprint()
	subjType := "PAYMENT_PROPOSAL"
	subjID := "prop-42"
	subjVer := 1

	// Create and fully approve a 1-stage workflow
	p := twoStageParams()
	p.Stages = []domain.CreateWorkflowStageInput{{ApproverPrincipalID: "approver-1"}}
	p.SubjectType = &subjType
	p.SubjectID = &subjID
	p.SubjectVersion = &subjVer
	p.SubjectFingerprint = &fp

	instance, _, _, err := s.CreateWorkflow(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. Not approved yet -> verification fails
	res, err := s.VerifyRelease(ctx, domain.VerifyReleaseParams{
		WorkflowInstanceID:        instance.WorkflowInstanceID,
		TenantID:                  testTenantID,
		CurrentSubjectFingerprint: fp,
	})
	if err != nil {
		t.Fatalf("verify release: %v", err)
	}
	if res.CanRelease || res.Status != "INVALID" {
		t.Errorf("expected cannot release while PENDING, got %+v", res)
	}

	// Approve the workflow
	_, _, _, err = s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID,
		ActorPrincipalID:   "approver-1",
		Action:             "APPROVE",
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// 2. Matching fingerprint and version -> can release!
	res, err = s.VerifyRelease(ctx, domain.VerifyReleaseParams{
		WorkflowInstanceID:        instance.WorkflowInstanceID,
		TenantID:                  testTenantID,
		ExpectedSubjectVersion:    &subjVer,
		CurrentSubjectFingerprint: fp,
	})
	if err != nil {
		t.Fatalf("verify release: %v", err)
	}
	if !res.CanRelease || res.Status != "VALID" {
		t.Errorf("expected CanRelease=true, got %+v", res)
	}

	// 3. Fingerprint mismatch (material change!) -> verification blocked
	alteredFp := svcenvelope.NewFingerprintBuilder().Set("journal_id", "j-101").SetAmount("total_debit", 9999.00, "USD").Build()
	res, err = s.VerifyRelease(ctx, domain.VerifyReleaseParams{
		WorkflowInstanceID:        instance.WorkflowInstanceID,
		TenantID:                  testTenantID,
		CurrentSubjectFingerprint: alteredFp,
	})
	if err != nil {
		t.Fatalf("verify release: %v", err)
	}
	if res.CanRelease || res.Status != "INVALID" {
		t.Errorf("expected CanRelease=false on fingerprint mismatch, got %+v", res)
	}

	// 4. Version mismatch -> verification blocked
	differentVer := 2
	res, err = s.VerifyRelease(ctx, domain.VerifyReleaseParams{
		WorkflowInstanceID:        instance.WorkflowInstanceID,
		TenantID:                  testTenantID,
		ExpectedSubjectVersion:    &differentVer,
		CurrentSubjectFingerprint: fp,
	})
	if err != nil {
		t.Fatalf("verify release: %v", err)
	}
	if res.CanRelease || res.Status != "INVALID" {
		t.Errorf("expected CanRelease=false on version mismatch, got %+v", res)
	}

	// 5. Invalidate workflow -> verification blocked
	_, _, err = s.InvalidateWorkflow(ctx, domain.InvalidateWorkflowParams{
		WorkflowInstanceID: instance.WorkflowInstanceID,
		TenantID:           testTenantID,
		ActorPrincipalID:   "security-officer",
		ReasonCode:         "CONTROL_FAILURE",
	})
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	res, err = s.VerifyRelease(ctx, domain.VerifyReleaseParams{
		WorkflowInstanceID:        instance.WorkflowInstanceID,
		TenantID:                  testTenantID,
		CurrentSubjectFingerprint: fp,
	})
	if err != nil {
		t.Fatalf("verify release: %v", err)
	}
	if res.CanRelease || res.Status != "INVALID" {
		t.Errorf("expected CanRelease=false on invalidated workflow, got %+v", res)
	}
}

func TestPgStore_VerifyRelease_LegacyUnboundWorkflow(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	// Create legacy workflow without subject fingerprint
	p := twoStageParams()
	p.Stages = []domain.CreateWorkflowStageInput{{ApproverPrincipalID: "approver-1"}}
	instance, _, _, err := s.CreateWorkflow(ctx, p)
	if err != nil {
		t.Fatalf("create legacy: %v", err)
	}

	_, _, _, _ = s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID,
		ActorPrincipalID:   "approver-1",
		Action:             "APPROVE",
	})

	// Verify release on unbound workflow must fail safely
	res, err := s.VerifyRelease(ctx, domain.VerifyReleaseParams{
		WorkflowInstanceID:        instance.WorkflowInstanceID,
		TenantID:                  testTenantID,
		CurrentSubjectFingerprint: makeTestSubjectFingerprint(),
	})
	if err != nil {
		t.Fatalf("verify release on legacy: %v", err)
	}
	if res.CanRelease || res.Status != "INVALID" {
		t.Errorf("expected unbound workflow to fail release verification safely, got %+v", res)
	}
}

func TestPgStore_Outbox_PersistenceAndAtomicity(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	// 1. CreateWorkflow inserts outbox event workflow.started in same transaction
	p := twoStageParams()
	p.Stages = []domain.CreateWorkflowStageInput{{ApproverPrincipalID: "approver-1"}}
	instance, _, _, err := s.CreateWorkflow(ctx, p)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	var startedCount int
	var outboxEventID string
	var eventType string
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(outbox_event_id::TEXT), ''), COALESCE(MAX(event_type), '')
		FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'workflow.started'
	`, instance.WorkflowInstanceID).Scan(&startedCount, &outboxEventID, &eventType)
	if err != nil {
		t.Fatalf("query outbox for workflow.started: %v", err)
	}
	if startedCount != 1 {
		t.Fatalf("expected 1 outbox event for workflow.started, got %d", startedCount)
	}
	if outboxEventID == "" {
		t.Fatalf("expected non-empty outbox_event_id")
	}

	// 2. SubmitAction inserts approval.granted and workflow.completed (since 1-stage final)
	_, _, _, err = s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID,
		ActorPrincipalID:   "approver-1",
		Action:             "APPROVE",
	})
	if err != nil {
		t.Fatalf("submit action: %v", err)
	}

	var grantedCount, completedCount int
	err = pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE event_type = 'approval.granted'),
			COUNT(*) FILTER (WHERE event_type = 'workflow.completed')
		FROM outbox_events
		WHERE aggregate_id = $1
	`, instance.WorkflowInstanceID).Scan(&grantedCount, &completedCount)
	if err != nil {
		t.Fatalf("query outbox for approval actions: %v", err)
	}
	if grantedCount != 1 {
		t.Errorf("expected 1 outbox approval.granted, got %d", grantedCount)
	}
	if completedCount != 1 {
		t.Errorf("expected 1 outbox workflow.completed, got %d", completedCount)
	}

	// 3. InvalidateWorkflow inserts workflow.approval.invalidated
	_, transitioned, err := s.InvalidateWorkflow(ctx, domain.InvalidateWorkflowParams{
		WorkflowInstanceID: instance.WorkflowInstanceID,
		TenantID:           testTenantID,
		ActorPrincipalID:   "admin-1",
		ReasonCode:         "CONTROL_FAILURE",
	})
	if err != nil || !transitioned {
		t.Fatalf("invalidate workflow: err=%v transitioned=%v", err, transitioned)
	}

	var invalidatedCount int
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'workflow.approval.invalidated'
	`, instance.WorkflowInstanceID).Scan(&invalidatedCount)
	if err != nil {
		t.Fatalf("query outbox for invalidation: %v", err)
	}
	if invalidatedCount != 1 {
		t.Errorf("expected 1 outbox workflow.approval.invalidated, got %d", invalidatedCount)
	}
}
