// Lifecycle commands, evidence reads and the definition lifecycle (Group 2
// completion pass, 21 Oct 2026).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
	"zoiko.io/workflow-svc/internal/outbox"
)

func firstCmd(cmd []domain.LifecycleCommand) domain.LifecycleCommand {
	if len(cmd) > 0 {
		return cmd[0]
	}
	return domain.LifecycleCommand{}
}

// EscalateWorkflow transitions PENDING -> ESCALATED. Idempotent if already
// ESCALATED. The current stage is kept: an escalated workflow stays
// decidable, by the stage's approver and by the instance's escalation target.
func (s *PgStore) EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error) {
	current, err := s.FindWorkflowByID(ctx, workflowInstanceID)
	if err != nil {
		return nil, false, err
	}
	if current.WorkflowStatus == domain.WorkflowStatusEscalated {
		return current, false, nil
	}
	if current.WorkflowStatus != domain.WorkflowStatusPending {
		return nil, false, domain.ErrInvalidTransition
	}
	return s.transitionInstanceStatus(ctx, current, domain.WorkflowStatusEscalated, actorPrincipalID, current.CurrentStage, firstCmd(cmd))
}

// CancelWorkflow transitions PENDING or ESCALATED -> CANCELLED. Idempotent if
// already CANCELLED. Illegal from a terminal status.
func (s *PgStore) CancelWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error) {
	current, err := s.FindWorkflowByID(ctx, workflowInstanceID)
	if err != nil {
		return nil, false, err
	}
	if current.WorkflowStatus == domain.WorkflowStatusCancelled {
		return current, false, nil
	}
	if current.WorkflowStatus != domain.WorkflowStatusPending && current.WorkflowStatus != domain.WorkflowStatusEscalated {
		return nil, false, domain.ErrInvalidTransition
	}
	return s.transitionInstanceStatus(ctx, current, domain.WorkflowStatusCancelled, actorPrincipalID, 0, firstCmd(cmd))
}

// ExpireWorkflow transitions PENDING or ESCALATED -> EXPIRED once due_at has
// passed (R-001 Expire). A timeout never counts as approval: the instance
// ends, and the subject must start a new approval. Idempotent.
func (s *PgStore) ExpireWorkflow(ctx context.Context, workflowInstanceID string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error) {
	current, err := s.FindWorkflowByID(ctx, workflowInstanceID)
	if err != nil {
		return nil, false, err
	}
	if current.WorkflowStatus == domain.WorkflowStatusExpired {
		return current, false, nil
	}
	if current.WorkflowStatus != domain.WorkflowStatusPending && current.WorkflowStatus != domain.WorkflowStatusEscalated {
		return nil, false, domain.ErrInvalidTransition
	}
	if current.DueAt == nil || current.DueAt.After(time.Now()) {
		return nil, false, domain.ErrInvalidTransition
	}
	return s.transitionInstanceStatus(ctx, current, domain.WorkflowStatusExpired, systemActor, 0, firstCmd(cmd))
}

// systemActor is the actor of a transition the service makes itself (a timer).
const systemActor = "system:workflow-svc"

func (s *PgStore) transitionInstanceStatus(ctx context.Context, current *domain.WorkflowInstance, toState, actorPrincipalID string, newCurrentStage int, cmd domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error) {
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if replay, err := replayedCommand(ctx, tx, current.WorkflowInstanceID, cmd.Meta); err != nil || replay {
		if err != nil {
			return nil, false, err
		}
		return current, false, nil
	}

	updated, err := scanInstance(tx.QueryRow(ctx, `
		UPDATE workflow_instances
		SET workflow_status = $1, current_stage = $2,
		    completed_at = CASE WHEN $1::VARCHAR IN ('APPROVED','REJECTED','CANCELLED','INVALIDATED','EXPIRED') THEN NOW() ELSE completed_at END,
		    escalated_at = CASE WHEN $1::VARCHAR = 'ESCALATED' THEN NOW() ELSE escalated_at END,
		    next_action_at = CASE WHEN $1::VARCHAR = 'PENDING' THEN next_action_at ELSE NULL END,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_instance_id = $3 AND row_version = $4
		RETURNING `+instanceColumns, toState, newCurrentStage, current.WorkflowInstanceID, current.RowVersion))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrConcurrencyConflict
		}
		s.log.Error("pg transitionInstanceStatus: update failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	rationale := fmt.Sprintf("%s -> %s by %s", current.WorkflowStatus, toState, actorPrincipalID)
	if cmd.ReasonCode != "" {
		rationale += " code=" + cmd.ReasonCode
	}
	if cmd.Rationale != nil && *cmd.Rationale != "" {
		rationale = *cmd.Rationale
	}
	cycle := current.ReviewCycle
	var stageOrder *int
	if current.CurrentStage > 0 {
		so := current.CurrentStage
		stageOrder = &so
	}
	corr := corrOf(current, cmd.Meta)
	if err := insertTransitionRecord(ctx, tx, transitionRecord{
		InstanceID: current.WorkflowInstanceID, From: current.WorkflowStatus, To: toState, ActedBy: actorPrincipalID,
		Rationale: &rationale, CorrelationID: corr, StageOrder: stageOrder, ReviewCycle: &cycle,
		IdempotencyKey: cmd.Meta.IdempotencyKey, RequestHash: cmd.Meta.RequestHash,
		Authority: cmd.Authority, DecisionID: cmd.Meta.DecisionID,
	}); err != nil {
		if errors.Is(err, errIdempotentRetry) {
			return current, false, nil
		}
		return nil, false, err
	}

	headers := eventHeaders(updated, nil, cmd.Authority, cmd.Meta)
	emit := func(eventType string, payload map[string]any) error {
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "workflow_instance", AggregateID: updated.WorkflowInstanceID,
			EventType: eventType, TenantID: updated.TenantID, LegalEntityID: updated.LegalEntityID, ActorID: &actorPrincipalID,
			CorrelationID: corr, Headers: headers, Payload: payload})
	}
	switch toState {
	case domain.WorkflowStatusEscalated:
		err = emit("workflow.escalated", map[string]any{"workflow_instance_id": updated.WorkflowInstanceID,
			"current_stage": updated.CurrentStage, "escalation_target": updated.EscalationTarget, "reason_code": cmd.ReasonCode})
	case domain.WorkflowStatusCancelled, domain.WorkflowStatusExpired:
		// Doc 03 §8.4 names no separate cancellation event: a terminal
		// status is reported as workflow.completed with its status (C3,
		// decided 21 Oct 2026). EXPIRED also gets its own event so a
		// subject owner can react to a lapsed approval.
		if toState == domain.WorkflowStatusExpired {
			if err = emit("workflow.expired", map[string]any{"workflow_instance_id": updated.WorkflowInstanceID,
				"due_at": current.DueAt, "expired_at": updated.CompletedAt}); err != nil {
				break
			}
		}
		err = emit("workflow.completed", map[string]any{"workflow_instance_id": updated.WorkflowInstanceID,
			"workflow_status": updated.WorkflowStatus, "completed_at": updated.CompletedAt})
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	actionType := "WORKFLOW_" + strings.ToUpper(toState)
	switch toState {
	case domain.WorkflowStatusEscalated:
		actionType = "WORKFLOW_ESCALATE"
	case domain.WorkflowStatusCancelled:
		actionType = "WORKFLOW_CANCEL"
	case domain.WorkflowStatusExpired:
		actionType = "WORKFLOW_EXPIRE"
	}
	if err := writeLedgerTx(ctx, tx, updated, actorPrincipalID, actionType, toState, nil, cmd.Authority, cmd.Meta,
		map[string]any{"from_status": current.WorkflowStatus, "reason_code": cmd.ReasonCode, "rationale": rationale,
			"review_cycle": cycle}); err != nil {
		if errors.Is(err, errIdempotentRetry) {
			return current, false, nil
		}
		return nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg transitionInstanceStatus: commit failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, true, nil
}

// EscalateToWorkflow routes a PENDING or ESCALATED instance to an escalation
// target (R-001 §4.2). The current stage is kept and the target may decide
// it: escalation used to zero current_stage and stamp completed_at, which
// left the instance unable to resume — the dead end of row 28.
func (s *PgStore) EscalateToWorkflow(ctx context.Context, params domain.EscalateToWorkflowParams) (*domain.WorkflowInstance, bool, error) {
	current, err := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
	if err != nil {
		return nil, false, err
	}
	if current.WorkflowStatus == domain.WorkflowStatusEscalated && current.EscalationTarget != nil && *current.EscalationTarget == params.EscalationTarget {
		return current, false, nil
	}
	if current.WorkflowStatus != domain.WorkflowStatusPending && current.WorkflowStatus != domain.WorkflowStatusEscalated {
		return nil, false, domain.ErrInvalidTransition
	}
	if params.EscalationTarget == current.InitiatedBy {
		return nil, false, domain.ErrSelfApprovalNotAllowed
	}

	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	updated, err := scanInstance(tx.QueryRow(ctx, `
		UPDATE workflow_instances
		SET workflow_status = 'ESCALATED', escalation_target = $1, escalated_at = NOW(), next_action_at = NULL,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_instance_id = $2 AND row_version = $3
		RETURNING `+instanceColumns, params.EscalationTarget, params.WorkflowInstanceID, current.RowVersion))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrConcurrencyConflict
		}
		s.log.Error("pg EscalateToWorkflow: update failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	meta := params.Meta
	if meta.CorrelationID == "" {
		meta.CorrelationID = params.CorrelationID
	}
	corr := corrOf(updated, meta)
	reason := params.ReasonCode
	if reason == "" {
		reason = "MANUAL_ESCALATION"
	}
	rationale := fmt.Sprintf("escalated to %s by %s: code=%s", params.EscalationTarget, params.ActorPrincipalID, reason)
	if params.Narrative != nil && *params.Narrative != "" {
		rationale += " narrative=" + *params.Narrative
	}
	cycle := current.ReviewCycle
	so := current.CurrentStage
	if err := insertTransitionRecord(ctx, tx, transitionRecord{
		InstanceID: params.WorkflowInstanceID, From: current.WorkflowStatus, To: domain.WorkflowStatusEscalated,
		ActedBy: params.ActorPrincipalID, Rationale: &rationale, CorrelationID: corr, StageOrder: &so, ReviewCycle: &cycle,
		IdempotencyKey: meta.IdempotencyKey, RequestHash: meta.RequestHash, Authority: params.Authority, DecisionID: meta.DecisionID,
	}); err != nil {
		if errors.Is(err, errIdempotentRetry) {
			return current, false, nil
		}
		return nil, false, err
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "workflow_instance", AggregateID: params.WorkflowInstanceID,
		EventType: "workflow.escalated", TenantID: updated.TenantID, LegalEntityID: updated.LegalEntityID,
		ActorID: &params.ActorPrincipalID, CorrelationID: corr, Headers: eventHeaders(updated, nil, params.Authority, meta),
		Payload: map[string]any{"workflow_instance_id": updated.WorkflowInstanceID, "escalation_target": params.EscalationTarget,
			"current_stage": updated.CurrentStage, "reason_code": reason}}); err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := writeLedgerTx(ctx, tx, updated, params.ActorPrincipalID, "WORKFLOW_ESCALATE", domain.WorkflowStatusEscalated, nil,
		params.Authority, meta, map[string]any{"escalation_target": params.EscalationTarget, "reason_code": reason,
			"from_status": current.WorkflowStatus, "rationale": rationale}); err != nil {
		if errors.Is(err, errIdempotentRetry) {
			return current, false, nil
		}
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg EscalateToWorkflow: commit failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, true, nil
}

// RequestChanges reopens a REJECTED or ESCALATED instance for a new review
// cycle: every stage is reset to PENDING, stage 1 becomes current (its
// deadline restarts), quorum votes start afresh (they are kept per cycle,
// append-only), and the transition records the true from-state and the
// requester. It used to set the instance back to PENDING but leave the
// stages REJECTED, so no approver could act on the reopened workflow.
func (s *PgStore) RequestChanges(ctx context.Context, workflowInstanceID, actorPrincipalID, rationale string, causationID *string, cmd ...domain.LifecycleCommand) error {
	c := firstCmd(cmd)
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := lockInstance(ctx, tx, workflowInstanceID)
	if err != nil {
		return err
	}
	if replay, err := replayedCommand(ctx, tx, workflowInstanceID, c.Meta); err != nil || replay {
		return err
	}
	if cur.WorkflowStatus != domain.WorkflowStatusRejected && cur.WorkflowStatus != domain.WorkflowStatusEscalated {
		return domain.ErrInvalidTransition
	}
	var firstHours *int
	if err := tx.QueryRow(ctx, `SELECT due_hours FROM workflow_stages WHERE workflow_instance_id = $1 AND stage_order = 1`, workflowInstanceID).Scan(&firstHours); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	var due *time.Time
	if firstHours != nil {
		t := time.Now().UTC().Add(time.Duration(*firstHours) * time.Hour)
		due = &t
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workflow_stages
		SET stage_status = 'PENDING', acted_at = NULL, acted_by = NULL, rationale = NULL,
		    due_at = CASE WHEN stage_order = 1 THEN $2::timestamptz ELSE NULL END,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_instance_id = $1`, workflowInstanceID, due); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	updated, err := scanInstance(tx.QueryRow(ctx, `
		UPDATE workflow_instances
		SET workflow_status = 'PENDING', current_stage = 1, review_cycle = review_cycle + 1,
		    completed_at = NULL, next_action_at = $2, escalation_target = NULL,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_instance_id = $1 AND row_version = $3
		RETURNING `+instanceColumns, workflowInstanceID, due, cur.RowVersion))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConcurrencyConflict
		}
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	corr := corrOf(cur, c.Meta)
	cycle := updated.ReviewCycle
	if err := insertTransitionRecord(ctx, tx, transitionRecord{
		InstanceID: workflowInstanceID, From: cur.WorkflowStatus, To: domain.WorkflowStatusPending, ActedBy: actorPrincipalID,
		Rationale: &rationale, CorrelationID: corr, CausationID: causationID, ReviewCycle: &cycle,
		IdempotencyKey: c.Meta.IdempotencyKey, RequestHash: c.Meta.RequestHash, Authority: c.Authority, DecisionID: c.Meta.DecisionID,
	}); err != nil {
		if errors.Is(err, errIdempotentRetry) {
			return nil
		}
		return err
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "workflow_instance", AggregateID: workflowInstanceID,
		EventType: "workflow.reopened", TenantID: updated.TenantID, LegalEntityID: updated.LegalEntityID, ActorID: &actorPrincipalID,
		CorrelationID: corr, Headers: eventHeaders(updated, causationID, c.Authority, c.Meta),
		Payload: map[string]any{"workflow_instance_id": workflowInstanceID, "from_status": cur.WorkflowStatus,
			"review_cycle": cycle, "rationale": rationale}}); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := writeLedgerTx(ctx, tx, updated, actorPrincipalID, "WORKFLOW_REQUEST_CHANGES", "REOPENED", causationID, c.Authority, c.Meta,
		map[string]any{"from_status": cur.WorkflowStatus, "review_cycle": cycle, "rationale": rationale}); err != nil {
		if errors.Is(err, errIdempotentRetry) {
			return nil
		}
		return err
	}
	return tx.Commit(ctx)
}

// GetApprovalEvidence returns the transition history with its lineage, under
// the caller's tenant (it read the bare pool, which RLS empties for the
// runtime role).
func (s *PgStore) GetApprovalEvidence(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowTransition, error) {
	if _, err := s.FindWorkflowByID(ctx, workflowInstanceID); err != nil {
		return nil, err
	}
	const query = `
		SELECT workflow_transition_id, workflow_instance_id, from_state, to_state, acted_by, rationale, correlation_id,
		       causation_id, acted_at, stage_order, review_cycle, access_decision_id, authority_basis, delegation_id,
		       workflow_definition_version, decision_id::text
		FROM workflow_transitions WHERE workflow_instance_id = $1 ORDER BY acted_at, workflow_transition_id;`
	var transitions []*domain.WorkflowTransition
	err := s.withRLS(ctx, svcmiddleware.TenantFromContext(ctx), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, workflowInstanceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t := &domain.WorkflowTransition{}
			if err := rows.Scan(&t.WorkflowTransitionID, &t.WorkflowInstanceID, &t.FromState, &t.ToState, &t.ActedBy, &t.Rationale,
				&t.CorrelationID, &t.CausationID, &t.ActedAt, &t.StageOrder, &t.ReviewCycle, &t.AccessDecisionID, &t.AuthorityBasis,
				&t.DelegationID, &t.WorkflowDefinitionVersion, &t.DecisionID); err != nil {
				return err
			}
			transitions = append(transitions, t)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg GetApprovalEvidence failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return transitions, nil
}

// ── definitions ─────────────────────────────────────────────────────────────

// ListWorkflowDefinitions lists the tenant's definitions under RLS (it read
// the bare pool, which FORCE RLS empties for the runtime role).
func (s *PgStore) ListWorkflowDefinitions(ctx context.Context, params domain.ListWorkflowDefinitionsParams) ([]*domain.WorkflowDefinition, error) {
	query := `SELECT ` + definitionColumns + ` FROM workflow_definitions WHERE tenant_id = $1`
	args := []any{params.TenantID}
	if params.WorkflowType != nil {
		args = append(args, *params.WorkflowType)
		query += fmt.Sprintf(" AND workflow_type = $%d", len(args))
	}
	if params.IsActive != nil {
		args = append(args, *params.IsActive)
		query += fmt.Sprintf(" AND is_active = $%d", len(args))
	}
	query += ` ORDER BY workflow_type, version DESC`
	if params.Limit > 0 {
		args = append(args, params.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	if params.Offset > 0 {
		args = append(args, params.Offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}
	var definitions []*domain.WorkflowDefinition
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			def, scanErr := scanDefinition(rows)
			if scanErr != nil {
				return scanErr
			}
			definitions = append(definitions, def)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListWorkflowDefinitions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return definitions, nil
}

// SupersedeWorkflowDefinition marks oldDefID as superseded by newDefID and
// activates the new definition, atomically, under the caller's tenant. Both
// must exist in that tenant with the same workflow type; oldDefID must be
// active. It used to run with no app.tenant_id, so under FORCE RLS it found
// neither definition.
func (s *PgStore) SupersedeWorkflowDefinition(ctx context.Context, oldDefID, newDefID string) error {
	if oldDefID == newDefID {
		return domain.ErrInvalidTransition
	}
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenant := svcmiddleware.TenantFromContext(ctx)

	var replacementActive bool
	err = tx.QueryRow(ctx, `
		SELECT n.is_active
		FROM workflow_definitions n
		JOIN workflow_definitions o ON o.workflow_definition_id = $1
		WHERE n.workflow_definition_id = $2
		  AND n.tenant_id = o.tenant_id AND o.tenant_id = NULLIF($3, '')::uuid
		  AND n.workflow_type = o.workflow_type;`, oldDefID, newDefID, tenant).Scan(&replacementActive)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrWorkflowNotFound
		}
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE workflow_definitions
		SET is_active = false, superseded_by = $2, updated_at = NOW(), row_version = row_version + 1
		WHERE workflow_definition_id = $1 AND tenant_id = NULLIF($3, '')::uuid AND is_active = true;`, oldDefID, newDefID, tenant)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrInvalidTransition
	}
	if !replacementActive {
		if _, err := tx.Exec(ctx, `
			UPDATE workflow_definitions SET is_active = true, updated_at = NOW(), row_version = row_version + 1
			WHERE workflow_definition_id = $1 AND tenant_id = NULLIF($2, '')::uuid AND is_active = false;`, newDefID, tenant); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}
	return tx.Commit(ctx)
}

// setDefinitionActive flips is_active under the caller's tenant. A no-op
// (already in that state) is not an error; an id the tenant does not hold is
// ErrWorkflowNotFound.
func (s *PgStore) setDefinitionActive(ctx context.Context, workflowDefinitionID string, active bool) error {
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenant := svcmiddleware.TenantFromContext(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_definitions WHERE workflow_definition_id = $1 AND tenant_id = NULLIF($2, '')::uuid)`,
		workflowDefinitionID, tenant).Scan(&exists); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if !exists {
		return domain.ErrWorkflowNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workflow_definitions SET is_active = $3, updated_at = NOW(), row_version = row_version + 1
		WHERE workflow_definition_id = $1 AND tenant_id = NULLIF($2, '')::uuid AND is_active <> $3;`, workflowDefinitionID, tenant, active); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return tx.Commit(ctx)
}

// ActivateWorkflowDefinition activates a workflow definition.
func (s *PgStore) ActivateWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error {
	return s.setDefinitionActive(ctx, workflowDefinitionID, true)
}

// SuspendWorkflowDefinition suspends a workflow definition.
func (s *PgStore) SuspendWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error {
	return s.setDefinitionActive(ctx, workflowDefinitionID, false)
}

// RetireWorkflowDefinition retires a workflow definition.
func (s *PgStore) RetireWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error {
	return s.setDefinitionActive(ctx, workflowDefinitionID, false)
}

// DefinitionStages decodes a definition's stages_json into stage inputs, in
// order. A definition routes by role (ROLE stages) unless a stage names a
// principal or a quorum pool.
func DefinitionStages(def *domain.WorkflowDefinition) ([]domain.CreateWorkflowStageInput, error) {
	var raw []domain.DefinitionStage
	if err := json.Unmarshal(def.StagesJSON, &raw); err != nil {
		return nil, fmt.Errorf("stages_json is not a stage list: %w", err)
	}
	if len(raw) == 0 {
		return nil, domain.ErrNoStages
	}
	out := make([]domain.CreateWorkflowStageInput, 0, len(raw))
	for i, st := range raw {
		in := domain.CreateWorkflowStageInput{StageType: st.StageType, ApproverPrincipalID: st.ApproverPrincipalID,
			RequiredApprovals: st.RequiredApprovals, QuorumApprovers: st.QuorumApprovers, ApproverRole: st.ApproverRole,
			DueAtHours: st.DueAtHours}
		if in.StageType == "" {
			switch {
			case st.ApproverRole != "":
				in.StageType = domain.StageTypeRole
			case len(st.QuorumApprovers) > 0:
				in.StageType = domain.StageTypeQuorum
			default:
				in.StageType = domain.StageTypeSingle
			}
		}
		if in.StageType == domain.StageTypeRole && in.ApproverRole == "" {
			return nil, fmt.Errorf("stages_json[%d]: a ROLE stage needs approver_role", i)
		}
		out = append(out, in)
	}
	return out, nil
}
