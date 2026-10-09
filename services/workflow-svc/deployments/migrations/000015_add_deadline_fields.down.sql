-- 000015_add_deadline_fields.down.sql
-- Rollback deadline tracking fields

ALTER TABLE workflow_instances
    DROP COLUMN IF EXISTS due_at,
    DROP COLUMN IF EXISTS next_action_at;