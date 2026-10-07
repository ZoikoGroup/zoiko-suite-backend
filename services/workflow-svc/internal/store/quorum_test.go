package store_test

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

// ZS-SVC-R-001 §5.1/§8.3 — quorum / N-of-M dual-control approval,
// against real Postgres. Confirmed via repo-wide search before this
// was built: zero prior implementation anywhere in this platform.

func quorumOnlyParams(pool int, required int) domain.CreateWorkflowParams {
	approvers := []string{"voter-1", "voter-2", "voter-3", "voter-4"}[:pool]
	return domain.CreateWorkflowParams{
		TenantID: testTenantID, LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		WorkflowType: "BUDGET_APPROVAL", InitiatedBy: "requester-1",
		Stages: []domain.CreateWorkflowStageInput{
			{StageType: domain.StageTypeQuorum, RequiredApprovals: required, QuorumApprovers: approvers},
		},
	}
}

func TestPgStore_Quorum_CreateStage_PersistsShape(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, stages, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(stages) != 1 || stages[0].StageType != domain.StageTypeQuorum {
		t.Fatalf("expected one QUORUM stage, got %+v", stages)
	}
	if stages[0].RequiredApprovals == nil || *stages[0].RequiredApprovals != 2 {
		t.Fatalf("expected required_approvals=2, got %+v", stages[0].RequiredApprovals)
	}
	if stages[0].ApproverPrincipalID != "" {
		t.Fatalf("a QUORUM stage must have no single approver, got %q", stages[0].ApproverPrincipalID)
	}
	if instance.WorkflowStatus != "PENDING" {
		t.Fatalf("expected PENDING, got %s", instance.WorkflowStatus)
	}
}

// Core happy path: 2-of-3 reached after exactly two APPROVE votes, and
// since this is the only stage, the workflow completes.
func TestPgStore_Quorum_ReachedAfterTwoApprovals_CompletesWorkflow(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, _, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, stage, transitioned, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	if !transitioned || stage.StageStatus != "PENDING" {
		t.Fatalf("after 1/2 approvals, stage should still be PENDING, got %+v", stage)
	}

	finalInstance, finalStage, transitioned, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-2", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("vote 2: %v", err)
	}
	if !transitioned || finalStage.StageStatus != "APPROVED" {
		t.Fatalf("after 2/2 required approvals, stage should be APPROVED, got %+v", finalStage)
	}
	if finalInstance.WorkflowStatus != "APPROVED" || finalInstance.CurrentStage != 0 {
		t.Fatalf("expected workflow APPROVED and terminal, got status=%s stage=%d", finalInstance.WorkflowStatus, finalInstance.CurrentStage)
	}
}

// Quorum is about reaching enough support, not unanimity — a dissent
// does not fail the stage by itself; it only fails once reaching the
// threshold becomes mathematically impossible.
func TestPgStore_Quorum_SingleDissentDoesNotFailStage(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, _, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, stage, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "REJECT",
	})
	if err != nil {
		t.Fatalf("vote 1 (reject): %v", err)
	}
	// 1 reject, 2 remaining voters, 0 approvals so far: 0+2 >= 2, still reachable.
	if stage.StageStatus != "PENDING" {
		t.Fatalf("one dissent out of three must not fail the stage, got %+v", stage)
	}

	_, finalStage, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-2", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("vote 2 (approve): %v", err)
	}
	if finalStage.StageStatus != "PENDING" {
		t.Fatalf("still only 1/2 approvals, expected PENDING, got %+v", finalStage)
	}

	_, resolvedStage, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-3", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("vote 3 (approve): %v", err)
	}
	if resolvedStage.StageStatus != "APPROVED" {
		t.Fatalf("2/3 approvals despite one earlier dissent should reach quorum, got %+v", resolvedStage)
	}
}

// The stage fails as soon as reaching the threshold becomes
// mathematically impossible — not merely on the first dissent.
func TestPgStore_Quorum_FailsWhenMathematicallyImpossible(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, _, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "REJECT",
	}); err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	// 2 rejects: 0 approvals so far, 1 remaining voter, 0+1 < 2 — impossible.
	finalInstance, finalStage, transitioned, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-2", Action: "REJECT",
	})
	if err != nil {
		t.Fatalf("vote 2: %v", err)
	}
	if !transitioned || finalStage.StageStatus != "REJECTED" {
		t.Fatalf("2/3 rejects with threshold 2 must fail the stage once impossible, got %+v", finalStage)
	}
	if finalInstance.WorkflowStatus != "REJECTED" {
		t.Fatalf("expected workflow REJECTED, got %s", finalInstance.WorkflowStatus)
	}
}

func TestPgStore_Quorum_NotEligibleApprover_Refused(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, _, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, _, _, err = s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "stranger-1", Action: "APPROVE",
	})
	if !errors.Is(err, domain.ErrNotEligibleQuorumApprover) {
		t.Fatalf("expected ErrNotEligibleQuorumApprover, got %v", err)
	}
}

// NP-09's concurrency-safe double-count protection: a replay of the
// identical vote is an idempotent no-op; a different vote from the
// same approver is a real conflict, never a silent overwrite.
func TestPgStore_Quorum_DoubleVote(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, _, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "APPROVE",
	}); err != nil {
		t.Fatalf("vote 1: %v", err)
	}

	// Identical replay — idempotent no-op.
	_, _, transitioned, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("replay should not error: %v", err)
	}
	if transitioned {
		t.Fatalf("identical replay must be a no-op, got transitioned=true")
	}

	// Changed vote from the same approver — real conflict.
	_, _, _, err = s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "REJECT",
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a changed vote from the same approver must be refused, got %v", err)
	}
}

// A mixed chain — SINGLE stage then QUORUM stage — proves the two
// shapes coexist in the same workflow without interfering.
func TestPgStore_Quorum_MixedChainWithSingleStage(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	params := domain.CreateWorkflowParams{
		TenantID: testTenantID, LegalEntityID: "00000000-0000-0000-0000-0000000000e1",
		WorkflowType: "BUDGET_APPROVAL", InitiatedBy: "requester-1",
		Stages: []domain.CreateWorkflowStageInput{
			{ApproverPrincipalID: "approver-1"},
			{StageType: domain.StageTypeQuorum, RequiredApprovals: 2, QuorumApprovers: []string{"voter-1", "voter-2", "voter-3"}},
		},
	}
	instance, stages, _, err := s.CreateWorkflow(ctx, params)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if stages[0].StageType != domain.StageTypeSingle || stages[1].StageType != domain.StageTypeQuorum {
		t.Fatalf("expected [SINGLE, QUORUM], got %+v", stages)
	}

	// A quorum vote cannot jump ahead of stage 1.
	if _, _, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "APPROVE",
	}); !errors.Is(err, domain.ErrNotQuorumStage) {
		t.Fatalf("expected ErrNotQuorumStage while stage 1 (SINGLE) is current, got %v", err)
	}

	advanced, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("submit stage 1: %v", err)
	}
	if advanced.CurrentStage != 2 {
		t.Fatalf("expected to advance to stage 2, got %d", advanced.CurrentStage)
	}

	// Now a SINGLE-stage action is the wrong shape for the current stage.
	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "APPROVE",
	}); !errors.Is(err, domain.ErrWrongApprover) {
		t.Fatalf("expected ErrWrongApprover submitting a SINGLE action against a QUORUM stage, got %v", err)
	}

	if _, _, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-1", Action: "APPROVE",
	}); err != nil {
		t.Fatalf("quorum vote 1: %v", err)
	}
	finalInstance, finalStage, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "voter-2", Action: "APPROVE",
	})
	if err != nil {
		t.Fatalf("quorum vote 2: %v", err)
	}
	if finalStage.StageStatus != "APPROVED" || finalInstance.WorkflowStatus != "APPROVED" {
		t.Fatalf("expected the mixed chain to complete APPROVED, got instance=%+v stage=%+v", finalInstance, finalStage)
	}
}

// Raw-trigger-equivalent negative control: a decision from a principal
// NOT in the eligible pool is rejected by the composite FK at the
// database level, not merely by the application's own pre-check.
func TestPgStore_Quorum_RawInsert_IneligibleApprover_RejectedByFK(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	instance, stages, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = instance

	_, err = pool.Exec(context.Background(),
		`INSERT INTO workflow_stage_quorum_decisions (workflow_stage_id, approver_principal_id, decision) VALUES ($1, $2, 'APPROVE')`,
		stages[0].WorkflowStageID, "stranger-1")
	if err == nil {
		t.Fatal("a decision from a non-eligible principal must be rejected by the composite FK")
	}
}
