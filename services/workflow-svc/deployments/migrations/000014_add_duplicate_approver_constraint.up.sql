-- 000014_add_duplicate_approver_constraint.up.sql
-- Add unique constraint to prevent duplicate approvers in same workflow

-- Unique index to prevent same principal from appearing multiple times in one workflow
CREATE UNIQUE INDEX idx_workflow_stages_unique_approver 
    ON workflow_stages (workflow_instance_id, approver_principal_id);