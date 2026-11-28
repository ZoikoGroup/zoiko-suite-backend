-- 000007_exception_case_assignee.down.sql
-- Exception & Escalation Service — remove the named assignee (rollback)

BEGIN;

DROP INDEX IF EXISTS idx_exception_cases_assigned_to_user;
ALTER TABLE exception_cases
    DROP COLUMN IF EXISTS assigned_to_user,
    DROP COLUMN IF EXISTS assigned_to_role;

COMMIT;
