// Package timers runs workflow-svc's deadline and re-check pass (Governance
// audit workflow rows 26-28 and 33, 21 Oct 2026).
//
// Before this pass due_at, next_action_at and escalation_target were stored
// and nothing read them: a workflow past its deadline stayed PENDING for
// ever, an overdue stage never escalated, and an approver who lost their
// authority after the workflow started stayed its approver. The sweeper:
//
//   - expires an open instance whose due_at has passed (workflow.expired);
//   - escalates a PENDING instance whose current stage is past its deadline
//     (next_action_at) — to the definition's escalation target when it has
//     one, otherwise to ESCALATED for the administrators' queue;
//   - re-authorizes the named approvers of an instance flagged for a re-check
//     (policy.updated / authority.delegated / authorization.denied), and
//     escalates it when one of them may no longer approve.
//
// Every transition runs under the instance's own tenant through the store's
// ordinary lifecycle methods, so it is recorded, evented and ledgered like
// any other.
package timers

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/authz"
	"zoiko.io/workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// SystemActor is the actor the sweeper's transitions are recorded under.
const SystemActor = "system:workflow-svc"

// Reason codes the sweeper records.
const (
	ReasonDeadlinePassed      = "DEADLINE_PASSED"
	ReasonStageDeadlinePassed = "STAGE_DEADLINE_PASSED"
	ReasonApproverAuthority   = "APPROVER_AUTHORITY_LOST"
)

// Store is what the sweeper needs from the workflow store.
type Store interface {
	ListDueInstances(ctx context.Context, now time.Time, limit int) ([]domain.DueInstance, error)
	ListCurrentApprovers(ctx context.Context, workflowInstanceID string) ([]domain.ApproverCheck, error)
	ClearApproverRecheck(ctx context.Context, workflowInstanceID string, checkedFrom time.Time) error
	ExpireWorkflow(ctx context.Context, workflowInstanceID string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error)
	EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error)
	EscalateToWorkflow(ctx context.Context, params domain.EscalateToWorkflowParams) (*domain.WorkflowInstance, bool, error)
}

// Authorizer re-checks an approver.
type Authorizer interface {
	CheckApprovalAllowed(ctx context.Context, principalID, legalEntityID, resourceOwnerPrincipalID string, attrs map[string]string) (domain.ActionAuthority, error)
}

// Sweeper is the timer pass.
type Sweeper struct {
	store    Store
	authz    Authorizer
	interval time.Duration
	batch    int
	log      *zap.Logger
	now      func() time.Time

	actions *prometheus.CounterVec
	overdue prometheus.Gauge
}

// New builds a sweeper. reg may be nil (no metrics).
func New(store Store, az Authorizer, interval time.Duration, batch int, log *zap.Logger, reg prometheus.Registerer) *Sweeper {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if batch <= 0 {
		batch = 100
	}
	s := &Sweeper{store: store, authz: az, interval: interval, batch: batch, log: log, now: time.Now,
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "workflow_timer_actions_total",
			Help: "Transitions and re-checks made by the workflow timer pass, by action and result.",
		}, []string{"action", "result"}),
		overdue: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_timer_due_instances",
			Help: "Open workflow instances the last timer pass found due (a growing value means the pass is not keeping up or keeps failing).",
		}),
	}
	if reg != nil {
		reg.MustRegister(s.actions, s.overdue)
	}
	return s
}

// WithClock replaces the clock (tests).
func (s *Sweeper) WithClock(now func() time.Time) *Sweeper { s.now = now; return s }

// Start runs passes until ctx is cancelled.
func (s *Sweeper) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	s.log.Info("workflow timer pass started", zap.Duration("interval", s.interval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.RunOnce(ctx); err != nil {
				s.log.Error("workflow timer pass failed", zap.Error(err))
			}
		}
	}
}

// RunOnce makes one pass.
func (s *Sweeper) RunOnce(ctx context.Context) (domain.TimerSweepResult, error) {
	var res domain.TimerSweepResult
	now := s.now()
	due, err := s.store.ListDueInstances(ctx, now, s.batch)
	if err != nil {
		return res, err
	}
	s.overdue.Set(float64(len(due)))
	for _, d := range due {
		tctx := svcmiddleware.WithTenant(ctx, d.TenantID)
		switch {
		case d.DueAt != nil && !d.DueAt.After(now):
			if s.expire(tctx, d) {
				res.Expired++
			}
			continue
		case d.WorkflowStatus == domain.WorkflowStatusPending && d.NextActionAt != nil && !d.NextActionAt.After(now):
			if s.escalate(tctx, d, ReasonStageDeadlinePassed) {
				res.Escalated++
				d.WorkflowStatus = domain.WorkflowStatusEscalated
			}
		}
		if d.RecheckAt != nil && s.recheck(tctx, d) {
			res.Rechecked++
		}
	}
	return res, nil
}

func command(reason string) domain.LifecycleCommand {
	return domain.LifecycleCommand{ReasonCode: reason, Meta: domain.CommandMeta{DecisionID: uuid.NewString()}}
}

func (s *Sweeper) record(action string, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	s.actions.WithLabelValues(action, result).Inc()
}

func (s *Sweeper) expire(ctx context.Context, d domain.DueInstance) bool {
	_, changed, err := s.store.ExpireWorkflow(ctx, d.WorkflowInstanceID, command(ReasonDeadlinePassed))
	s.record("expire", err)
	if err != nil {
		s.log.Warn("timer: expire failed", zap.String("workflow_instance_id", d.WorkflowInstanceID), zap.Error(err))
		return false
	}
	return changed
}

// escalate routes the instance to its escalation target when it has one
// (and that target is not the initiator), otherwise to ESCALATED.
func (s *Sweeper) escalate(ctx context.Context, d domain.DueInstance, reason string) bool {
	var changed bool
	var err error
	if d.EscalationTarget != nil && *d.EscalationTarget != "" && *d.EscalationTarget != d.InitiatedBy {
		cmd := command(reason)
		_, changed, err = s.store.EscalateToWorkflow(ctx, domain.EscalateToWorkflowParams{
			WorkflowInstanceID: d.WorkflowInstanceID, ActorPrincipalID: SystemActor,
			EscalationTarget: *d.EscalationTarget, ReasonCode: reason, Meta: cmd.Meta,
		})
	} else {
		_, changed, err = s.store.EscalateWorkflow(ctx, d.WorkflowInstanceID, SystemActor, command(reason))
	}
	s.record("escalate", err)
	if err != nil {
		s.log.Warn("timer: escalate failed", zap.String("workflow_instance_id", d.WorkflowInstanceID),
			zap.String("reason", reason), zap.Error(err))
		return false
	}
	return changed
}

// recheck re-authorizes each named approver of the current stage. A ROLE
// stage has no named approver: whoever decides it is authorized at decision
// time. When authorization-svc cannot answer, the flag stays for the next
// pass.
func (s *Sweeper) recheck(ctx context.Context, d domain.DueInstance) bool {
	if d.WorkflowStatus != domain.WorkflowStatusPending {
		// Already escalated (or escalated by this pass): off the approvers'
		// desks. Asking authorization-svc anyway would emit another
		// authorization.denied and flag the instance again, for ever.
		err := s.store.ClearApproverRecheck(ctx, d.WorkflowInstanceID, *d.RecheckAt)
		s.record("recheck", err)
		return err == nil
	}
	approvers, err := s.store.ListCurrentApprovers(ctx, d.WorkflowInstanceID)
	if err != nil {
		s.record("recheck", err)
		return false
	}
	lost := ""
	for _, a := range approvers {
		if a.ApproverPrincipalID == "" {
			continue
		}
		_, err := s.authz.CheckApprovalAllowed(ctx, a.ApproverPrincipalID, d.LegalEntityID, d.InitiatedBy, map[string]string{
			authz.AttrWorkflowType: d.WorkflowType, authz.AttrApproverRole: a.ApproverRole,
			authz.AttrStageOrder: strconv.Itoa(a.StageOrder),
		})
		switch {
		case errors.Is(err, domain.ErrAuthorizationDenied):
			lost = a.ApproverPrincipalID
		case err != nil:
			s.record("recheck", err)
			return false
		}
		if lost != "" {
			break
		}
	}
	if lost != "" {
		s.log.Info("timer: approver may no longer approve — escalating",
			zap.String("workflow_instance_id", d.WorkflowInstanceID), zap.String("approver_principal_id", lost))
		if !s.escalate(ctx, d, ReasonApproverAuthority) {
			return false
		}
	}
	err = s.store.ClearApproverRecheck(ctx, d.WorkflowInstanceID, *d.RecheckAt)
	s.record("recheck", err)
	return err == nil
}
