package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Late data after certification (SPEC_DEVIATIONS A7; ZS-CONTROL-001 Invariant 14,
// scenarios 05 and 21). The engine's LateDataSweeper re-reads source watermarks;
// these are the reads and the one write it needs. Nothing here updates a run,
// a population or a certification: a moved watermark only ever ADDS an exception.

// RunRef names one run of one tenant.
type RunRef struct{ TenantID, RunID string }

// LateDataCandidates returns, across tenants, the latest run of each control,
// legal entity and period when that run is CERTIFIED with its certification
// standing (a revoked one already blocks the gate; a superseded one is not the
// latest). Runs never checked come first, then the least recently checked; a run
// checked at or after checkedBefore is skipped. The pool is a superuser, as for
// Sweep; each check is then made under the run's own tenant.
func (s *PgStore) LateDataCandidates(ctx context.Context, checkedBefore time.Time, batch int) ([]RunRef, error) {
	refs, err := s.collect(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (tenant_id, control_definition_id, legal_entity_id, period_id)
			       tenant_id, run_id, lifecycle_state, certification_state
			FROM control_runs
			ORDER BY tenant_id, control_definition_id, legal_entity_id, period_id, created_at DESC, run_id DESC)
		SELECT l.tenant_id::text, l.run_id::text
		FROM latest l
		LEFT JOIN LATERAL (SELECT MIN(checked_at) AS checked_at FROM control_watermark_checks c
			WHERE c.tenant_id = l.tenant_id AND c.run_id = l.run_id) c ON true
		WHERE l.lifecycle_state = 'CERTIFIED' AND l.certification_state = 'CERTIFIED'
		  AND (c.checked_at IS NULL OR c.checked_at < $1)
		ORDER BY c.checked_at NULLS FIRST, l.run_id
		LIMIT $2`, checkedBefore, batch)
	if err != nil {
		return nil, err
	}
	out := make([]RunRef, len(refs))
	for i, r := range refs {
		out[i] = RunRef{TenantID: r.tenant, RunID: r.id}
	}
	return out, nil
}

// RunExecutor is the principal who started the run's execution (its move to
// PREPARING). Populations are read on that principal's behalf: the source
// authorizes the human caller, not the control service
// (control-population-contract.md), so a re-check of the population that
// principal froze is made as them, and the source can still refuse it.
func (s *PgStore) RunExecutor(ctx context.Context, tenantID, runID string) (string, error) {
	var actor string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT actor_id FROM control_run_transitions
			WHERE tenant_id = $1 AND run_id = $2 AND dimension = 'LIFECYCLE' AND to_state = 'PREPARING'
			ORDER BY transition_id LIMIT 1`, tenantID, runID).Scan(&actor)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	})
	return actor, err
}

// Watermark check outcomes, as stored in control_watermark_checks.
const (
	WatermarkCurrent    = "CURRENT"
	WatermarkMoved      = "MOVED"
	WatermarkUnverified = "UNVERIFIED"
)

// WatermarkCheck is the result of re-reading one side's source watermark.
type WatermarkCheck struct {
	RunID    string
	Snapshot domain.PopulationSnapshot // the frozen side being checked
	Observed string                    // the source's current watermark; empty when Error is set
	Error    string                    // why the source could not be read
	// OwnerRole owns a late-data exception (the control definition's owner role).
	OwnerRole     string
	At            time.Time
	Actor         string
	CorrelationID string
}

// CheckOutcome is what RecordWatermarkCheck did.
type CheckOutcome struct {
	Outcome string // CURRENT, MOVED or UNVERIFIED; empty when the run was no longer eligible
	// Opened is the late-data exception opened by this check, if any.
	Opened *domain.ControlException
}

// RecordWatermarkCheck records one side's check and, when the watermark has moved
// past the frozen one, opens a LATE_DATA exception on the run, in ONE transaction
// under the run's row lock (so concurrent sweepers serialize and open it once).
//
// The run must still be the latest run of its control, entity and period and be
// CERTIFIED with its certification standing; otherwise nothing is recorded. A new
// exception is opened only when no unresolved late-data exception exists for the
// side and the observed watermark is not the one the last such exception was
// opened for (that one was resolved under authority; a further move is new).
//
// Additive only: the run row, its transitions, its population and its
// certification are read, never written.
func (s *PgStore) RecordWatermarkCheck(ctx context.Context, tenantID string, c WatermarkCheck) (CheckOutcome, error) {
	var res CheckOutcome
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var run domain.ControlRun
		if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, tenantID, c.RunID), &run); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if run.LifecycleState != domain.LifecycleCertified || run.CertificationState != domain.CertCertified {
			return nil
		}
		var latest string
		if err := tx.QueryRow(ctx, `SELECT run_id::text FROM control_runs
			WHERE tenant_id = $1 AND control_definition_id = $2 AND legal_entity_id = $3 AND period_id = $4
			ORDER BY created_at DESC, run_id DESC LIMIT 1`,
			tenantID, run.ControlDefinitionID, run.LegalEntityID, run.PeriodID).Scan(&latest); err != nil {
			return err
		}
		if latest != run.RunID {
			return nil
		}
		if c.Snapshot.RunID != run.RunID {
			return fmt.Errorf("%w: population %s is not run %s's", domain.ErrInvalidArgument, c.Snapshot.PopulationID, run.RunID)
		}

		var observed *string
		switch {
		case c.Error != "":
			res.Outcome = WatermarkUnverified
		case c.Observed == c.Snapshot.Watermark:
			res.Outcome, observed = WatermarkCurrent, &c.Observed
		default:
			res.Outcome, observed = WatermarkMoved, &c.Observed
		}

		var reported, lateID *string
		if res.Outcome == WatermarkMoved {
			var prior *string
			if err := tx.QueryRow(ctx, `SELECT reported_watermark FROM control_watermark_checks
				WHERE tenant_id = $1 AND run_id = $2 AND side = $3`, tenantID, run.RunID, string(c.Snapshot.Side)).
				Scan(&prior); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			var unresolved bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM control_exceptions
				WHERE tenant_id = $1 AND run_id = $2 AND side = $3
				  AND category = 'LATE_DATA' AND reason_code = $4
				  AND state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY'))`,
				tenantID, run.RunID, string(c.Snapshot.Side), domain.ReasonLateDataAfterCertification).Scan(&unresolved); err != nil {
				return err
			}
			if !unresolved && (prior == nil || *prior != c.Observed) {
				e := domain.LateDataException(run, c.Snapshot, c.Observed, c.OwnerRole, c.At)
				if err := openException(ctx, tx, run, e, "opened by late-data check: source changed after certification",
					c.Actor, c.CorrelationID, map[string]any{
						"side": string(c.Snapshot.Side), "population_id": c.Snapshot.PopulationID, "spec_ref": c.Snapshot.SpecRef,
						"frozen_watermark": c.Snapshot.Watermark, "observed_watermark": c.Observed,
						"certification_state": run.CertificationState,
					}); err != nil {
					return err
				}
				res.Opened = &e
				reported, lateID = &c.Observed, &e.ExceptionID
			}
		}

		_, err := tx.Exec(ctx, `INSERT INTO control_watermark_checks
				(tenant_id, run_id, side, population_id, checked_at, outcome, observed_watermark, error, reported_watermark, late_exception_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (run_id, side) DO UPDATE SET
				checked_at = EXCLUDED.checked_at, outcome = EXCLUDED.outcome,
				observed_watermark = EXCLUDED.observed_watermark, error = EXCLUDED.error,
				reported_watermark = COALESCE(EXCLUDED.reported_watermark, control_watermark_checks.reported_watermark),
				late_exception_id = COALESCE(EXCLUDED.late_exception_id, control_watermark_checks.late_exception_id)`,
			tenantID, run.RunID, string(c.Snapshot.Side), c.Snapshot.PopulationID, c.At, res.Outcome, observed, c.Error, reported, lateID)
		return err
	})
	if err != nil {
		return CheckOutcome{}, err
	}
	return res, nil
}
