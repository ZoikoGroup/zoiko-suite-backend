package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/policy-svc/internal/domain"
)

// evaluationColumns selects an evaluation and its ledger delivery state.
const evaluationColumns = `
	e.evaluation_id, e.tenant_id, e.decision_id, e.legal_entity_id, e.policy_type,
	COALESCE(e.requested_by_principal_id, ''), e.input_fingerprint, e.action_context,
	e.as_of, e.known_at, e.result, e.outcome, e.policy_id, e.policy_version_id,
	e.policy_code, e.artifact_digest, e.rule_basis, e.selected_rule_ids,
	e.evaluator_version, e.correlation_id, COALESCE(e.causation_id, ''),
	COALESCE(e.request_id, ''), e.evaluated_at,
	CASE
		WHEN o.delivered_at IS NOT NULL THEN 'RECORDED'
		WHEN o.failed_at IS NOT NULL THEN 'FAILED'
		ELSE 'PENDING'
	END
	FROM policy_evaluations e
	LEFT JOIN policy_evidence_outbox o ON o.evaluation_id = e.evaluation_id`

func scanEvaluation(row pgx.Row) (*domain.Evaluation, error) {
	e := &domain.Evaluation{}
	err := row.Scan(
		&e.EvaluationID, &e.TenantID, &e.DecisionID, &e.LegalEntityID, &e.PolicyType,
		&e.RequestedByPrincipalID, &e.InputFingerprint, &e.ActionContext,
		&e.AsOf, &e.KnownAt, &e.Result, &e.Outcome, &e.PolicyID, &e.PolicyVersionID,
		&e.PolicyCode, &e.ArtifactDigest, &e.RuleBasis, &e.SelectedRuleIDs,
		&e.EvaluatorVersion, &e.CorrelationID, &e.CausationID,
		&e.RequestID, &e.EvaluatedAt, &e.LedgerStatus,
	)
	return e, err
}

// SaveEvaluation stores an evaluation's evidence and queues its delivery to
// governance-decision-log-svc, in one transaction: once Evaluate answers 200
// the evidence is durable and its delivery cannot be lost (decision C6).
//
// A concurrent or repeated save for the same (tenant, decision_id) stores
// nothing and returns the existing row with created=false; the caller
// decides whether it is the same request.
func (s *PgStore) SaveEvaluation(ctx context.Context, ev *domain.Evaluation) (*domain.Evaluation, bool, error) {
	var out *domain.Evaluation
	var created bool
	err := s.withRLS(ctx, ev.TenantID, func(tx pgx.Tx) error {
		action := ev.ActionContext
		if len(action) == 0 {
			action = []byte(`{}`)
		}
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO policy_evaluations (
				tenant_id, decision_id, legal_entity_id, policy_type, requested_by_principal_id,
				input_fingerprint, action_context, as_of, known_at, result, outcome,
				policy_id, policy_version_id, policy_code, artifact_digest, rule_basis,
				selected_rule_ids, evaluator_version, correlation_id, causation_id, request_id,
				evaluated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
			ON CONFLICT (tenant_id, decision_id) DO NOTHING
			RETURNING evaluation_id;`,
			ev.TenantID, ev.DecisionID, ev.LegalEntityID, ev.PolicyType, nullableString(ev.RequestedByPrincipalID),
			ev.InputFingerprint, action, ev.AsOf, ev.KnownAt, ev.Result, ev.Outcome,
			ev.PolicyID, ev.PolicyVersionID, ev.PolicyCode, ev.ArtifactDigest, ev.RuleBasis,
			ev.SelectedRuleIDs, ev.EvaluatorVersion, ev.CorrelationID, nullableString(ev.CausationID), nullableString(ev.RequestID),
			s.now(),
		).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			existing, err := scanEvaluation(tx.QueryRow(ctx, `SELECT `+evaluationColumns+` WHERE e.tenant_id = $1 AND e.decision_id = $2;`, ev.TenantID, ev.DecisionID))
			if err != nil {
				return err
			}
			out = existing
			return nil
		case err != nil:
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO policy_evidence_outbox (evaluation_id, tenant_id, next_attempt_at, created_at)
			VALUES ($1, $2, $3, $3);`, id, ev.TenantID, s.now()); err != nil {
			return err
		}
		saved, err := scanEvaluation(tx.QueryRow(ctx, `SELECT `+evaluationColumns+` WHERE e.evaluation_id = $1;`, id))
		if err != nil {
			return err
		}
		out, created = saved, true
		return nil
	})
	if err != nil {
		return nil, false, s.domainError("SaveEvaluation", err)
	}
	return out, created, nil
}

// FindEvaluationByDecision returns the tenant's evaluation for decision_id.
func (s *PgStore) FindEvaluationByDecision(ctx context.Context, tenantID, decisionID string) (*domain.Evaluation, error) {
	return s.findEvaluation(ctx, tenantID, `e.decision_id = $1`, decisionID)
}

// FindEvaluationByID returns the tenant's evaluation with the given evidence id.
func (s *PgStore) FindEvaluationByID(ctx context.Context, tenantID, evaluationID string) (*domain.Evaluation, error) {
	return s.findEvaluation(ctx, tenantID, `e.evaluation_id = $1`, evaluationID)
}

func (s *PgStore) findEvaluation(ctx context.Context, tenantID, where, arg string) (*domain.Evaluation, error) {
	var out *domain.Evaluation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		out, scanErr = scanEvaluation(tx.QueryRow(ctx, `SELECT `+evaluationColumns+` WHERE `+where+`;`, arg))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvaluationNotFound
	}
	if err != nil {
		return nil, s.domainError("findEvaluation", err)
	}
	return out, nil
}

// PendingEvidence is one evaluation awaiting delivery to the ledger.
type PendingEvidence struct {
	EvaluationID string
	TenantID     string
	Attempts     int
}

// ClaimEvidence leases up to limit undelivered evaluations whose next attempt
// is due. The lease pushes next_attempt_at forward, so another instance does
// not pick the same rows while this one is delivering them.
func (s *PgStore) ClaimEvidence(ctx context.Context, limit int, lease time.Duration) ([]PendingEvidence, error) {
	now := s.now()
	rows, err := s.pool.Query(ctx, `
		UPDATE policy_evidence_outbox
		SET next_attempt_at = $3, attempts = attempts + 1
		WHERE evaluation_id IN (
			SELECT evaluation_id FROM policy_evidence_outbox
			WHERE delivered_at IS NULL AND failed_at IS NULL AND next_attempt_at <= $1
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING evaluation_id, tenant_id, attempts;`, now, limit, now.Add(lease))
	if err != nil {
		return nil, s.domainError("ClaimEvidence", err)
	}
	defer rows.Close()
	var out []PendingEvidence
	for rows.Next() {
		var p PendingEvidence
		if err := rows.Scan(&p.EvaluationID, &p.TenantID, &p.Attempts); err != nil {
			return nil, s.domainError("ClaimEvidence", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkEvidenceDelivered records that the ledger holds the evaluation.
func (s *PgStore) MarkEvidenceDelivered(ctx context.Context, evaluationID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE policy_evidence_outbox SET delivered_at = $2, last_error = NULL
		WHERE evaluation_id = $1;`, evaluationID, s.now())
	if err != nil {
		return s.domainError("MarkEvidenceDelivered", err)
	}
	return nil
}

// MarkEvidenceRetry records a failed attempt and when to try again.
func (s *PgStore) MarkEvidenceRetry(ctx context.Context, evaluationID, lastError string, next time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE policy_evidence_outbox SET last_error = $2, next_attempt_at = $3
		WHERE evaluation_id = $1;`, evaluationID, lastError, next)
	if err != nil {
		return s.domainError("MarkEvidenceRetry", err)
	}
	return nil
}

// MarkEvidenceFailed parks an evaluation the ledger refused for good. It is
// kept (and counted in EvidenceBacklog) for an operator, never retried.
func (s *PgStore) MarkEvidenceFailed(ctx context.Context, evaluationID, lastError string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE policy_evidence_outbox SET last_error = $2, failed_at = $3
		WHERE evaluation_id = $1;`, evaluationID, lastError, s.now())
	if err != nil {
		return s.domainError("MarkEvidenceFailed", err)
	}
	return nil
}

// EvidenceBacklog counts evaluations not yet in the ledger, and those parked.
func (s *PgStore) EvidenceBacklog(ctx context.Context) (pending, failed int64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE delivered_at IS NULL AND failed_at IS NULL),
			COUNT(*) FILTER (WHERE failed_at IS NOT NULL)
		FROM policy_evidence_outbox;`).Scan(&pending, &failed)
	if err != nil {
		return 0, 0, s.domainError("EvidenceBacklog", err)
	}
	return pending, failed, nil
}
