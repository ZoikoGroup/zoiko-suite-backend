-- 000015_add_deadline_fields.up.sql
-- Add deadline tracking fields for SLA/escalation per GOV-06

ALTER TABLE workflow_instances
    ADD COLUMN due_at TIMESTAMPTZ,
    ADD COLUMN next_action_at TIMESTAMPTZ;

CREATE INDEX idx_workflow_instances_next_action_at ON workflow_instances (next_action_at) WHERE next_action_at IS NOT NULL;