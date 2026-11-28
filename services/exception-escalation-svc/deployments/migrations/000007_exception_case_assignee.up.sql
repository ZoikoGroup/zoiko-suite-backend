-- 000007_exception_case_assignee.up.sql
-- Restore the named assignee on an exception case. Assigned Actions reads this
-- column; Doc 04 §13.1 recreated the table in 000006 without it.

BEGIN;

ALTER TABLE exception_cases
    ADD COLUMN IF NOT EXISTS assigned_to_role TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS assigned_to_user TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_exception_cases_assigned_to_user
    ON exception_cases (tenant_id, assigned_to_user)
    WHERE assigned_to_user <> '';

COMMIT;
