DROP INDEX IF EXISTS idx_control_exceptions_sla;
ALTER TABLE control_exceptions DROP COLUMN IF EXISTS sla_breach_notified_at;
ALTER TABLE exception_transitions
    DROP COLUMN IF EXISTS carry_to_period,
    DROP COLUMN IF EXISTS evidence_ref,
    DROP COLUMN IF EXISTS authority_ref;
