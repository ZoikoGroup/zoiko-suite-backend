package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-control-svc/internal/domain"
)

// CertifyRun records a certifier's decision on a READY_TO_CERTIFY run, atomically
// with its state movement and outbox event.
//
// Maker-checker (Invariants 11/12): the deciding principal may not be the run's
// creator, nor anyone who appears on the run's transition history (the person who
// executed it, assigned an exception on it, and so on). A single actor can therefore
// never both produce and certify a result.
func (s *PgStore) CertifyRun(ctx context.Context, tenantID, runID, actor, correlationID string, expectedVersion int, req domain.CertifyRequest) (*domain.ControlRun, error) {
	var out *domain.ControlRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var cur domain.ControlRun
		if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, tenantID, runID), &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if expectedVersion != SkipVersionCheck && cur.Version != expectedVersion {
			return fmt.Errorf("%w: run is at version %d, caller expected %d", domain.ErrConflict, cur.Version, expectedVersion)
		}
		if cur.LifecycleState != domain.LifecycleReadyToCertify {
			return fmt.Errorf("%w: only a READY_TO_CERTIFY run can be certified or rejected (run is %s)", domain.ErrInvalidTransition, cur.LifecycleState)
		}
		if cur.CreatedBy == actor {
			return domain.ErrSegregation
		}
		var took bool
		// A certifier who earlier REJECTED this run may decide it again: their own rejection is a
		// certifier act, not participation in producing the result. Rows written in the same
		// transaction share occurred_at, which identifies them.
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM control_run_transitions t
			WHERE t.tenant_id = $1 AND t.run_id = $2 AND t.actor_id = $3
			  AND NOT EXISTS (SELECT 1 FROM control_run_transitions j
				WHERE j.tenant_id = t.tenant_id AND j.run_id = t.run_id AND j.actor_id = t.actor_id
				  AND j.dimension = 'CERTIFICATION' AND j.to_state = 'REJECTED' AND j.occurred_at = t.occurred_at))`,
			tenantID, runID, actor).Scan(&took); err != nil {
			return err
		}
		if took {
			return domain.ErrSegregation
		}

		// READY_TO_CERTIFY means certification is now required: NOT_REQUIRED -> PENDING is
		// recorded in the same transaction so the history shows the request, then the decision.
		if cur.CertificationState == domain.CertNotRequired {
			if _, err := tx.Exec(ctx, `UPDATE control_runs SET certification_state = 'PENDING',
				version = version + 1, updated_at = now() WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID); err != nil {
				return err
			}
			if err := insertTransition(ctx, tx, tenantID, runID, "CERTIFICATION", string(domain.CertNotRequired),
				string(domain.CertPending), "certification requested", "system", correlationID); err != nil {
				return err
			}
		}

		p := AdvanceParams{ExpectedVersion: SkipVersionCheck, Reason: req.Reason, Actor: actor, CorrelationID: correlationID}
		cert := domain.CertCertified
		p.ToLifecycle = domain.LifecycleCertified
		if req.Decision == domain.DecisionReject {
			cert = domain.CertRejected
			p.ToLifecycle = domain.LifecycleExceptionReview
		}
		p.ToCert = &cert
		var err error
		out, err = advanceTx(ctx, tx, tenantID, runID, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CloseGate returns each mandatory (close_gating) control with its latest run for
// the entity and period. Only the latest run counts: an earlier certified run does
// not survive a later run that failed or is still open.
func (s *PgStore) CloseGate(ctx context.Context, tenantID, legalEntityID, periodID string) (*domain.CloseGate, error) {
	var in []domain.GateInput
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT d.control_code, d.name, COALESCE(r.run_id::text,''), COALESCE(r.lifecycle_state,''),
			       COALESCE(r.result_state,''), COALESCE(r.certification_state,'')
			FROM control_definitions d
			LEFT JOIN LATERAL (
				SELECT run_id, lifecycle_state, result_state, certification_state FROM control_runs
				WHERE tenant_id = d.tenant_id AND control_definition_id = d.control_definition_id
				  AND legal_entity_id = $2 AND period_id = $3
				ORDER BY created_at DESC, run_id DESC LIMIT 1) r ON true
			WHERE d.tenant_id = $1 AND d.close_gating
			ORDER BY d.control_code`, tenantID, legalEntityID, periodID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g domain.GateInput
			if err := rows.Scan(&g.ControlCode, &g.ControlName, &g.RunID, &g.Lifecycle, &g.Result, &g.Certification); err != nil {
				return err
			}
			in = append(in, g)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	g := domain.EvaluateGate(legalEntityID, periodID, in)
	return &g, nil
}

// ExceptionSummary aggregates the UNRESOLVED exceptions of the latest run of each
// control for the entity and period, and assesses them against the entity's
// aggregate materiality threshold. Closed, waived and carried-forward exceptions
// are excluded (the last two are authorised, and are counted by the exceptions API).
func (s *PgStore) ExceptionSummary(ctx context.Context, tenantID, legalEntityID, periodID string) (*domain.ExceptionSummary, error) {
	var rows []domain.ExposureRow
	var mat *domain.MaterialityView2
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q, err := tx.Query(ctx, `
			WITH latest AS (
				SELECT DISTINCT ON (control_definition_id) run_id FROM control_runs
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND period_id = $3
				ORDER BY control_definition_id, created_at DESC, run_id DESC)
			SELECT e.currency::text, e.severity, COUNT(*), COALESCE(SUM(e.exposure),0)::text
			FROM control_exceptions e JOIN latest l ON l.run_id = e.run_id
			WHERE e.tenant_id = $1
			  AND e.state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY')
			GROUP BY e.currency, e.severity ORDER BY e.currency, e.severity`, tenantID, legalEntityID, periodID)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var r domain.ExposureRow
			if err := q.Scan(&r.Currency, &r.Severity, &r.Count, &r.Exposure); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		if err := q.Err(); err != nil {
			return err
		}
		var m domain.MaterialityView2
		err = tx.QueryRow(ctx, `SELECT materiality_id::text, aggregate_threshold::text, currency::text
			FROM materiality_policies WHERE tenant_id = $1 AND legal_entity_id = $2 AND effective_from <= CURRENT_DATE
			ORDER BY materiality_version DESC LIMIT 1`, tenantID, legalEntityID).Scan(&m.MaterialityID, &m.AggregateThreshold, &m.Currency)
		switch {
		case err == nil:
			mat = &m
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sum, err := domain.Summarise(legalEntityID, periodID, rows, mat)
	if err != nil {
		return nil, err
	}
	return &sum, nil
}
