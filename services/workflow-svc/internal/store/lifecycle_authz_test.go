package store_test

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

// IsCurrentStageApprover decides whether an escalation is a participant's
// (WORKFLOW_ESCALATE) or an administrative act (WORKFLOW_ADMINISTER), so a
// false "yes" would hand a stranger the weaker permission (Governance audit
// 6/7, gap #1).

func TestPgStore_IsCurrentStageApprover_SingleStagesFollowTheChain(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	instance, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	expect := func(principal string, want bool) {
		t.Helper()
		got, err := s.IsCurrentStageApprover(ctx, instance.WorkflowInstanceID, principal)
		if err != nil {
			t.Fatalf("IsCurrentStageApprover(%s): %v", principal, err)
		}
		if got != want {
			t.Errorf("IsCurrentStageApprover(%s) = %v, want %v", principal, got, want)
		}
	}

	expect("approver-1", true)   // stage 1 is current
	expect("approver-2", false)  // on the chain, not on the current stage
	expect("requester-1", false) // the initiator is never a stage approver
	expect("stranger-1", false)

	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{
		WorkflowInstanceID: instance.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "APPROVE",
	}); err != nil {
		t.Fatalf("approve stage 1: %v", err)
	}
	expect("approver-1", false) // stage 1 is done
	expect("approver-2", true)
}

func TestPgStore_IsCurrentStageApprover_QuorumPool(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	instance, _, _, err := s.CreateWorkflow(ctx, quorumOnlyParams(3, 2))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for principal, want := range map[string]bool{"voter-1": true, "voter-3": true, "voter-4": false, "requester-1": false} {
		got, err := s.IsCurrentStageApprover(ctx, instance.WorkflowInstanceID, principal)
		if err != nil {
			t.Fatalf("IsCurrentStageApprover(%s): %v", principal, err)
		}
		if got != want {
			t.Errorf("IsCurrentStageApprover(%s) = %v, want %v", principal, got, want)
		}
	}
}

// Tenant B naming tenant A's workflow and its real approver must get false,
// both as the superuser (the explicit predicate) and as a NOBYPASSRLS role
// with no tenant at all (the RLS policy).
func TestPgStore_IsCurrentStageApprover_TenantIsolation(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	instance, _, _, err := s.CreateWorkflow(tenantCtx(testTenantID), twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	tenantB := "00000000-0000-0000-0000-0000000000b1"
	if got, err := s.IsCurrentStageApprover(tenantCtx(tenantB), instance.WorkflowInstanceID, "approver-1"); err != nil || got {
		t.Fatalf("ISOLATION FAILURE: tenant B saw tenant A's current approver: got=%v err=%v", got, err)
	}

	appStore := store.New(appRolePool(t, pool), zap.NewNop())
	if got, err := appStore.IsCurrentStageApprover(context.Background(), instance.WorkflowInstanceID, "approver-1"); err != nil || got {
		t.Fatalf("ISOLATION FAILURE: no tenant in context still matched: got=%v err=%v", got, err)
	}
	if got, err := appStore.IsCurrentStageApprover(tenantCtx(testTenantID), instance.WorkflowInstanceID, "approver-1"); err != nil || !got {
		t.Fatalf("the RLS-bound role must still see its own tenant's approver: got=%v err=%v", got, err)
	}
}
