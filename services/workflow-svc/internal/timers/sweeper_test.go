package timers_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
	"zoiko.io/workflow-svc/internal/timers"
)

type call struct {
	op, id, tenant, actor, target, reason string
	decisionID                            string
}

type fakeStore struct {
	mu        sync.Mutex
	due       []domain.DueInstance
	approvers map[string][]domain.ApproverCheck
	calls     []call
	cleared   []string
}

func (f *fakeStore) ListDueInstances(context.Context, time.Time, int) ([]domain.DueInstance, error) {
	return f.due, nil
}
func (f *fakeStore) ListCurrentApprovers(_ context.Context, id string) ([]domain.ApproverCheck, error) {
	return f.approvers[id], nil
}
func (f *fakeStore) ClearApproverRecheck(_ context.Context, id string, _ time.Time) error {
	f.cleared = append(f.cleared, id)
	return nil
}
func (f *fakeStore) ExpireWorkflow(ctx context.Context, id string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error) {
	f.calls = append(f.calls, call{op: "expire", id: id, tenant: svcmiddleware.TenantFromContext(ctx), reason: cmd[0].ReasonCode, decisionID: cmd[0].Meta.DecisionID})
	return &domain.WorkflowInstance{}, true, nil
}
func (f *fakeStore) EscalateWorkflow(ctx context.Context, id, actor string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error) {
	f.calls = append(f.calls, call{op: "escalate", id: id, tenant: svcmiddleware.TenantFromContext(ctx), actor: actor, reason: cmd[0].ReasonCode, decisionID: cmd[0].Meta.DecisionID})
	return &domain.WorkflowInstance{}, true, nil
}
func (f *fakeStore) EscalateToWorkflow(ctx context.Context, p domain.EscalateToWorkflowParams) (*domain.WorkflowInstance, bool, error) {
	f.calls = append(f.calls, call{op: "escalate_to", id: p.WorkflowInstanceID, tenant: svcmiddleware.TenantFromContext(ctx), actor: p.ActorPrincipalID, target: p.EscalationTarget, reason: p.ReasonCode, decisionID: p.Meta.DecisionID})
	return &domain.WorkflowInstance{}, true, nil
}

type fakeAuthz struct{ answers map[string]error }

func (a fakeAuthz) CheckApprovalAllowed(_ context.Context, principal, _, _ string, _ map[string]string) (domain.ActionAuthority, error) {
	return domain.ActionAuthority{}, a.answers[principal]
}

var now = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func ptrTime(t time.Time) *time.Time { return &t }
func ptrStr(s string) *string        { return &s }

func run(t *testing.T, st *fakeStore, az fakeAuthz) domain.TimerSweepResult {
	t.Helper()
	res, err := timers.New(st, az, time.Second, 10, zap.NewNop(), nil).WithClock(func() time.Time { return now }).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A workflow past its due_at expires, under its own tenant, with a ledger
// decision id. Before 21 Oct 2026 nothing read due_at.
func TestSweep_ExpiresPastDueAt(t *testing.T) {
	st := &fakeStore{due: []domain.DueInstance{{TenantID: "t-1", WorkflowInstanceID: "w-1", WorkflowStatus: "PENDING", DueAt: ptrTime(now.Add(-time.Minute))}}}
	res := run(t, st, fakeAuthz{})
	if res.Expired != 1 || len(st.calls) != 1 || st.calls[0].op != "expire" || st.calls[0].tenant != "t-1" ||
		st.calls[0].reason != timers.ReasonDeadlinePassed || st.calls[0].decisionID == "" {
		t.Fatalf("calls = %+v, res = %+v", st.calls, res)
	}
}

func TestSweep_EscalatesAnOverdueStageToItsTarget(t *testing.T) {
	st := &fakeStore{due: []domain.DueInstance{{TenantID: "t-1", WorkflowInstanceID: "w-1", WorkflowStatus: "PENDING",
		NextActionAt: ptrTime(now.Add(-time.Minute)), EscalationTarget: ptrStr("head-of-finance"), InitiatedBy: "init"}}}
	res := run(t, st, fakeAuthz{})
	if res.Escalated != 1 || st.calls[0].op != "escalate_to" || st.calls[0].target != "head-of-finance" ||
		st.calls[0].actor != timers.SystemActor || st.calls[0].reason != timers.ReasonStageDeadlinePassed {
		t.Fatalf("calls = %+v", st.calls)
	}
}

// With no target, or a target who is the initiator, the instance goes to
// ESCALATED for the administrators' queue instead.
func TestSweep_EscalatesWithoutATargetToTheQueue(t *testing.T) {
	for _, target := range []*string{nil, ptrStr("init")} {
		st := &fakeStore{due: []domain.DueInstance{{TenantID: "t-1", WorkflowInstanceID: "w-1", WorkflowStatus: "PENDING",
			NextActionAt: ptrTime(now.Add(-time.Minute)), EscalationTarget: target, InitiatedBy: "init"}}}
		run(t, st, fakeAuthz{})
		if len(st.calls) != 1 || st.calls[0].op != "escalate" {
			t.Fatalf("target %v: calls = %+v", target, st.calls)
		}
	}
}

// A future deadline, or an ESCALATED instance's stage deadline, is not acted on.
func TestSweep_LeavesWhatIsNotDue(t *testing.T) {
	st := &fakeStore{due: []domain.DueInstance{
		{TenantID: "t-1", WorkflowInstanceID: "w-1", WorkflowStatus: "PENDING", DueAt: ptrTime(now.Add(time.Hour)), NextActionAt: ptrTime(now.Add(time.Hour))},
		{TenantID: "t-1", WorkflowInstanceID: "w-2", WorkflowStatus: "ESCALATED", NextActionAt: ptrTime(now.Add(-time.Hour))},
	}}
	run(t, st, fakeAuthz{})
	if len(st.calls) != 0 {
		t.Fatalf("calls = %+v", st.calls)
	}
}

// A flagged instance whose approver lost authority is escalated; one whose
// approvers all still hold it is just cleared.
func TestSweep_RecheckEscalatesWhenAnApproverLostAuthority(t *testing.T) {
	st := &fakeStore{
		due: []domain.DueInstance{
			{TenantID: "t-1", WorkflowInstanceID: "w-lost", WorkflowStatus: "PENDING", RecheckAt: ptrTime(now)},
			{TenantID: "t-1", WorkflowInstanceID: "w-ok", WorkflowStatus: "PENDING", RecheckAt: ptrTime(now)},
		},
		approvers: map[string][]domain.ApproverCheck{
			"w-lost": {{ApproverPrincipalID: "gone"}},
			"w-ok":   {{ApproverPrincipalID: "fine"}, {StageType: "ROLE", ApproverRole: "FINANCE"}},
		},
	}
	res := run(t, st, fakeAuthz{answers: map[string]error{"gone": domain.ErrAuthorizationDenied}})
	if len(st.calls) != 1 || st.calls[0].id != "w-lost" || st.calls[0].reason != timers.ReasonApproverAuthority {
		t.Fatalf("calls = %+v", st.calls)
	}
	if res.Rechecked != 2 || len(st.cleared) != 2 {
		t.Fatalf("rechecked = %d cleared = %v", res.Rechecked, st.cleared)
	}
}

// authorization-svc unreachable: nothing is escalated and the flag stays.
func TestSweep_RecheckKeepsTheFlagWhenAuthzCannotAnswer(t *testing.T) {
	st := &fakeStore{
		due:       []domain.DueInstance{{TenantID: "t-1", WorkflowInstanceID: "w-1", WorkflowStatus: "PENDING", RecheckAt: ptrTime(now)}},
		approvers: map[string][]domain.ApproverCheck{"w-1": {{ApproverPrincipalID: "a"}}},
	}
	run(t, st, fakeAuthz{answers: map[string]error{"a": domain.ErrAuthorizationServiceUnavailable}})
	if len(st.calls) != 0 || len(st.cleared) != 0 {
		t.Fatalf("calls = %+v cleared = %v", st.calls, st.cleared)
	}
}

// An escalated instance's flag is cleared without asking authorization-svc:
// the question's own denial would emit authorization.denied and flag it again.
func TestSweep_RecheckOfAnEscalatedInstanceAsksNothing(t *testing.T) {
	st := &fakeStore{
		due:       []domain.DueInstance{{TenantID: "t-1", WorkflowInstanceID: "w-1", WorkflowStatus: "ESCALATED", RecheckAt: ptrTime(now)}},
		approvers: map[string][]domain.ApproverCheck{"w-1": {{ApproverPrincipalID: "gone"}}},
	}
	run(t, st, fakeAuthz{answers: map[string]error{"gone": domain.ErrAuthorizationDenied}})
	if len(st.calls) != 0 || len(st.cleared) != 1 {
		t.Fatalf("calls = %+v cleared = %v", st.calls, st.cleared)
	}
}
