package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// Timers (Governance audit workflow rows 26-28, 21 Oct 2026). due_at,
// next_action_at and escalation_target were stored and never read: a workflow
// past its deadline stayed PENDING for ever. The sweep below lists what is due
// across tenants (SELECT only, under the app.timer_sweep policy of 000020);
// every transition it then makes runs under the instance's own tenant through
// the ordinary lifecycle methods.

// ListDueInstances returns up to limit open instances that need a timer
// action at now: past due_at (expire), a PENDING stage past next_action_at
// (escalate), or flagged for an approver re-check.
func (s *PgStore) ListDueInstances(ctx context.Context, now time.Time, limit int) ([]domain.DueInstance, error) {
	if limit <= 0 {
		limit = 100
	}
	const query = `
		SELECT tenant_id::text, workflow_instance_id::text, workflow_status, due_at, next_action_at,
		       approver_recheck_at, current_stage, row_version, escalation_target, legal_entity_id::text,
		       initiated_by, workflow_type
		  FROM workflow_instances
		 WHERE workflow_status IN ('PENDING', 'ESCALATED')
		   AND (due_at <= $1
		        OR (workflow_status = 'PENDING' AND next_action_at <= $1)
		        OR approver_recheck_at IS NOT NULL)
		 ORDER BY LEAST(COALESCE(due_at, 'infinity'), COALESCE(next_action_at, 'infinity'), COALESCE(approver_recheck_at, 'infinity'))
		 LIMIT $2`
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.timer_sweep', 'true', true)"); err != nil {
		return nil, fmt.Errorf("%w: set_config app.timer_sweep: %v", domain.ErrStoreUnavailable, err)
	}
	rows, err := tx.Query(ctx, query, now, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	var out []domain.DueInstance
	for rows.Next() {
		var d domain.DueInstance
		if err := rows.Scan(&d.TenantID, &d.WorkflowInstanceID, &d.WorkflowStatus, &d.DueAt, &d.NextActionAt,
			&d.RecheckAt, &d.CurrentStage, &d.RowVersion, &d.EscalationTarget, &d.LegalEntityID,
			&d.InitiatedBy, &d.WorkflowType); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, tx.Commit(ctx)
}

// ListCurrentApprovers returns who can still decide the instance's current
// stage: its named approver, its ROLE, or each quorum approver who has not
// voted in this review cycle. Runs under the caller's tenant.
func (s *PgStore) ListCurrentApprovers(ctx context.Context, workflowInstanceID string) ([]domain.ApproverCheck, error) {
	const query = `
		SELECT st.workflow_instance_id::text, st.stage_order, st.stage_type,
		       COALESCE(st.approver_principal_id, ''), COALESCE(st.approver_role, '')
		  FROM workflow_stages st
		  JOIN workflow_instances wi ON wi.workflow_instance_id = st.workflow_instance_id
		 WHERE st.workflow_instance_id = $1
		   AND st.stage_order = wi.current_stage
		   AND st.stage_type <> 'QUORUM'
		UNION ALL
		SELECT st.workflow_instance_id::text, st.stage_order, st.stage_type, qa.approver_principal_id, ''
		  FROM workflow_stages st
		  JOIN workflow_instances wi ON wi.workflow_instance_id = st.workflow_instance_id
		  JOIN workflow_stage_quorum_approvers qa ON qa.workflow_stage_id = st.workflow_stage_id
		 WHERE st.workflow_instance_id = $1
		   AND st.stage_order = wi.current_stage
		   AND st.stage_type = 'QUORUM'
		   AND NOT EXISTS (SELECT 1 FROM workflow_stage_quorum_decisions qd
		                    WHERE qd.workflow_stage_id = st.workflow_stage_id
		                      AND qd.approver_principal_id = qa.approver_principal_id
		                      AND qd.review_cycle = wi.review_cycle)
		 ORDER BY 2, 4`
	var out []domain.ApproverCheck
	err := s.withRLS(ctx, svcmiddleware.TenantFromContext(ctx), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, workflowInstanceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.ApproverCheck
			if err := rows.Scan(&c.WorkflowInstanceID, &c.StageOrder, &c.StageType, &c.ApproverPrincipalID, &c.ApproverRole); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// MarkApproverRecheck flags the tenant's PENDING instances for an approver
// re-check: those where principalID can still decide the current stage, or
// every one when principalID is "" (a policy changed). Returns how many were
// flagged. An ESCALATED instance is already off its approvers' desks, and
// re-checking it would loop: the re-check's own denial emits
// authorization.denied, which would flag it again.
func (s *PgStore) MarkApproverRecheck(ctx context.Context, tenantID, principalID string) (int, error) {
	const query = `
		UPDATE workflow_instances wi
		   SET approver_recheck_at = NOW()
		 WHERE wi.workflow_status = 'PENDING'
		   AND wi.approver_recheck_at IS NULL
		   AND ($1 = '' OR EXISTS (
		        SELECT 1 FROM workflow_stages st
		         WHERE st.workflow_instance_id = wi.workflow_instance_id
		           AND st.stage_order = wi.current_stage
		           AND (st.approver_principal_id = $1
		                OR EXISTS (SELECT 1 FROM workflow_stage_quorum_approvers qa
		                            WHERE qa.workflow_stage_id = st.workflow_stage_id
		                              AND qa.approver_principal_id = $1))))`
	var n int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, principalID)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return int(n), nil
}

// ClearApproverRecheck clears the flag set at or before checkedFrom, so a
// flag raised while the check ran survives to the next sweep.
func (s *PgStore) ClearApproverRecheck(ctx context.Context, workflowInstanceID string, checkedFrom time.Time) error {
	err := s.withRLS(ctx, svcmiddleware.TenantFromContext(ctx), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workflow_instances SET approver_recheck_at = NULL
			WHERE workflow_instance_id = $1 AND approver_recheck_at <= $2`, workflowInstanceID, checkedFrom)
		return err
	})
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// RecordConsumedEvent claims eventID for processing and reports whether this
// is its first delivery. At-least-once delivery then applies it once.
func (s *PgStore) RecordConsumedEvent(ctx context.Context, eventID, eventType, tenantID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO workflow_consumed_events (event_id, event_type, tenant_id)
		VALUES ($1, $2, NULLIF($3, '')::uuid) ON CONFLICT (event_id) DO NOTHING`, eventID, eventType, tenantID)
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetConsumedEvent releases a claim whose processing failed, so the
// redelivery is applied.
func (s *PgStore) ForgetConsumedEvent(ctx context.Context, eventID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM workflow_consumed_events WHERE event_id = $1`, eventID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// ListTenantsWithPending names every tenant with a PENDING instance, read
// across tenants under the same SELECT-only app.timer_sweep policy the timer
// pass uses. A platform-wide event (a global policy change) re-checks each.
func (s *PgStore) ListTenantsWithPending(ctx context.Context) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.timer_sweep', 'true', true)"); err != nil {
		return nil, fmt.Errorf("%w: set_config app.timer_sweep: %v", domain.ErrStoreUnavailable, err)
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT tenant_id::text FROM workflow_instances WHERE workflow_status = 'PENDING' ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, tx.Commit(ctx)
}
