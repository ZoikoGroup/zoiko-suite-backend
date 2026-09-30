-- 000014_add_duplicate_approver_constraint.down.sql
-- Rollback duplicate approver constraint

DROP INDEX IF EXISTS idx_workflow_stages_unique_approver;