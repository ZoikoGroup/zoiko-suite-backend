DROP INDEX IF EXISTS idx_control_exceptions_recurrence;
DROP INDEX IF EXISTS idx_exception_transitions_root_cause;
ALTER TABLE exception_transitions
    DROP COLUMN IF EXISTS root_cause_note,
    DROP COLUMN IF EXISTS root_cause_code;
