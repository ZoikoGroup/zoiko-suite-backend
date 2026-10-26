-- 000020_completion.down.sql
BEGIN;

DROP TABLE IF EXISTS workflow_consumed_events;

DROP INDEX IF EXISTS idx_outbox_events_destination_due;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS next_attempt_at;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS destination;

DROP POLICY IF EXISTS workflow_timer_sweep_select ON workflow_instances;
DROP INDEX IF EXISTS idx_workflow_instances_next_action;
DROP INDEX IF EXISTS idx_workflow_instances_due;
ALTER TABLE workflow_definitions DROP COLUMN IF EXISTS escalation_target;
ALTER TABLE workflow_definitions DROP COLUMN IF EXISTS expiry_hours;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS approver_recheck_at;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS escalated_at;

ALTER TABLE workflow_stages DROP CONSTRAINT IF EXISTS workflow_stages_shape_matches_type;
ALTER TABLE workflow_stages ADD CONSTRAINT workflow_stages_shape_matches_type CHECK (
    (stage_type = 'SINGLE' AND approver_principal_id IS NOT NULL AND required_approvals IS NULL)
    OR (stage_type = 'QUORUM' AND approver_principal_id IS NULL AND required_approvals IS NOT NULL AND required_approvals > 0)
);
ALTER TABLE workflow_stages DROP CONSTRAINT IF EXISTS workflow_stages_stage_type_check;
ALTER TABLE workflow_stages ADD CONSTRAINT workflow_stages_stage_type_check CHECK (stage_type IN ('SINGLE', 'QUORUM'));
ALTER TABLE workflow_stages DROP COLUMN IF EXISTS acted_by;
ALTER TABLE workflow_stages DROP COLUMN IF EXISTS due_hours;
ALTER TABLE workflow_stages DROP COLUMN IF EXISTS due_at;
ALTER TABLE workflow_stages DROP COLUMN IF EXISTS approver_role;

DROP TRIGGER IF EXISTS trg_quorum_decisions_append_only ON workflow_stage_quorum_decisions;
ALTER TABLE workflow_stage_quorum_decisions DROP CONSTRAINT IF EXISTS workflow_stage_quorum_decisions_one_per_approver;
ALTER TABLE workflow_stage_quorum_decisions ADD CONSTRAINT workflow_stage_quorum_decisions_one_per_approver
    UNIQUE (workflow_stage_id, approver_principal_id);
ALTER TABLE workflow_stage_quorum_decisions DROP COLUMN IF EXISTS review_cycle;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS review_cycle;

DROP TRIGGER IF EXISTS trg_workflow_transitions_no_truncate ON workflow_transitions;
DROP TRIGGER IF EXISTS trg_workflow_transitions_append_only ON workflow_transitions;
DROP FUNCTION IF EXISTS workflow_refuse_mutation();
DROP INDEX IF EXISTS uq_workflow_transitions_idempotency;
ALTER TABLE workflow_transitions
    DROP COLUMN IF EXISTS decision_id,
    DROP COLUMN IF EXISTS workflow_definition_version,
    DROP COLUMN IF EXISTS delegation_id,
    DROP COLUMN IF EXISTS authority_basis,
    DROP COLUMN IF EXISTS access_decision_id,
    DROP COLUMN IF EXISTS review_cycle,
    DROP COLUMN IF EXISTS stage_order,
    DROP COLUMN IF EXISTS request_hash,
    DROP COLUMN IF EXISTS idempotency_key;

CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_instances_correlation_unique
    ON workflow_instances (tenant_id, correlation_id) WHERE correlation_id != '';

COMMIT;
