-- Wave 7: root cause captured on the exception's append-only history (ZS-CONTROL-001 s21:
-- "Root cause: required for significant/recurrent exceptions; standardized taxonomy plus
-- narrative"). The taxonomy itself is a controlled decision (s34) so the code is validated
-- for shape only. Recorded on the transition that resolves or excuses the exception.
ALTER TABLE exception_transitions
    ADD COLUMN root_cause_code VARCHAR(48) NOT NULL DEFAULT '',
    ADD COLUMN root_cause_note TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_exception_transitions_root_cause ON exception_transitions (tenant_id, root_cause_code)
    WHERE root_cause_code <> '';
CREATE INDEX idx_control_exceptions_recurrence ON control_exceptions (tenant_id, reason_code);
