package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/events"
)

// ResolveException moves an exception along its governed lifecycle with optimistic
// concurrency, edge validation, maker-checker, an append-only transition carrying the
// authority/evidence behind it, an outbox event, and, when the last exception of a
// failed run becomes terminal, the run's own roll-up to READY_TO_CERTIFY, all in ONE
// transaction.
func (s *PgStore) ResolveException(ctx context.Context, tenantID, exceptionID, actor, correlationID string, expectedVersion int, req domain.ResolveExceptionRequest) (*domain.ControlException, error) {
	var out domain.ControlException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var cur domain.ControlException
		if err := scanException(tx.QueryRow(ctx, `SELECT `+exColumns+` FROM control_exceptions
			WHERE tenant_id = $1 AND exception_id = $2 FOR UPDATE`, tenantID, exceptionID), &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if cur.Version != expectedVersion {
			return fmt.Errorf("%w: exception is at version %d, caller expected %d", domain.ErrConflict, cur.Version, expectedVersion)
		}
		to := domain.ExceptionState(req.ToState)
		if err := domain.ValidateExceptionTransition(cur.State, to); err != nil {
			return err
		}
		var remediator string
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT actor_id FROM exception_transitions
			WHERE tenant_id = $1 AND exception_id = $2 AND to_state = 'REMEDIATED'
			ORDER BY transition_id DESC LIMIT 1), '')`, tenantID, exceptionID).Scan(&remediator); err != nil {
			return err
		}
		if err := domain.CheckIndependence(to, actor, cur.OwnerPrincipal, remediator); err != nil {
			return err
		}
		var earlier int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM control_exceptions e JOIN control_runs r ON r.run_id = e.run_id
			WHERE e.tenant_id = $1 AND r.tenant_id = $1 AND r.legal_entity_id = $2
			  AND r.control_definition_id = (SELECT control_definition_id FROM control_runs WHERE tenant_id = $1 AND run_id = $3)
			  AND e.reason_code = $4 AND e.record_ids && $5::text[] AND e.exception_id <> $6 AND e.created_at < $7`,
			tenantID, cur.LegalEntityID, cur.RunID, cur.ReasonCode, cur.RecordIDs, cur.ExceptionID, cur.CreatedAt).Scan(&earlier); err != nil {
			return err
		}
		if err := req.CheckRootCause(cur.Severity, earlier > 0); err != nil {
			return err
		}

		if err := scanException(tx.QueryRow(ctx, `UPDATE control_exceptions SET state = $3,
				version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND exception_id = $2 RETURNING `+exColumns, tenantID, exceptionID, req.ToState), &out); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO exception_transitions
			(tenant_id, exception_id, from_state, to_state, reason, actor_id, correlation_id, authority_ref, evidence_ref, carry_to_period, root_cause_code, root_cause_note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, tenantID, exceptionID, string(cur.State), req.ToState,
			req.Reason, actor, correlationID, req.AuthorityRef, req.EvidenceRef, req.CarryToPeriod, req.RootCauseCode, req.RootCauseNote); err != nil {
			return err
		}

		var run domain.ControlRun
		if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2`, tenantID, out.RunID), &run); err != nil {
			return err
		}
		if et := resolutionEvent(to); et != "" {
			if err := emitException(ctx, tx, run, exceptionID, et, actor, correlationID, map[string]any{
				"exception_id": exceptionID, "run_id": out.RunID, "from_state": string(cur.State), "to_state": req.ToState,
				"reason": req.Reason, "authority_ref": req.AuthorityRef, "evidence_ref": req.EvidenceRef,
				"carry_to_period": req.CarryToPeriod, "severity": string(out.Severity),
			}); err != nil {
				return err
			}
		}
		if isTerminalException(to) && run.ResultState == domain.ResultFail {
			_, err := rollupTx(ctx, tx, tenantID, out.RunID, actor, correlationID, "all exceptions resolved", false)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func isTerminalException(s domain.ExceptionState) bool {
	return s == domain.ExClosed || s == domain.ExWaived || s == domain.ExCarriedForward
}

func resolutionEvent(to domain.ExceptionState) string {
	switch to {
	case domain.ExRemediated:
		return events.RemediationRecorded
	case domain.ExReperformed:
		return events.ExceptionReperformed
	case domain.ExClosed:
		return events.ExceptionClosed
	case domain.ExWaived:
		return events.ExceptionWaived
	case domain.ExCarriedForward:
		return events.ExceptionCarried
	}
	return ""
}

// rollupTx moves a run in EXCEPTION_REVIEW to READY_TO_CERTIFY once no exception of it
// is unresolved. A run whose certification was rejected returns to PENDING.
// explicit=true is the operator's own resubmission after a rejection; without it only a
// FAIL result rolls up, so a rejected run cannot bounce back to the certifier with
// nothing having changed. It returns nil, nil when there is nothing to do.
func rollupTx(ctx context.Context, tx pgx.Tx, tenantID, runID, actor, correlationID, reason string, explicit bool) (*domain.ControlRun, error) {
	var cur domain.ControlRun
	if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
		WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, tenantID, runID), &cur); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	if cur.LifecycleState != domain.LifecycleExceptionReview {
		if explicit {
			return nil, fmt.Errorf("%w: only a run in EXCEPTION_REVIEW can be submitted for certification (run is %s)", domain.ErrInvalidTransition, cur.LifecycleState)
		}
		return nil, nil
	}
	if !explicit && cur.ResultState != domain.ResultFail {
		return nil, nil
	}
	var unresolved, approved int
	if err := tx.QueryRow(ctx, `SELECT
			COUNT(*) FILTER (WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY')),
			COUNT(*) FILTER (WHERE state IN ('WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY'))
		FROM control_exceptions WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID).Scan(&unresolved, &approved); err != nil {
		return nil, err
	}
	result, ok := domain.RollupOutcome(unresolved, approved)
	if !ok {
		if explicit {
			return nil, fmt.Errorf("%w: %d exception(s) of this run are still unresolved", domain.ErrInvalidTransition, unresolved)
		}
		return nil, nil
	}
	p := AdvanceParams{ExpectedVersion: SkipVersionCheck, ToLifecycle: domain.LifecycleReadyToCertify,
		Reason: reason, Actor: actor, CorrelationID: correlationID}
	if result != cur.ResultState {
		p.ToResult = &result
	}
	if cur.CertificationState == domain.CertRejected {
		pending := domain.CertPending
		p.ToCert = &pending
	}
	return advanceTx(ctx, tx, tenantID, runID, p)
}

// SubmitForCertification is the operator's resubmission of a run that is in
// EXCEPTION_REVIEW with every exception resolved, the way back after a rejected
// certification. Nothing is decided here; the certifier still decides.
func (s *PgStore) SubmitForCertification(ctx context.Context, tenantID, runID, actor, correlationID string, expectedVersion int, reason string) (*domain.ControlRun, error) {
	var out *domain.ControlRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var v int
		if err := tx.QueryRow(ctx, `SELECT version FROM control_runs WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`,
			tenantID, runID).Scan(&v); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if expectedVersion != SkipVersionCheck && v != expectedVersion {
			return fmt.Errorf("%w: run is at version %d, caller expected %d", domain.ErrConflict, v, expectedVersion)
		}
		var err error
		out, err = rollupTx(ctx, tx, tenantID, runID, actor, correlationID, reason, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListExceptionTransitions returns an exception's append-only history.
func (s *PgStore) ListExceptionTransitions(ctx context.Context, tenantID, exceptionID string) ([]domain.ExceptionTransition, error) {
	var out []domain.ExceptionTransition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT from_state, to_state, reason, actor_id, correlation_id, authority_ref,
				evidence_ref, carry_to_period, root_cause_code, root_cause_note, occurred_at
			FROM exception_transitions WHERE tenant_id = $1 AND exception_id = $2 ORDER BY transition_id`, tenantID, exceptionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.ExceptionTransition
			if err := rows.Scan(&t.From, &t.To, &t.Reason, &t.ActorID, &t.CorrelationID, &t.AuthorityRef,
				&t.EvidenceRef, &t.CarryToPeriod, &t.RootCauseCode, &t.RootCauseNote, &t.OccurredAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// SweepResult counts what one sweep did.
type SweepResult struct {
	ExpiredRuns int
	SLABreaches int
	Errors      int
}

type tenantRef struct{ tenant, id string }

func (s *PgStore) collect(ctx context.Context, sql string, args ...any) ([]tenantRef, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tenantRef
	for rows.Next() {
		var r tenantRef
		if err := rows.Scan(&r.tenant, &r.id); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Sweep is the service's housekeeping pass, safe to run repeatedly and concurrently:
//   - a run that never started (SCHEDULED / PREPARING) for longer than runTTL is EXPIRED,
//     so a run nobody executed does not sit forever looking pending;
//   - each unresolved exception past its due date gets ONE sla_breached event.
//
// It runs across tenants (the pool is a superuser) and applies each change under that
// tenant's own RLS context.
func (s *PgStore) Sweep(ctx context.Context, now time.Time, runTTL time.Duration, batch int) (SweepResult, error) {
	var res SweepResult
	stale, err := s.collect(ctx, `SELECT tenant_id::text, run_id::text FROM control_runs
		WHERE lifecycle_state IN ('SCHEDULED','PREPARING') AND created_at < $1
		ORDER BY created_at LIMIT $2`, now.Add(-runTTL), batch)
	if err != nil {
		return res, err
	}
	for _, r := range stale {
		_, err := s.AdvanceRun(ctx, r.tenant, r.id, AdvanceParams{ExpectedVersion: SkipVersionCheck,
			ToLifecycle: domain.LifecycleExpired, Reason: "run not executed within its validity window",
			Actor: "system:sweeper", CorrelationID: "sweep"})
		switch {
		case err == nil:
			res.ExpiredRuns++
		case errors.Is(err, domain.ErrInvalidTransition):
			// raced with an execution starting: leave it
		default:
			res.Errors++
			s.log.Warn("sweep: expire run failed", zap.String("run_id", r.id), zap.Error(err))
		}
	}

	late, err := s.collect(ctx, `SELECT tenant_id::text, exception_id::text FROM control_exceptions
		WHERE sla_breach_notified_at IS NULL AND due_at < $1
		  AND state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY')
		ORDER BY due_at LIMIT $2`, now, batch)
	if err != nil {
		return res, err
	}
	for _, r := range late {
		breached, err := s.markSLABreach(ctx, r.tenant, r.id, now)
		switch {
		case err != nil:
			res.Errors++
			s.log.Warn("sweep: sla breach failed", zap.String("exception_id", r.id), zap.Error(err))
		case breached:
			res.SLABreaches++
		}
	}
	return res, nil
}

func (s *PgStore) markSLABreach(ctx context.Context, tenantID, exceptionID string, now time.Time) (bool, error) {
	done := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e domain.ControlException
		if err := scanException(tx.QueryRow(ctx, `SELECT `+exColumns+` FROM control_exceptions
			WHERE tenant_id = $1 AND exception_id = $2 AND sla_breach_notified_at IS NULL FOR UPDATE`,
			tenantID, exceptionID), &e); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // already announced by another sweeper
			}
			return err
		}
		if isTerminalException(e.State) || !now.After(e.DueAt) {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE control_exceptions SET sla_breach_notified_at = now()
			WHERE tenant_id = $1 AND exception_id = $2`, tenantID, exceptionID); err != nil {
			return err
		}
		var run domain.ControlRun
		if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2`, tenantID, e.RunID), &run); err != nil {
			return err
		}
		done = true
		return emitException(ctx, tx, run, exceptionID, events.ExceptionSLABreached, "system:sweeper", "sweep", map[string]any{
			"exception_id": exceptionID, "run_id": e.RunID, "severity": string(e.Severity), "state": string(e.State),
			"owner_role": e.OwnerRole, "due_at": e.DueAt.UTC().Format(time.RFC3339),
		})
	})
	return done, err
}

// supersedePrior retires the earlier run a new run replaces (Invariant 2: a certified run
// is never edited, only superseded). It applies only when the earlier run covers the same
// legal entity and period and is READY_TO_CERTIFY or CERTIFIED; any other prior run is
// merely linked. A certified run's certification is marked SUPERSEDED, and the earlier run
// records which run replaced it.
func supersedePrior(ctx context.Context, tx pgx.Tx, tenantID, priorID string, next domain.ControlRun, actor, correlationID, reason string) error {
	var pr domain.ControlRun
	if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
		WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, tenantID, priorID), &pr); err != nil {
		return err
	}
	if pr.LegalEntityID != next.LegalEntityID || pr.PeriodID != next.PeriodID {
		return nil
	}
	if pr.LifecycleState != domain.LifecycleReadyToCertify && pr.LifecycleState != domain.LifecycleCertified {
		return nil
	}
	p := AdvanceParams{ExpectedVersion: SkipVersionCheck, ToLifecycle: domain.LifecycleSuperseded,
		Reason: "superseded by run " + next.RunID + ": " + reason, Actor: actor, CorrelationID: correlationID}
	if pr.CertificationState == domain.CertCertified {
		c := domain.CertSuperseded
		p.ToCert = &c
	}
	if _, err := advanceTx(ctx, tx, tenantID, priorID, p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE control_runs SET superseded_by_run_id = $3
		WHERE tenant_id = $1 AND run_id = $2`, tenantID, priorID, next.RunID)
	return err
}
