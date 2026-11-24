-- Migration 000026: WF-S24 operator repair + WF-S25 emergency approval - DOWN

DROP TRIGGER IF EXISTS emergency_approvals_updated_at ON emergency_approvals;
DROP TABLE IF EXISTS emergency_approvals CASCADE;

DROP TRIGGER IF EXISTS operator_actions_updated_at ON operator_actions;
DROP TABLE IF EXISTS operator_actions CASCADE;

ALTER TABLE workflow_definitions 
    DROP COLUMN IF EXISTS emergency_conditions;

ALTER TABLE workflow_instances 
    DROP COLUMN IF EXISTS next_action_at;

DROP INDEX IF EXISTS idx_workflow_instances_next_action;

DROP TYPE IF EXISTS operator_action_kind;
DROP TYPE IF EXISTS operator_action_status;
DROP TYPE IF EXISTS emergency_condition;