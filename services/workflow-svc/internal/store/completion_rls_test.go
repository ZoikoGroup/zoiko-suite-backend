package store_test

// Governance audit, 21 Oct 2026: run as the runtime role. Every test below
// uses appRolePool (NOSUPERUSER NOBYPASSRLS), because the defect this pass
// found first only exists there: after 000017 put FORCE RLS on the stage,
// quorum and transition tables, the stage reads still used the bare pool, so
// as zoiko_app an instance had no stages, no approver could approve, and a
// same-key retry answered 409. As the superuser every test passed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

func meta(key, hash string) domain.CommandMeta {
	return domain.CommandMeta{IdempotencyKey: key, RequestHash: hash, CorrelationID: "corr-rls", DecisionID: uuid.NewString()}
}

func TestPgStore_RLS_ApprovalChainCompletesAsTheRuntimeRole(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(appRolePool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	inst, stages, created, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if len(stages) != 2 {
		t.Fatalf("stages = %d", len(stages))
	}
	read, err := s.FindStagesByWorkflowID(ctx, inst.WorkflowInstanceID)
	if err != nil || len(read) != 2 {
		t.Fatalf("stages invisible to the runtime role: %d, %v", len(read), err)
	}
	cur, err := s.FindCurrentStage(ctx, inst.WorkflowInstanceID)
	if err != nil || cur.ApproverPrincipalID != "approver-1" {
		t.Fatalf("current stage: %+v, %v", cur, err)
	}

	authority := domain.ActionAuthority{AccessDecisionID: "ad-1", Basis: "rbac:role=APPROVER"}
	m1 := meta("idem-approve-1", "h1")
	_, _, moved, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "approver-1", Action: "APPROVE", Authority: authority, Meta: m1})
	if err != nil || !moved {
		t.Fatalf("stage 1 approve as runtime role: moved=%v err=%v", moved, err)
	}
	// Same key, same body: a replay, not a 409.
	if _, _, moved, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "approver-1", Action: "APPROVE", Meta: meta("idem-approve-1", "h1")}); err != nil || moved {
		t.Fatalf("same-key retry: moved=%v err=%v, want a replay", moved, err)
	}
	// Same key, different body: refused.
	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "approver-1", Action: "REJECT", Meta: meta("idem-approve-1", "h2")}); !errors.Is(err, domain.ErrIdempotencyMismatch) {
		t.Fatalf("same key, different body: %v", err)
	}
	final, _, moved, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "approver-2", Action: "APPROVE", Authority: authority, Meta: meta("idem-approve-2", "h3")})
	if err != nil || !moved || final.WorkflowStatus != "APPROVED" {
		t.Fatalf("final approve: %+v moved=%v err=%v", final, moved, err)
	}

	// Each decision wrote its ledger record to the outbox in the same
	// transaction, under the decision id the response carries.
	var ledger int
	if err := admin.QueryRow(context.Background(), `SELECT COUNT(*) FROM outbox_events
		WHERE aggregate_id = $1 AND destination = 'decision_log' AND outbox_event_id::text = $2`,
		inst.WorkflowInstanceID, m1.DecisionID).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger record for decision %s: %d, %v", m1.DecisionID, ledger, err)
	}
	// The transition keeps the authority snapshot (R-001 lineage).
	evidence, err := s.GetApprovalEvidence(ctx, inst.WorkflowInstanceID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	found := false
	for _, tr := range evidence {
		if tr.AccessDecisionID != nil && *tr.AccessDecisionID == "ad-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no transition carries access_decision_id ad-1: %+v", evidence)
	}
}

// Row 36: the transition log is append-only for the runtime role.
func TestPgStore_RLS_TransitionsAreAppendOnly(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	app := appRolePool(t, admin)
	s := store.New(app, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "approver-1", Action: "APPROVE", Meta: meta("", "")}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE workflow_transitions SET acted_by = 'forged' WHERE workflow_instance_id = $1`,
		`DELETE FROM workflow_transitions WHERE workflow_instance_id = $1`,
	} {
		tx, err := app.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", testTenantID)
		_, err = tx.Exec(context.Background(), stmt, inst.WorkflowInstanceID)
		_ = tx.Rollback(context.Background())
		if err == nil {
			t.Fatalf("%s succeeded on the append-only log", stmt)
		}
	}
}

// Rows 27-28: escalation keeps the stage actionable, and the escalation
// target can decide it.
func TestPgStore_RLS_EscalationTargetCanDecide(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(appRolePool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatal(err)
	}
	esc, _, err := s.EscalateToWorkflow(ctx, domain.EscalateToWorkflowParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "admin-1", EscalationTarget: "head-of-finance", ReasonCode: "SLA_BREACH", Meta: meta("", "")})
	if err != nil {
		t.Fatalf("escalate-to: %v", err)
	}
	if esc.WorkflowStatus != domain.WorkflowStatusEscalated || esc.CurrentStage != 1 {
		t.Fatalf("escalated instance = status %s stage %d, want ESCALATED at stage 1", esc.WorkflowStatus, esc.CurrentStage)
	}
	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "someone-else", Action: "APPROVE", ActingAs: "escalation", Meta: meta("", "")}); !errors.Is(err, domain.ErrNotEscalationOwner) {
		t.Fatalf("a non-target acted on the escalation: %v", err)
	}
	if _, _, moved, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "head-of-finance", Action: "APPROVE", ActingAs: "escalation", Meta: meta("", "")}); err != nil || !moved {
		t.Fatalf("escalation target approve: moved=%v err=%v", moved, err)
	}
}

// The timer pass lists due instances across tenants as the runtime role
// (000020's app.timer_sweep policy), and only while the GUC is set.
func TestPgStore_RLS_TimerSweepSeesDueInstancesAcrossTenants(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(appRolePool(t, admin), zap.NewNop())

	past := time.Now().Add(-time.Hour)
	const otherTenant = "00000000-0000-0000-0000-000000000002"
	var ids []string
	for _, tenant := range []string{testTenantID, otherTenant} {
		p := twoStageParams()
		p.TenantID = tenant
		p.DueAt = &past
		inst, _, _, err := s.CreateWorkflow(tenantCtx(tenant), p)
		if err != nil {
			t.Fatalf("create in %s: %v", tenant, err)
		}
		ids = append(ids, inst.WorkflowInstanceID)
	}
	due, err := s.ListDueInstances(context.Background(), time.Now(), 50)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	seen := map[string]string{}
	for _, d := range due {
		seen[d.WorkflowInstanceID] = d.TenantID
	}
	if seen[ids[0]] != testTenantID || seen[ids[1]] != otherTenant {
		t.Fatalf("due = %v, want both tenants' instances with their tenants", seen)
	}

	// The expiry itself runs under the instance's own tenant.
	exp, moved, err := s.ExpireWorkflow(tenantCtx(otherTenant), ids[1], domain.LifecycleCommand{ReasonCode: "DEADLINE_PASSED", Meta: meta("", "")})
	if err != nil || !moved || exp.WorkflowStatus != domain.WorkflowStatusExpired {
		t.Fatalf("expire: %+v moved=%v err=%v", exp, moved, err)
	}
	// And a tenant cannot expire another tenant's instance.
	if _, _, err := s.ExpireWorkflow(tenantCtx(otherTenant), ids[0]); !errors.Is(err, domain.ErrWorkflowNotFound) {
		t.Fatalf("cross-tenant expire: %v", err)
	}
}

// Row 48: an authority change flags the pending instances the principal can
// decide, the re-check lists who to re-authorize, and clearing keeps a flag
// raised after the check began.
func TestPgStore_RLS_ApproverRecheckFlagsAndClears(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(appRolePool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.MarkApproverRecheck(ctx, testTenantID, "approver-2"); err != nil || n != 0 {
		t.Fatalf("approver-2 is not on the current stage yet: flagged %d, %v", n, err)
	}
	if n, err := s.MarkApproverRecheck(ctx, testTenantID, "approver-1"); err != nil || n != 1 {
		t.Fatalf("flag approver-1: %d, %v", n, err)
	}
	approvers, err := s.ListCurrentApprovers(ctx, inst.WorkflowInstanceID)
	if err != nil || len(approvers) != 1 || approvers[0].ApproverPrincipalID != "approver-1" {
		t.Fatalf("current approvers: %+v, %v", approvers, err)
	}
	due, err := s.ListDueInstances(context.Background(), time.Now(), 50)
	if err != nil || len(due) != 1 || due[0].RecheckAt == nil {
		t.Fatalf("flagged instance not due for re-check: %+v, %v", due, err)
	}
	if err := s.ClearApproverRecheck(ctx, inst.WorkflowInstanceID, due[0].RecheckAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.ListDueInstances(context.Background(), time.Now(), 50); len(due) != 1 {
		t.Fatal("a flag raised after the check began was cleared")
	}
	if err := s.ClearApproverRecheck(ctx, inst.WorkflowInstanceID, *due[0].RecheckAt); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.ListDueInstances(context.Background(), time.Now(), 50); len(due) != 0 {
		t.Fatalf("flag not cleared: %+v", due)
	}
}

func TestPgStore_RLS_ConsumedEventsApplyOnce(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(appRolePool(t, admin), zap.NewNop())
	ctx := context.Background()

	id := "evt-" + uuid.NewString()
	if first, err := s.RecordConsumedEvent(ctx, id, "policy.updated", testTenantID); err != nil || !first {
		t.Fatalf("first delivery: %v %v", first, err)
	}
	if first, err := s.RecordConsumedEvent(ctx, id, "policy.updated", testTenantID); err != nil || first {
		t.Fatalf("redelivery claimed again: %v %v", first, err)
	}
	if err := s.ForgetConsumedEvent(ctx, id); err != nil {
		t.Fatal(err)
	}
	if first, err := s.RecordConsumedEvent(ctx, id, "policy.updated", testTenantID); err != nil || !first {
		t.Fatalf("released claim not reclaimable: %v %v", first, err)
	}
}

// A quorum approver who already voted in this review cycle is off the
// re-check list: re-authorizing them could only escalate a stage their vote
// no longer depends on.
func TestPgStore_RLS_RecheckListsOnlyQuorumApproversStillToVote(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(appRolePool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	inst, _, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "voter-1", Action: "APPROVE", Meta: meta("", "")}); err != nil {
		t.Fatal(err)
	}
	approvers, err := s.ListCurrentApprovers(ctx, inst.WorkflowInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, a := range approvers {
		got[a.ApproverPrincipalID] = true
	}
	if len(approvers) != 2 || got["voter-1"] || !got["voter-2"] || !got["voter-3"] {
		t.Fatalf("approvers still to vote = %+v, want voter-2 and voter-3", approvers)
	}
}

func TestPgStore_RLS_ListTenantsWithPendingCrossesTenants(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(appRolePool(t, admin), zap.NewNop())

	const otherTenant = "00000000-0000-0000-0000-000000000002"
	for _, tenant := range []string{testTenantID, otherTenant} {
		p := twoStageParams()
		p.TenantID = tenant
		if _, _, _, err := s.CreateWorkflow(tenantCtx(tenant), p); err != nil {
			t.Fatal(err)
		}
	}
	tenants, err := s.ListTenantsWithPending(context.Background())
	if err != nil || len(tenants) != 2 {
		t.Fatalf("tenants = %v, %v", tenants, err)
	}
}
