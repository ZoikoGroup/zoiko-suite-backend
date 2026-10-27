package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payroll-run-svc/internal/domain"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
)

// RunEventFunc builds the outbox event for a payroll run from its state AFTER the
// change. The handler supplies it (it knows the actor and correlation); the
// store calls it inside the transaction that changed the run, so the event exists
// if and only if the change does. A run lifecycle event used to be a Kafka write
// after the commit, which a broker outage silently dropped.
type RunEventFunc func(run domain.PayrollRun) (domain.OutboxEvent, error)

type runEventKey struct{}

// WithRunEvent attaches the event to emit for the next run state change made
// with the returned context.
func WithRunEvent(ctx context.Context, fn RunEventFunc) context.Context {
	return context.WithValue(ctx, runEventKey{}, fn)
}

// RunEventFromContext returns the attached builder, or nil.
func RunEventFromContext(ctx context.Context) RunEventFunc {
	fn, _ := ctx.Value(runEventKey{}).(RunEventFunc)
	return fn
}

func loadRunTx(ctx context.Context, tx pgx.Tx, tenantID, runID string) (*domain.PayrollRun, error) {
	var r domain.PayrollRun
	err := tx.QueryRow(ctx, `
		SELECT run_id, tenant_id, legal_entity_id, run_number, pay_period_start::text,
		       pay_period_end::text, pay_date::text, status, is_shadow_run, total_gross_pay,
		       total_net_pay, total_tax_deductions, total_other_deductions, employee_count,
		       created_at, updated_at, finalized_at, governance_decision_id, snapshot_hash
		FROM payroll_runs WHERE run_id = $1 AND tenant_id = $2`, runID, tenantID).Scan(
		&r.RunID, &r.TenantID, &r.LegalEntityID, &r.RunNumber, &r.PayPeriodStart,
		&r.PayPeriodEnd, &r.PayDate, &r.Status, &r.IsShadowRun, &r.TotalGrossPay,
		&r.TotalNetPay, &r.TotalTaxDeductions, &r.TotalOtherDeductions, &r.EmployeeCount,
		&r.CreatedAt, &r.UpdatedAt, &r.FinalizedAt, &r.GovernanceDecisionID, &r.SnapshotHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPayrollRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// enqueueRunEvent writes the context's run event, if any, in the caller's transaction.
func enqueueRunEvent(ctx context.Context, tx pgx.Tx, tenantID, runID string) error {
	fn := RunEventFromContext(ctx)
	if fn == nil {
		return nil
	}
	run, err := loadRunTx(ctx, tx, tenantID, runID)
	if err != nil {
		return fmt.Errorf("load run for event: %w", err)
	}
	return enqueueRunFor(ctx, tx, *run, fn)
}

func enqueueRunFor(ctx context.Context, tx pgx.Tx, run domain.PayrollRun, fn RunEventFunc) error {
	ev, err := fn(run)
	if err != nil {
		return fmt.Errorf("build run event: %w", err)
	}
	if !domain.EventCatalogue[ev.EventType] {
		return fmt.Errorf("event type %q is not in the WFP catalogue", ev.EventType)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
		ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload); err != nil {
		return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
	}
	return nil
}

// EnqueueRunEvent writes a run event that is not tied to a state change of its own
// (a run BLOCKED before it could be calculated), in its own transaction.
func (s *PgStore) EnqueueRunEvent(ctx context.Context, run domain.PayrollRun, fn RunEventFunc) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error { return enqueueRunFor(ctx, tx, run, fn) })
}
