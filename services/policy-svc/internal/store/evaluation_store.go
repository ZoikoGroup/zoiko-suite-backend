package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/policy-svc/internal/domain"
)

// evaluationColumns selects an evaluation and its ledger delivery state.
const evaluationColumns = `
	e.evaluation_id, e.tenant_id, e.decision_id, e.legal_entity_id, e.policy_type,
	COALESCE(e.requested_by_principal_id, ''), e.input_fingerprint, e.action_context,
	e.as_of, e.known_at, e.result, e.outcome, COALESCE(e.policy_id::text, ''), COALESCE(e.policy_version_id::text, ''),
	COALESCE(e.policy_code, ''), COALESCE(e.artifact_digest, ''), e.rule_basis, e.selected_rule_ids,
	e.evaluator_version, e.correlation_id, COALESCE(e.causation_id, ''),
	COALESCE(e.request_id, ''), e.evaluated_at,
	CASE
		WHEN o.delivered_at IS NOT NULL THEN 'RECORDED'
		WHEN o.failed_at IS NOT NULL THEN 'FAILED'
		ELSE 'PENDING'
	END,
	COALESCE(e.contract_version, ''), COALESCE(e.decision_outcome, ''), e.reason_codes, e.valid_until,
	e.decision_envelope
	FROM policy_evaluations e
	LEFT JOIN policy_evidence_outbox o ON o.evaluation_id = e.evaluation_id`

func scanEvaluation(row pgx.Row) (*domain.Evaluation, error) {
	e := &domain.Evaluation{}
	var envelope *string
	err := row.Scan(
		&e.EvaluationID, &e.TenantID, &e.DecisionID, &e.LegalEntityID, &e.PolicyType,
		&e.RequestedByPrincipalID, &e.InputFingerprint, &e.ActionContext,
		&e.AsOf, &e.KnownAt, &e.Result, &e.Outcome, &e.PolicyID, &e.PolicyVersionID,
		&e.PolicyCode, &e.ArtifactDigest, &e.RuleBasis, &e.SelectedRuleIDs,
		&e.EvaluatorVersion, &e.CorrelationID, &e.CausationID,
		&e.RequestID, &e.EvaluatedAt, &e.LedgerStatus,
		&e.ContractVersion, &e.DecisionOutcome, &e.ReasonCodes, &e.ValidUntil,
		&envelope,
	)
	if envelope != nil && *envelope != "" {
		e.DecisionEnvelope = json.RawMessage(*envelope)
	}
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
		evaluatedAt := ev.EvaluatedAt
		if evaluatedAt.IsZero() {
			evaluatedAt = s.now()
		}
		reasons := ev.ReasonCodes
		if reasons == nil {
			reasons = []string{}
		}
		// Stored as text, byte for byte: the signed envelope a replay serves
		// must be the one first answered.
		var envelope *string
		if len(ev.DecisionEnvelope) > 0 {
			text := string(ev.DecisionEnvelope)
			envelope = &text
		}
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO policy_evaluations (
				evaluation_id,
				tenant_id, decision_id, legal_entity_id, policy_type, requested_by_principal_id,
				input_fingerprint, action_context, as_of, known_at, result, outcome,
				policy_id, policy_version_id, policy_code, artifact_digest, rule_basis,
				selected_rule_ids, evaluator_version, correlation_id, causation_id, request_id,
				evaluated_at, contract_version, decision_outcome, reason_codes, valid_until, decision_envelope
			) VALUES (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
				$13::uuid, $14::uuid, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28)
			ON CONFLICT (tenant_id, decision_id) DO NOTHING
			RETURNING evaluation_id;`,
			nullableString(ev.EvaluationID),
			ev.TenantID, ev.DecisionID, ev.LegalEntityID, ev.PolicyType, nullableString(ev.RequestedByPrincipalID),
			ev.InputFingerprint, action, ev.AsOf, ev.KnownAt, ev.Result, ev.Outcome,
			nullableString(ev.PolicyID), nullableString(ev.PolicyVersionID), nullableString(ev.PolicyCode), nullableString(ev.ArtifactDigest), ev.RuleBasis,
			ev.SelectedRuleIDs, ev.EvaluatorVersion, ev.CorrelationID, nullableString(ev.CausationID), nullableString(ev.RequestID),
			evaluatedAt, nullableString(ev.ContractVersion), nullableString(ev.DecisionOutcome), reasons, ev.ValidUntil, envelope,
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
	var out []PendingEvidence
	err := s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
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
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p PendingEvidence
			if err := rows.Scan(&p.EvaluationID, &p.TenantID, &p.Attempts); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.domainError("ClaimEvidence", err)
	}
	return out, nil
}

// RedeliverEvidence puts evidence the ledger refused (FAILED) back on the
// relay's queue, in the caller's tenant scope, and records who asked and why
// as policy.evidence.redelivery_requested in the same transaction. Evidence
// still pending is left alone (changed=false); evidence already recorded is
// ErrEvidenceAlreadyRecorded.
func (s *PgStore) RedeliverEvidence(ctx context.Context, p domain.EvidenceRedeliveryParams) (bool, error) {
	var changed bool
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var delivered, failed *time.Time
		err := tx.QueryRow(ctx, `
			SELECT delivered_at, failed_at FROM policy_evidence_outbox
			WHERE evaluation_id = $1 FOR UPDATE;`, p.EvaluationID).Scan(&delivered, &failed)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrEvaluationNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case delivered != nil:
			return domain.ErrEvidenceAlreadyRecorded
		case failed == nil:
			return nil // still pending: the relay already has it
		}
		if _, err := tx.Exec(ctx, `
			UPDATE policy_evidence_outbox
			SET failed_at = NULL, attempts = 0, next_attempt_at = $2,
			    last_error = 'redelivery requested: ' || COALESCE(last_error, '')
			WHERE evaluation_id = $1;`, p.EvaluationID, s.now()); err != nil {
			return err
		}
		changed = true
		key := p.Meta.IdempotencyKey
		if key == "" {
			key = "policy.evidence.redelivery_requested:" + p.EvaluationID + ":" + s.now().Format(time.RFC3339Nano)
		}
		tenant := p.TenantID
		return s.enqueue(ctx, tx, OutboxEvent{
			EventType: "policy.evidence.redelivery_requested", TenantID: &tenant, ActorID: p.ActorID,
			CorrelationID: p.Meta.CorrelationID, CausationID: p.Meta.CausationID, IdempotencyKey: key,
			JurisdictionContext: p.Meta.JurisdictionContext, AggregateID: p.EvaluationID, AggregateVersion: 1,
			Payload: mustJSON(map[string]any{
				"evidence_id": p.EvaluationID, "requested_by_principal_id": p.ActorID, "reason": p.Reason,
			}),
		})
	})
	if err != nil {
		return false, s.domainError("RedeliverEvidence", err)
	}
	return changed, nil
}

// relayExec runs one statement against the evidence outbox as the relay.
func (s *PgStore) relayExec(ctx context.Context, op, sql string, args ...any) error {
	if err := s.withRelay(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		return s.domainError(op, err)
	}
	return nil
}

// MarkEvidenceDelivered records that the ledger holds the evaluation.
func (s *PgStore) MarkEvidenceDelivered(ctx context.Context, evaluationID string) error {
	return s.relayExec(ctx, "MarkEvidenceDelivered", `
		UPDATE policy_evidence_outbox SET delivered_at = $2, last_error = NULL
		WHERE evaluation_id = $1;`, evaluationID, s.now())
}

// MarkEvidenceRetry records a failed attempt and when to try again.
func (s *PgStore) MarkEvidenceRetry(ctx context.Context, evaluationID, lastError string, next time.Time) error {
	return s.relayExec(ctx, "MarkEvidenceRetry", `
		UPDATE policy_evidence_outbox SET last_error = $2, next_attempt_at = $3
		WHERE evaluation_id = $1;`, evaluationID, lastError, next)
}

// MarkEvidenceFailed parks an evaluation the ledger refused for good. It is
// kept (and counted in EvidenceBacklog) for an operator, never retried.
func (s *PgStore) MarkEvidenceFailed(ctx context.Context, evaluationID, lastError string) error {
	return s.relayExec(ctx, "MarkEvidenceFailed", `
		UPDATE policy_evidence_outbox SET last_error = $2, failed_at = $3
		WHERE evaluation_id = $1;`, evaluationID, lastError, s.now())
}

// EvidenceBacklog counts evaluations not yet in the ledger, and those parked.
func (s *PgStore) EvidenceBacklog(ctx context.Context) (pending, failed int64, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE delivered_at IS NULL AND failed_at IS NULL),
				COUNT(*) FILTER (WHERE failed_at IS NOT NULL)
			FROM policy_evidence_outbox;`).Scan(&pending, &failed)
	})
	if err != nil {
		return 0, 0, s.domainError("EvidenceBacklog", err)
	}
	return pending, failed, nil
}
