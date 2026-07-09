package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

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
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS workflow_transitions, workflow_stages, workflow_instances CASCADE;")

	mig1, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	if err != nil {
		t.Fatalf("failed to read migration 1: %v", err)
	}
	if _, err := pool.Exec(ctx, string(mig1)); err != nil {
		t.Fatalf("failed to execute migration 1: %v", err)
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

func TestPgStore_CreateWorkflow_NoStages(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	params := twoStageParams()
	params.Stages = nil
	_, _, err := s.CreateWorkflow(context.Background(), params)
	if !errors.Is(err, domain.ErrNoStages) {
		t.Fatalf("expected ErrNoStages, got %v", err)
	}
}

func TestPgStore_FullApprovalChain_CompletesWorkflow(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	instance, stages, err := s.CreateWorkflow(ctx, twoStageParams())
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
	ctx := context.Background()

	instance, _, err := s.CreateWorkflow(ctx, twoStageParams())
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
	ctx := context.Background()

	instance, _, err := s.CreateWorkflow(ctx, twoStageParams())
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
	ctx := context.Background()

	instance, _, err := s.CreateWorkflow(ctx, twoStageParams())
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
	ctx := context.Background()

	params := twoStageParams()
	params.Stages = []domain.CreateWorkflowStageInput{{ApproverPrincipalID: "approver-1"}}
	instance, _, err := s.CreateWorkflow(ctx, params)
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
