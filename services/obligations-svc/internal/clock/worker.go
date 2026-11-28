// Package clock runs the background work of R-001 WFC-05 deadline clocks:
//
//   - the sweep persists computed state moves (ACTIVE -> DUE_SOON -> DUE ->
//     OVERDUE) and emits their events. Reads never wait for it: a clock past
//     due_at reads OVERDUE whether or not the sweep has run (R-INV-26, NP-16).
//     Each move is guarded on the row version it read, so a restart mid-sweep
//     writes no second transition and no second event (NP-37).
//   - due milestones fire exactly once (guarded UPDATE on fired_at); an
//     escalation never changes the clock's state or due_at (R-001 s7.4).
//   - the case relay delivers WORKFLOW_CASE outbox rows to workflow-svc
//     (POST /v1/cases, idempotent per clock and milestone) and records the
//     case it opened.
package clock

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/store"
	"zoiko.io/obligations-svc/internal/telemetry"
	"zoiko.io/obligations-svc/internal/workflowclient"
)

// Store is what the workers need from persistence.
type Store interface {
	ClocksNeedingSweep(ctx context.Context, now time.Time, limit int) ([]store.WorkItem, error)
	SweepClock(ctx context.Context, item store.WorkItem, now time.Time) (bool, error)
	DueMilestones(ctx context.Context, now time.Time, limit int) ([]store.WorkItem, error)
	FireMilestone(ctx context.Context, item store.WorkItem, now time.Time) (bool, error)
	ClockStats(ctx context.Context, now time.Time) ([]store.OverdueCount, float64, error)
	RelayOutboxDestination(ctx context.Context, destination string, limit int, deliver func(ctx context.Context, e store.OutboxEvent) error) (int, int, error)
	RecordCaseOpened(ctx context.Context, req store.CaseRequest, caseID string) error
}

// Worker runs the sweep, the milestones and the case relay on one ticker.
type Worker struct {
	store    Store
	workflow workflowclient.Client
	metrics  *telemetry.Metrics
	log      *zap.Logger
	interval time.Duration
	now      func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWorker builds a worker. workflow may be nil (case requests stay queued).
func NewWorker(st Store, wf workflowclient.Client, metrics *telemetry.Metrics, log *zap.Logger, interval time.Duration) *Worker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Worker{store: st, workflow: wf, metrics: metrics, log: log, interval: interval, now: func() time.Time { return time.Now().UTC() }}
}

// Start runs the loop until Stop.
func (w *Worker) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			w.Tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Stop ends the loop and waits for the current tick.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

// TickResult counts what one tick did (tests and logs).
type TickResult struct {
	Moved, MilestonesFired, CasesOpened, CaseFailures int
}

// Tick runs one pass: sweep, milestones, case relay, gauges.
func (w *Worker) Tick(ctx context.Context) TickResult {
	var res TickResult
	now := w.now()

	items, err := w.store.ClocksNeedingSweep(ctx, now, 500)
	if err != nil {
		w.log.Error("clock sweep: listing failed", zap.Error(err))
	}
	for _, it := range items {
		moved, err := w.store.SweepClock(ctx, it, now)
		if err != nil {
			w.log.Error("clock sweep: move failed", zap.String("clock_id", it.ID), zap.Error(err))
			continue
		}
		if moved {
			res.Moved++
			if w.metrics != nil {
				w.metrics.ClockSweepMoves.WithLabelValues("computed").Inc()
			}
		}
	}

	due, err := w.store.DueMilestones(ctx, now, 500)
	if err != nil {
		w.log.Error("clock milestones: listing failed", zap.Error(err))
	}
	for _, it := range due {
		fired, err := w.store.FireMilestone(ctx, it, now)
		if err != nil {
			w.log.Error("clock milestones: fire failed", zap.String("milestone_id", it.ID), zap.Error(err))
			if w.metrics != nil {
				w.metrics.EscalationFailures.WithLabelValues("milestone").Inc()
			}
			continue
		}
		if fired {
			res.MilestonesFired++
			if w.metrics != nil {
				w.metrics.MilestonesFired.WithLabelValues("any").Inc()
			}
		}
	}

	if w.workflow != nil {
		opened, failed, err := w.store.RelayOutboxDestination(ctx, store.DestinationWorkflowCase, 50, func(ctx context.Context, e store.OutboxEvent) error {
			var req store.CaseRequest
			if err := json.Unmarshal(e.EventPayload, &req); err != nil {
				return err
			}
			caseID, err := w.workflow.OpenCase(ctx, workflowclient.CaseRequest{TenantID: req.TenantID, LegalEntityID: req.LegalEntityID,
				CaseFamily: req.CaseFamily, Severity: req.Severity, Confidentiality: req.Confidentiality, Title: req.Title,
				Description: req.Description, SourceType: req.SourceType, SourceID: req.SourceID, Milestone: req.Milestone,
				IdempotencyKey: req.IdempotencyKey})
			if err != nil {
				return err
			}
			return w.store.RecordCaseOpened(ctx, req, caseID)
		})
		if err != nil {
			w.log.Error("escalation case relay failed", zap.Error(err))
		}
		res.CasesOpened, res.CaseFailures = opened, failed
		if failed > 0 && w.metrics != nil {
			w.metrics.EscalationFailures.WithLabelValues("OPEN_CASE").Add(float64(failed))
		}
	}

	if w.metrics != nil {
		if counts, lag, err := w.store.ClockStats(ctx, now); err == nil {
			w.metrics.ClocksOverdue.Reset()
			for _, c := range counts {
				w.metrics.ClocksOverdue.WithLabelValues(c.ClockClass, c.Jurisdiction).Set(float64(c.Count))
			}
			w.metrics.ClockSweepLagSeconds.Set(lag)
		}
	}
	return res
}
