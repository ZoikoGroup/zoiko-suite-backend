// Approval decisions (Group 2 completion pass, 21 Oct 2026).
//
// Every decision — a named approver, a delegate, an escalation target, a
// ROLE-stage approver or a quorum vote — runs in one transaction that:
//   - locks the instance row (decisions on one instance are serialised; the
//     quorum path also locks the stage so concurrent final votes see each
//     other — under READ COMMITTED two concurrent votes used to each tally
//     without the other and leave a satisfied stage PENDING);
//   - recognises a retried command by its Idempotency-Key (same key, same
//     body → replay; different body → ErrIdempotencyMismatch);
//   - records the transition with its true from-state, the stage, review
//     cycle, route version, authority snapshot and ledger decision id;
//   - starts the next stage's deadline;
//   - writes the domain events and the governance-ledger record to the
//     outbox, so a decision and its evidence commit together.
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
	"zoiko.io/workflow-svc/internal/outbox"
)

// beginTenantTx opens a transaction with app.tenant_id set from ctx.
func (s *PgStore) beginTenantTx(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("%w: set_config app.tenant_id: %v", domain.ErrStoreUnavailable, err)
	}
	return tx, nil
}

// lockInstance re-reads the instance under the caller's tenant with a row lock.
func lockInstance(ctx context.Context, tx pgx.Tx, instanceID string) (*domain.WorkflowInstance, error) {
	tenant := svcmiddleware.TenantFromContext(ctx)
	inst, err := scanInstance(tx.QueryRow(ctx,
		`SELECT `+instanceColumns+` FROM workflow_instances WHERE workflow_instance_id = $1 AND tenant_id = NULLIF($2, '')::uuid FOR UPDATE`,
		instanceID, tenant))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWorkflowNotFound
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return inst, nil
}

func stageInTx(ctx context.Context, tx pgx.Tx, where string, args ...any) (*domain.WorkflowStage, error) {
	st, err := scanStage(tx.QueryRow(ctx, `SELECT `+stageColumns+` FROM workflow_stages WHERE `+where, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWrongApprover
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return st, nil
}

// SubmitAction applies an APPROVE/REJECT to the current stage.
//
// Who may decide which stage:
//   - the stage's named approver (SINGLE);
//   - a delegate of that approver (AssignedApproverID; the handler verified
//     the delegation with authorization-svc);
//   - the instance's escalation target, on an ESCALATED instance's current
//     stage (ActingAs "escalation") — escalation used to be a dead end;
//   - on a ROLE stage, anyone the handler's authorization check admitted,
//     except a principal who already decided another stage (ActingAs "role").
//
// Idempotency: the same command (Idempotency-Key and body) replays; a stage
// already in the outcome this action would produce is a no-op; a stage in the
// other outcome is ErrInvalidTransition.
func (s *PgStore) SubmitAction(ctx context.Context, params domain.SubmitActionParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error) {
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := lockInstance(ctx, tx, params.WorkflowInstanceID)
	if err != nil {
		return nil, nil, false, err
	}
	if replay, err := replayedCommand(ctx, tx, cur.WorkflowInstanceID, params.Meta); err != nil || replay {
		if err != nil {
			return nil, nil, false, err
		}
		st, _ := stageInTx(ctx, tx, `workflow_instance_id = $1 AND stage_order = $2`, cur.WorkflowInstanceID, maxInt(cur.CurrentStage, 1))
		return cur, st, false, nil
	}

	wantStatus := "APPROVED"
	if params.Action == "REJECT" {
		wantStatus = "REJECTED"
	}

	var st *domain.WorkflowStage
	switch {
	case params.ActingAs == "escalation" || params.ActingAs == "role":
		if cur.CurrentStage == 0 {
			return nil, nil, false, domain.ErrInvalidTransition
		}
		st, err = stageInTx(ctx, tx, `workflow_instance_id = $1 AND stage_order = $2`, cur.WorkflowInstanceID, cur.CurrentStage)
		if err != nil {
			return nil, nil, false, err
		}
		if params.ActingAs == "escalation" {
			if cur.WorkflowStatus != domain.WorkflowStatusEscalated || cur.EscalationTarget == nil || *cur.EscalationTarget != params.ActorPrincipalID {
				return nil, nil, false, domain.ErrNotEscalationOwner
			}
		} else if st.StageType != domain.StageTypeRole {
			return nil, nil, false, domain.ErrWrongApprover
		}
	default:
		approverID := params.ActorPrincipalID
		if params.AssignedApproverID != "" {
			approverID = params.AssignedApproverID
		}
		// The approver's stage, not just "the current stage": a retried
		// stage-1 approval arriving after stage 2 became current must still be
		// recognised as the replay it is.
		st, err = stageInTx(ctx, tx, `workflow_instance_id = $1 AND approver_principal_id = $2 ORDER BY stage_order LIMIT 1`,
			cur.WorkflowInstanceID, approverID)
		if err != nil {
			return nil, nil, false, err
		}
	}

	if cur.WorkflowStatus != domain.WorkflowStatusPending && cur.WorkflowStatus != domain.WorkflowStatusEscalated {
		return nil, nil, false, domain.ErrInvalidTransition
	}
	if st.StageStatus == wantStatus {
		return cur, st, false, nil
	}
	if st.StageStatus != "PENDING" {
		return nil, nil, false, domain.ErrInvalidTransition
	}
	if st.StageOrder != cur.CurrentStage {
		return nil, nil, false, domain.ErrWrongApprover
	}
	// R-INV-07 independence across stages: nobody decides two stages of one
	// instance in the same review cycle (named stages are kept distinct at
	// create; a ROLE stage or an escalation could otherwise collect them).
	var alreadyDecided bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_stages WHERE workflow_instance_id = $1 AND stage_order <> $2 AND acted_by = $3 AND stage_status IN ('APPROVED','REJECTED'))`,
		cur.WorkflowInstanceID, st.StageOrder, params.ActorPrincipalID).Scan(&alreadyDecided); err != nil {
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if alreadyDecided {
		return nil, nil, false, domain.ErrAlreadyDecided
	}

	updatedStage, err := scanStage(tx.QueryRow(ctx, `
		UPDATE workflow_stages
		SET stage_status = $1, acted_at = NOW(), rationale = $2, acted_by = $3,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_stage_id = $4 AND row_version = $5
		RETURNING `+stageColumns, wantStatus, params.Rationale, params.ActorPrincipalID, st.WorkflowStageID, st.RowVersion))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, domain.ErrConcurrencyConflict
		}
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	actedAs := "named_approver"
	switch {
	case params.AssignedApproverID != "":
		actedAs = "delegate_of:" + params.AssignedApproverID
	case params.ActingAs == "escalation":
		actedAs = "escalation_target"
	case params.ActingAs == "role":
		actedAs = "role_holder"
	}
	updated, err := s.advanceTx(ctx, tx, decisionOutcome{
		before: cur, stage: updatedStage, decision: wantStatus, actor: params.ActorPrincipalID,
		rationale: params.Rationale, causationID: params.CausationID, authority: params.Authority,
		meta: params.Meta, actedAs: actedAs,
		synthetic: fmt.Sprintf("stage %d %s by %s (%s)", st.StageOrder, wantStatus, params.ActorPrincipalID, actedAs),
	})
	if err != nil {
		if errors.Is(err, errIdempotentRetry) {
			_ = tx.Rollback(ctx)
			inst, findErr := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
			return inst, st, false, findErr
		}
		return nil, nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg SubmitAction: commit failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, updatedStage, true, nil
}

// SubmitQuorumVote casts one eligible approver's vote on the current QUORUM
// stage (ZS-SVC-R-001 §5.1/§8.3). The stage row is locked first, so votes on
// one stage are serialised and each tally sees every earlier vote.
func (s *PgStore) SubmitQuorumVote(ctx context.Context, params domain.SubmitQuorumVoteParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error) {
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := lockInstance(ctx, tx, params.WorkflowInstanceID)
	if err != nil {
		return nil, nil, false, err
	}
	if replay, err := replayedCommand(ctx, tx, cur.WorkflowInstanceID, params.Meta); err != nil || replay {
		if err != nil {
			return nil, nil, false, err
		}
		st, _ := stageInTx(ctx, tx, `workflow_instance_id = $1 AND stage_order = $2`, cur.WorkflowInstanceID, maxInt(cur.CurrentStage, 1))
		return cur, st, false, nil
	}
	// ESCALATED is still decidable (it used to refuse every vote).
	if cur.WorkflowStatus != domain.WorkflowStatusPending && cur.WorkflowStatus != domain.WorkflowStatusEscalated {
		return nil, nil, false, domain.ErrInvalidTransition
	}
	st, err := scanStage(tx.QueryRow(ctx, `SELECT `+stageColumns+` FROM workflow_stages WHERE workflow_instance_id = $1 AND stage_order = $2 FOR UPDATE`,
		cur.WorkflowInstanceID, cur.CurrentStage))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, domain.ErrInvalidTransition
		}
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if st.StageType != domain.StageTypeQuorum {
		return nil, nil, false, domain.ErrNotQuorumStage
	}
	if st.StageStatus != "PENDING" {
		return nil, nil, false, domain.ErrInvalidTransition
	}
	var eligible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_stage_quorum_approvers WHERE workflow_stage_id = $1 AND approver_principal_id = $2)`,
		st.WorkflowStageID, params.ActorPrincipalID).Scan(&eligible); err != nil {
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if !eligible {
		return nil, nil, false, domain.ErrNotEligibleQuorumApprover
	}

	wantDecision := domain.QuorumDecisionApprove
	if params.Action == "REJECT" {
		wantDecision = domain.QuorumDecisionReject
	}
	var inserted string
	insertErr := tx.QueryRow(ctx, `
		INSERT INTO workflow_stage_quorum_decisions (workflow_stage_id, approver_principal_id, decision, rationale, review_cycle)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workflow_stage_id, approver_principal_id, review_cycle) DO NOTHING
		RETURNING decision`, st.WorkflowStageID, params.ActorPrincipalID, wantDecision, params.Rationale, cur.ReviewCycle).Scan(&inserted)
	if insertErr != nil {
		if !errors.Is(insertErr, pgx.ErrNoRows) {
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, insertErr)
		}
		var existing string
		if err := tx.QueryRow(ctx, `SELECT decision FROM workflow_stage_quorum_decisions WHERE workflow_stage_id = $1 AND approver_principal_id = $2 AND review_cycle = $3`,
			st.WorkflowStageID, params.ActorPrincipalID, cur.ReviewCycle).Scan(&existing); err != nil {
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if existing != wantDecision {
			return nil, nil, false, domain.ErrInvalidTransition
		}
		return cur, st, false, nil
	}

	tally, err := quorumTallyCycle(ctx, tx, st.WorkflowStageID, cur.ReviewCycle)
	if err != nil {
		return nil, nil, false, err
	}
	required := 0
	if st.RequiredApprovals != nil {
		required = *st.RequiredApprovals
	}
	qs := domain.QuorumStageStatus{PoolSize: tally.poolSize, RequiredApprovals: required,
		ApprovalCount: tally.approvals, RejectionCount: tally.rejections, VotesCast: tally.votesCast}
	outcome, resolved := qs.Resolved()

	voteRationale := fmt.Sprintf("quorum vote %s by %s: %d/%d approvals (%d of %d eligible voted)",
		wantDecision, params.ActorPrincipalID, tally.approvals, required, tally.votesCast, tally.poolSize)
	quorum := map[string]any{"approvals": tally.approvals, "rejections": tally.rejections, "required": required,
		"votes_cast": tally.votesCast, "pool_size": tally.poolSize, "vote": wantDecision}

	if !resolved {
		// The vote is a decision in its own right: it gets a transition and a
		// ledger record (quorum votes used to reach neither).
		if err := s.recordVoteTx(ctx, tx, cur, st, params, voteRationale, quorum); err != nil {
			if errors.Is(err, errIdempotentRetry) {
				_ = tx.Rollback(ctx)
				inst, findErr := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
				return inst, st, false, findErr
			}
			return nil, nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return cur, st, true, nil
	}

	updatedStage, err := scanStage(tx.QueryRow(ctx, `
		UPDATE workflow_stages
		SET stage_status = $1, acted_at = NOW(), rationale = $2, acted_by = $3,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_stage_id = $4 AND row_version = $5
		RETURNING `+stageColumns, outcome, voteRationale, params.ActorPrincipalID, st.WorkflowStageID, st.RowVersion))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, domain.ErrConcurrencyConflict
		}
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	updated, err := s.advanceTx(ctx, tx, decisionOutcome{
		before: cur, stage: updatedStage, decision: outcome, actor: params.ActorPrincipalID,
		rationale: params.Rationale, causationID: params.CausationID, authority: params.Authority,
		meta: params.Meta, actedAs: "quorum_member", synthetic: voteRationale, quorum: quorum,
	})
	if err != nil {
		if errors.Is(err, errIdempotentRetry) {
			_ = tx.Rollback(ctx)
			inst, findErr := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
			return inst, updatedStage, false, findErr
		}
		return nil, nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg SubmitQuorumVote: commit failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, updatedStage, true, nil
}

func quorumTallyCycle(ctx context.Context, tx pgx.Tx, stageID string, cycle int) (quorumTallyResult, error) {
	var tr quorumTallyResult
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_stage_quorum_approvers WHERE workflow_stage_id = $1`, stageID).Scan(&tr.poolSize); err != nil {
		return tr, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE decision = 'APPROVE'), count(*) FILTER (WHERE decision = 'REJECT')
		FROM workflow_stage_quorum_decisions WHERE workflow_stage_id = $1 AND review_cycle = $2`, stageID, cycle).
		Scan(&tr.votesCast, &tr.approvals, &tr.rejections); err != nil {
		return tr, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return tr, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// decisionOutcome is a resolved stage and everything the transition, events
// and ledger record need.
type decisionOutcome struct {
	before      *domain.WorkflowInstance
	stage       *domain.WorkflowStage
	decision    string // APPROVED | REJECTED
	actor       string
	rationale   *string
	causationID *string
	authority   domain.ActionAuthority
	meta        domain.CommandMeta
	actedAs     string
	synthetic   string
	quorum      map[string]any
}

// advanceTx moves the instance past a resolved stage: REJECTED ends it,
// APPROVED on the last stage ends it, otherwise the next stage becomes
// current and its deadline starts. Writes the transition, the events and the
// ledger record in the caller's transaction.
func (s *PgStore) advanceTx(ctx context.Context, tx pgx.Tx, o decisionOutcome) (*domain.WorkflowInstance, error) {
	isLast, err := isFinalStage(ctx, tx, o.before.WorkflowInstanceID, o.stage.StageOrder)
	if err != nil {
		return nil, err
	}
	newStatus := domain.WorkflowStatusPending
	nextStage := o.stage.StageOrder + 1
	switch {
	case o.decision == "REJECTED":
		newStatus, nextStage = domain.WorkflowStatusRejected, 0
	case isLast:
		newStatus, nextStage = domain.WorkflowStatusApproved, 0
	}

	var nextAction *time.Time
	if nextStage > 0 {
		var hours *int
		if err := tx.QueryRow(ctx, `SELECT due_hours FROM workflow_stages WHERE workflow_instance_id = $1 AND stage_order = $2`,
			o.before.WorkflowInstanceID, nextStage).Scan(&hours); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if hours != nil {
			t := time.Now().UTC().Add(time.Duration(*hours) * time.Hour)
			nextAction = &t
			if _, err := tx.Exec(ctx, `UPDATE workflow_stages SET due_at = $1, updated_at = NOW() WHERE workflow_instance_id = $2 AND stage_order = $3`,
				t, o.before.WorkflowInstanceID, nextStage); err != nil {
				return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
		}
	}

	updated, err := scanInstance(tx.QueryRow(ctx, `
		UPDATE workflow_instances
		SET workflow_status = $1, current_stage = $2,
		    completed_at = CASE WHEN $1::VARCHAR IN ('APPROVED','REJECTED') THEN NOW() ELSE completed_at END,
		    next_action_at = $5,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_instance_id = $3 AND row_version = $4
		RETURNING `+instanceColumns, newStatus, nextStage, o.before.WorkflowInstanceID, o.before.RowVersion, nextAction))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrConcurrencyConflict
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	rationale := o.synthetic
	if o.rationale != nil && *o.rationale != "" {
		rationale = *o.rationale
	}
	stageOrder, cycle := o.stage.StageOrder, o.before.ReviewCycle
	var defVersion *int
	if o.before.WorkflowDefinitionVersion > 0 {
		v := o.before.WorkflowDefinitionVersion
		defVersion = &v
	}
	corr := corrOf(o.before, o.meta)
	if err := insertTransitionRecord(ctx, tx, transitionRecord{
		InstanceID: o.before.WorkflowInstanceID, From: o.before.WorkflowStatus, To: newStatus, ActedBy: o.actor,
		Rationale: &rationale, CorrelationID: corr, CausationID: o.causationID,
		StageOrder: &stageOrder, ReviewCycle: &cycle, DefinitionVersion: defVersion,
		IdempotencyKey: o.meta.IdempotencyKey, RequestHash: o.meta.RequestHash,
		Authority: o.authority, DecisionID: o.meta.DecisionID,
	}); err != nil {
		return nil, err
	}

	headers := eventHeaders(updated, o.causationID, o.authority, o.meta)
	stageEvent := "approval.granted"
	if o.decision == "REJECTED" {
		stageEvent = "approval.rejected"
	}
	stagePayload := map[string]any{
		"workflow_instance_id": updated.WorkflowInstanceID,
		"stage_order":          o.stage.StageOrder,
		"stage_type":           o.stage.StageType,
		"decided_by":           o.actor,
		"acted_as":             o.actedAs,
		"review_cycle":         cycle,
	}
	if o.stage.ApproverPrincipalID != "" {
		stagePayload["approver_principal_id"] = o.stage.ApproverPrincipalID
	}
	if o.quorum != nil {
		stagePayload["quorum"] = o.quorum
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "workflow_instance", AggregateID: updated.WorkflowInstanceID,
		EventType: stageEvent, TenantID: updated.TenantID, LegalEntityID: updated.LegalEntityID, ActorID: &o.actor,
		CorrelationID: corr, Headers: headers, Payload: stagePayload}); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if newStatus == domain.WorkflowStatusApproved || newStatus == domain.WorkflowStatusRejected {
		if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "workflow_instance", AggregateID: updated.WorkflowInstanceID,
			EventType: "workflow.completed", TenantID: updated.TenantID, LegalEntityID: updated.LegalEntityID, ActorID: &o.actor,
			CorrelationID: corr, Headers: headers, Payload: map[string]any{
				"workflow_instance_id": updated.WorkflowInstanceID,
				"workflow_status":      updated.WorkflowStatus,
				"completed_at":         updated.CompletedAt,
			}}); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}

	actionType := "WORKFLOW_STAGE_APPROVE"
	if o.decision == "REJECTED" {
		actionType = "WORKFLOW_STAGE_REJECT"
	}
	evaluation := map[string]any{
		"stage_order": o.stage.StageOrder, "stage_type": o.stage.StageType, "review_cycle": cycle,
		"acted_as": o.actedAs, "resulting_status": newStatus, "initiated_by": o.before.InitiatedBy,
		"rationale": rationale,
	}
	if o.quorum != nil {
		evaluation["quorum"] = o.quorum
	}
	if o.stage.ApproverRole != nil {
		evaluation["approver_role"] = *o.stage.ApproverRole
	}
	if err := writeLedgerTx(ctx, tx, updated, o.actor, actionType, o.decision, o.causationID, o.authority, o.meta, evaluation); err != nil {
		return nil, err
	}
	return updated, nil
}

// recordVoteTx records a quorum vote that did not resolve its stage.
func (s *PgStore) recordVoteTx(ctx context.Context, tx pgx.Tx, cur *domain.WorkflowInstance, st *domain.WorkflowStage,
	params domain.SubmitQuorumVoteParams, synthetic string, quorum map[string]any) error {
	rationale := synthetic
	if params.Rationale != nil && *params.Rationale != "" {
		rationale = *params.Rationale
	}
	stageOrder, cycle := st.StageOrder, cur.ReviewCycle
	corr := corrOf(cur, params.Meta)
	if err := insertTransitionRecord(ctx, tx, transitionRecord{
		InstanceID: cur.WorkflowInstanceID, From: cur.WorkflowStatus, To: cur.WorkflowStatus, ActedBy: params.ActorPrincipalID,
		Rationale: &rationale, CorrelationID: corr, CausationID: params.CausationID,
		StageOrder: &stageOrder, ReviewCycle: &cycle,
		IdempotencyKey: params.Meta.IdempotencyKey, RequestHash: params.Meta.RequestHash,
		Authority: params.Authority, DecisionID: params.Meta.DecisionID,
	}); err != nil {
		return err
	}
	action := "WORKFLOW_QUORUM_VOTE_APPROVE"
	outcome := "APPROVE"
	if params.Action == "REJECT" {
		action, outcome = "WORKFLOW_QUORUM_VOTE_REJECT", "REJECT"
	}
	return writeLedgerTx(ctx, tx, cur, params.ActorPrincipalID, action, outcome, params.CausationID, params.Authority, params.Meta,
		map[string]any{"stage_order": st.StageOrder, "stage_type": st.StageType, "review_cycle": cycle, "quorum": quorum,
			"resulting_status": cur.WorkflowStatus, "rationale": rationale})
}

func corrOf(inst *domain.WorkflowInstance, meta domain.CommandMeta) *string {
	if meta.CorrelationID != "" {
		c := meta.CorrelationID
		return &c
	}
	if inst.CorrelationID != "" {
		c := inst.CorrelationID
		return &c
	}
	return nil
}

// eventHeaders carries the envelope fields the relay adds to every event
// (causation, ledger decision, authority, idempotency, cycle, route version,
// jurisdiction).
func eventHeaders(inst *domain.WorkflowInstance, causationID *string, authority domain.ActionAuthority, meta domain.CommandMeta) map[string]string {
	h := map[string]string{"review_cycle": strconv.Itoa(inst.ReviewCycle)}
	if causationID != nil && *causationID != "" {
		h["causation_id"] = *causationID
	}
	if meta.DecisionID != "" {
		h["decision_id"] = meta.DecisionID
	}
	if authority.AccessDecisionID != "" {
		h["access_decision_id"] = authority.AccessDecisionID
	}
	if meta.IdempotencyKey != "" {
		h["idempotency_key"] = meta.IdempotencyKey
	}
	if meta.Jurisdiction != "" {
		h["jurisdiction_context"] = meta.Jurisdiction
	}
	if inst.WorkflowDefinitionVersion > 0 {
		h["workflow_definition_version"] = strconv.Itoa(inst.WorkflowDefinitionVersion)
	}
	return h
}

// writeLedgerTx writes the governance-ledger record of a decision to the
// outbox (destination decision_log). Without a decision id there is no
// record to write (system-internal calls that did not allocate one).
func writeLedgerTx(ctx context.Context, tx pgx.Tx, inst *domain.WorkflowInstance, actor, actionType, outcome string,
	causationID *string, authority domain.ActionAuthority, meta domain.CommandMeta, evaluation map[string]any) error {
	if meta.DecisionID == "" {
		return nil
	}
	if evaluation == nil {
		evaluation = map[string]any{}
	}
	evaluation["workflow_type"] = inst.WorkflowType
	evaluation["actor_principal_id"] = actor
	if authority.AccessDecisionID != "" {
		evaluation["access_decision_id"] = authority.AccessDecisionID
	}
	if authority.Basis != "" {
		evaluation["authority_basis"] = authority.Basis
	}
	if authority.DelegationID != "" {
		evaluation["delegation_id"] = authority.DelegationID
	}
	if inst.WorkflowDefinitionID != "" {
		evaluation["workflow_definition_id"] = inst.WorkflowDefinitionID
		evaluation["workflow_definition_version"] = inst.WorkflowDefinitionVersion
	}
	if inst.SubjectFingerprint != nil {
		evaluation["subject_fingerprint"] = *inst.SubjectFingerprint
	}
	if inst.SubjectVersion != nil {
		evaluation["subject_version"] = *inst.SubjectVersion
	}
	ruleBasis := fmt.Sprintf("workflow:%s", inst.WorkflowType)
	if inst.WorkflowDefinitionID != "" {
		ruleBasis += fmt.Sprintf(" definition:%s@v%d", inst.WorkflowDefinitionID, inst.WorkflowDefinitionVersion)
	}
	if authority.AccessDecisionID != "" {
		ruleBasis += " access_decision:" + authority.AccessDecisionID
	}
	if authority.Basis != "" {
		ruleBasis += " basis:" + authority.Basis
	}
	rec := domain.LedgerRecord{
		DecisionID: meta.DecisionID, TenantID: inst.TenantID, LegalEntityID: inst.LegalEntityID,
		ActionType: actionType, Outcome: outcome, RuleBasis: ruleBasis,
		WorkflowInstanceID: inst.WorkflowInstanceID, JurisdictionContext: meta.Jurisdiction,
		EvaluationContext: evaluation, DecidedAt: time.Now().UTC(), ActorPrincipalID: actor,
	}
	if c := corrOf(inst, meta); c != nil {
		rec.CorrelationID = *c
	}
	if causationID != nil {
		rec.CausationID = *causationID
	}
	if inst.SubjectType != nil {
		rec.ActionSubjectType = *inst.SubjectType
	}
	if inst.SubjectID != nil {
		rec.ActionSubjectID = *inst.SubjectID
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: meta.DecisionID, AggregateType: "workflow_instance", AggregateID: inst.WorkflowInstanceID,
		EventType: "governance.decision.record", TenantID: inst.TenantID, LegalEntityID: inst.LegalEntityID,
		ActorID: &actor, CorrelationID: corrOf(inst, meta), Payload: rec, Destination: outbox.DestinationDecisionLog,
	}); err != nil {
		if isUniqueViolation(err, "outbox_events_pkey") {
			// A decision id is used once: a second record under it is a
			// replay the transition's own idempotency already caught.
			return errIdempotentRetry
		}
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}
