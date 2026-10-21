package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/events"
)

// CertifyRun records a certifier's decision on a READY_TO_CERTIFY run, atomically
// with its state movement and outbox event.
//
// Maker-checker (Invariants 11/12): the deciding principal may not be the run's
// creator, nor anyone who appears on the run's transition history (the person who
// executed it, assigned an exception on it, and so on). A single actor can therefore
// never both produce and certify a result.
//
// A CERTIFY decision also writes the run's ControlCertification (Invariant 9,
// migration 000007) in the same transaction: authority is the certifier's live
// authorization-svc decision and is required — a certification whose authority
// cannot be identified is refused, not recorded without it.
//
// Idempotent on (tenant, Idempotency-Key) through command_idempotency, like
// BeginExecution: a repeat of the key for the same decision on the same run returns
// replay=true and the run as it now stands — a client retrying a CERTIFY whose
// response was lost gets its result, not a 412/409 on the moved-on run. The key used
// for another run or the other decision is a conflict.
func (s *PgStore) CertifyRun(ctx context.Context, tenantID, runID, actor, correlationID, idempotencyKey string, expectedVersion int,
	req domain.CertifyRequest, authority *domain.AuthoritySnapshot) (*domain.ControlRun, bool, error) {
	if req.Decision == domain.DecisionCertify && (authority == nil || authority.AccessDecisionID == "" || authority.PrincipalID != actor) {
		return nil, false, fmt.Errorf("%w: a certification must carry the certifier's own authorization decision", domain.ErrAuthorizationServiceUnavailable)
	}
	var out *domain.ControlRun
	apply := func(tx pgx.Tx) error {
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
		if err != nil || req.Decision != domain.DecisionCertify {
			return err
		}
		return recordCertification(ctx, tx, tenantID, *out, actor, correlationID, req.Reason, *authority)
	}
	replay := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		k := commandKey{key: idempotencyKey, command: certifyCommand(req.Decision), runID: runID}
		cur, err := lockRun(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if replay, err = lookupCommandKey(ctx, tx, tenantID, k); err != nil || replay {
			out = cur
			return err
		}
		if err := apply(tx); err != nil {
			return err
		}
		return recordCommandKey(ctx, tx, tenantID, k)
	})
	if err != nil {
		return nil, false, err
	}
	return out, replay, nil
}

// recordCertification writes the ControlCertification of run, which advanceTx has just
// moved to CERTIFIED inside tx, so it is committed or rolled back with the certification
// itself.
func recordCertification(ctx context.Context, tx pgx.Tx, tenantID string, run domain.ControlRun,
	certifier, correlationID, reason string, authority domain.AuthoritySnapshot) error {
	// Evidence package: the one sealed at execution (the latest, if reperformed).
	var packageID, digest string
	if err := tx.QueryRow(ctx, `SELECT package_id::text, digest FROM evidence_packages
		WHERE tenant_id = $1 AND run_id = $2 ORDER BY sealed_at DESC, package_id DESC LIMIT 1`, tenantID, run.RunID).
		Scan(&packageID, &digest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the run has no sealed evidence package to certify", domain.ErrInvalidTransition)
		}
		return err
	}

	var transitionID int64
	if err := tx.QueryRow(ctx, `SELECT transition_id FROM control_run_transitions
		WHERE tenant_id = $1 AND run_id = $2 AND dimension = 'CERTIFICATION' AND to_state = 'CERTIFIED'
		ORDER BY transition_id DESC LIMIT 1`, tenantID, run.RunID).Scan(&transitionID); err != nil {
		return fmt.Errorf("read certification transition: %w", err)
	}

	// Unresolved exceptions: everything not CLOSED, with the authority and target
	// period of the transition that last moved it (the waiver or carry-forward).
	rows, err := tx.Query(ctx, `
		SELECT e.exception_id::text, e.state, e.category, e.reason_code, e.severity, e.exposure::text, e.currency::text,
		       COALESCE(t.authority_ref, ''), COALESCE(t.carry_to_period, '')
		FROM control_exceptions e
		LEFT JOIN LATERAL (
			SELECT authority_ref, carry_to_period FROM exception_transitions
			WHERE tenant_id = e.tenant_id AND exception_id = e.exception_id
			ORDER BY transition_id DESC LIMIT 1) t ON true
		WHERE e.tenant_id = $1 AND e.run_id = $2 AND e.state <> 'CLOSED'
		ORDER BY e.exception_id`, tenantID, run.RunID)
	if err != nil {
		return err
	}
	unresolved := []domain.CertifiedException{}
	for rows.Next() {
		var e domain.CertifiedException
		if err := rows.Scan(&e.ExceptionID, &e.State, &e.Category, &e.ReasonCode, &e.Severity, &e.Exposure, &e.Currency,
			&e.AuthorityRef, &e.CarryToPeriod); err != nil {
			rows.Close()
			return err
		}
		x, ok := new(big.Rat).SetString(e.Exposure)
		if !ok {
			rows.Close()
			return fmt.Errorf("exposure %q is not a decimal", e.Exposure)
		}
		e.Exposure = domain.CanonicalAmount(x)
		unresolved = append(unresolved, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Impact assessment against the materiality policy the run pinned at creation.
	var mat *domain.MaterialityView2
	if run.MaterialityID != nil {
		var m domain.MaterialityView2
		if err := tx.QueryRow(ctx, `SELECT materiality_id::text, aggregate_threshold::text, currency::text
			FROM materiality_policies WHERE tenant_id = $1 AND materiality_id = $2`, tenantID, *run.MaterialityID).
			Scan(&m.MaterialityID, &m.AggregateThreshold, &m.Currency); err != nil {
			return fmt.Errorf("read pinned materiality policy: %w", err)
		}
		mat = &m
	}
	exposure, err := domain.AssessExposure(run.LegalEntityID, run.PeriodID, unresolved, mat)
	if err != nil {
		return err
	}

	authJSON, err := json.Marshal(authority)
	if err != nil {
		return err
	}
	unresolvedJSON, err := json.Marshal(unresolved)
	if err != nil {
		return err
	}
	exposureJSON, err := json.Marshal(exposure)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO control_certifications
		(tenant_id, run_id, run_version, certification_transition_id, certifier, authority_snapshot, result,
		 unresolved_exceptions, assessed_exposure, evidence_package_id, evidence_digest, reason, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		tenantID, run.RunID, run.Version, transitionID, certifier, authJSON, string(run.ResultState),
		unresolvedJSON, exposureJSON, packageID, digest, reason, correlationID)
	if err != nil {
		return fmt.Errorf("record certification: %w", err)
	}
	return nil
}

// GetCertification returns the run's ControlCertification, or ErrNotFound when the
// run was never certified (or was certified before migration 000007 recorded them).
func (s *PgStore) GetCertification(ctx context.Context, tenantID, runID string) (*domain.ControlCertification, error) {
	var c domain.ControlCertification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var authJSON, unresolvedJSON, exposureJSON []byte
		var result string
		if err := tx.QueryRow(ctx, `SELECT certification_id::text, run_id::text, run_version, certification_transition_id,
				certifier, authority_snapshot, result, unresolved_exceptions, assessed_exposure, certified_at,
				evidence_package_id::text, evidence_digest, reason
			FROM control_certifications WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID).
			Scan(&c.CertificationID, &c.RunID, &c.RunVersion, &c.CertificationTransitionID, &c.Certifier, &authJSON,
				&result, &unresolvedJSON, &exposureJSON, &c.CertifiedAt, &c.EvidencePackageID, &c.EvidenceDigest, &c.Reason); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		c.Result = domain.ResultState(result)
		if err := json.Unmarshal(authJSON, &c.Authority); err != nil {
			return err
		}
		if err := json.Unmarshal(unresolvedJSON, &c.UnresolvedExceptions); err != nil {
			return err
		}
		return json.Unmarshal(exposureJSON, &c.AssessedExposure)
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// revokeCommand is the command_idempotency name of RevokeCertification.
const revokeCommand = "REVOKE_CERTIFICATION"

// RevokeCertification withdraws a CERTIFIED run's certification (ZS-CONTROL-001
// §27, ZS-SVC-Z-001 RB-05), atomically with its transition, its revocation record
// and its outbox event. Only the certification dimension moves, CERTIFIED ->
// REVOKED; the lifecycle stays CERTIFIED and the result is untouched.
//
// Additive only ("prior certification retained"): the CERTIFICATION -> CERTIFIED
// transition row is never touched (control_run_transitions is append-only by
// trigger) and is referenced by the new control_certification_revocations row.
//
// Independence: the principal who certified the run cannot revoke it.
//
// Prospective (RB-05): nothing already decided on the certification is undone
// here. The close gate reads the run's state, so from now on it reports the
// control as blocking with reason CERTIFICATION_REVOKED; the event tells
// consumers the same.
//
// Idempotent on (tenant, Idempotency-Key): a repeat of the same key for the same
// run returns replay=true and the run as it now stands; the same key used for a
// different run or command is a conflict. The key is claimed only once every
// check has passed, so a refused revocation never consumes it.
func (s *PgStore) RevokeCertification(ctx context.Context, tenantID, runID, actor, correlationID, idempotencyKey string,
	expectedVersion int, req domain.RevokeCertificationRequest) (*domain.ControlRun, bool, error) {
	var out *domain.ControlRun
	replay := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var existingRun, existingCmd string
		err := tx.QueryRow(ctx, `SELECT run_id::text, command FROM command_idempotency
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, idempotencyKey).Scan(&existingRun, &existingCmd)
		switch {
		case err == nil:
			if existingRun != runID || existingCmd != revokeCommand {
				return fmt.Errorf("%w: idempotency key already used for a different command", domain.ErrConflict)
			}
			replay = true
			var r domain.ControlRun
			if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
				WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID), &r); err != nil {
				return err
			}
			out = &r
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

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
		if err := domain.ValidateCertificationRevocation(cur.LifecycleState, cur.CertificationState); err != nil {
			return err
		}

		// The certification being revoked: the latest CERTIFICATION -> CERTIFIED row.
		// Everyone who ever certified the run is a certifier for independence.
		rows, err := tx.Query(ctx, `SELECT transition_id, actor_id, occurred_at FROM control_run_transitions
			WHERE tenant_id = $1 AND run_id = $2 AND dimension = 'CERTIFICATION' AND to_state = 'CERTIFIED'
			ORDER BY transition_id`, tenantID, runID)
		if err != nil {
			return err
		}
		var certifiers []string
		var certTransitionID int64
		var certifiedBy string
		var certifiedAt time.Time
		for rows.Next() {
			if err := rows.Scan(&certTransitionID, &certifiedBy, &certifiedAt); err != nil {
				rows.Close()
				return err
			}
			certifiers = append(certifiers, certifiedBy)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(certifiers) == 0 {
			// A CERTIFIED run with no recorded certification would be a defect in
			// this service's own history; revoking it would revoke nothing traceable.
			return fmt.Errorf("run %s is CERTIFIED but has no recorded certification transition", runID)
		}
		if err := domain.CheckRevokerIndependence(actor, certifiers); err != nil {
			return err
		}

		var r domain.ControlRun
		if err := scanRun(tx.QueryRow(ctx, `UPDATE control_runs SET certification_state = 'REVOKED',
				version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND run_id = $2 AND lifecycle_state = 'CERTIFIED' AND certification_state = 'CERTIFIED'
			RETURNING `+runColumns, tenantID, runID), &r); err != nil {
			return err
		}
		if err := insertTransition(ctx, tx, tenantID, runID, "CERTIFICATION", string(domain.CertCertified),
			string(domain.CertRevoked), req.Reason, actor, correlationID); err != nil {
			return err
		}
		var revocationID string
		var revokedAt time.Time
		if err := tx.QueryRow(ctx, `INSERT INTO control_certification_revocations
				(tenant_id, run_id, certification_transition_id, certified_by, certified_at, reason, evidence_refs, revoked_by, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING revocation_id::text, revoked_at`,
			tenantID, runID, certTransitionID, certifiedBy, certifiedAt, req.Reason, req.EvidenceRefs, actor, correlationID,
		).Scan(&revocationID, &revokedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO command_idempotency (tenant_id, idempotency_key, command, run_id)
			VALUES ($1,$2,$3,$4)`, tenantID, idempotencyKey, revokeCommand, runID); err != nil {
			return err
		}
		if err := emit(ctx, tx, r, events.RunCertificationRevoked, actor, correlationID, map[string]any{
			"run_id": r.RunID, "control_definition_id": r.ControlDefinitionID, "period_id": r.PeriodID,
			"lifecycle_state": r.LifecycleState, "result_state": r.ResultState,
			"prior_certification_state": domain.CertCertified, "certification_state": r.CertificationState,
			"revocation_id": revocationID, "certification_transition_id": certTransitionID,
			"certified_by": certifiedBy, "certified_at": certifiedAt.UTC().Format(time.RFC3339Nano),
			"reason": req.Reason, "evidence_refs": req.EvidenceRefs,
			"effective_at": revokedAt.UTC().Format(time.RFC3339Nano), "version": r.Version,
		}); err != nil {
			return err
		}
		out = &r
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, replay, nil
}

// CloseGate returns each mandatory (close_gating) control with its latest run for
// the entity and period. Only the latest run counts: an earlier certified run does
// not survive a later run that failed or is still open.
func (s *PgStore) CloseGate(ctx context.Context, tenantID, legalEntityID, periodID string) (*domain.CloseGate, error) {
	var in []domain.GateInput
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT d.control_code, d.name, COALESCE(r.run_id::text,''), COALESCE(r.lifecycle_state,''),
			       COALESCE(r.result_state,''), COALESCE(r.certification_state,''),
			       COALESCE(to_char(v.revoked_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),''),
			       COALESCE(v.reason,'')
			FROM control_definitions d
			LEFT JOIN LATERAL (
				SELECT run_id, lifecycle_state, result_state, certification_state FROM control_runs
				WHERE tenant_id = d.tenant_id AND control_definition_id = d.control_definition_id
				  AND legal_entity_id = $2 AND period_id = $3
				ORDER BY created_at DESC, run_id DESC LIMIT 1) r ON true
			LEFT JOIN control_certification_revocations v ON v.tenant_id = d.tenant_id AND v.run_id = r.run_id
			WHERE d.tenant_id = $1 AND d.close_gating
			ORDER BY d.control_code`, tenantID, legalEntityID, periodID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g domain.GateInput
			if err := rows.Scan(&g.ControlCode, &g.ControlName, &g.RunID, &g.Lifecycle, &g.Result, &g.Certification,
				&g.RevokedAt, &g.RevocationReason); err != nil {
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
