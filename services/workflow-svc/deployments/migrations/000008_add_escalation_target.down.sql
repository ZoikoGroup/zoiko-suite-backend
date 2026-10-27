-- 000008_add_escalation_target.down.sql
-- Rollback escalation_target

ALTER TABLE workflow_instances
    DROP COLUMN IF EXISTS escalation_target;
