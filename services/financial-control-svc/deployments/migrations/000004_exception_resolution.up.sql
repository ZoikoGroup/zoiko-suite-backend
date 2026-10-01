-- Wave 7: exception resolution evidence and SLA-breach signalling.
--
-- exception_transitions stays append-only (its immutability trigger is untouched); the new
-- columns simply record WHY a governed transition was allowed: the authority that approved a
-- waiver or carry-forward, the evidence behind a remediation or reperformance, and the period
-- a carried-forward item moves to.
ALTER TABLE exception_transitions
    ADD COLUMN authority_ref   TEXT NOT NULL DEFAULT '',
    ADD COLUMN evidence_ref    TEXT NOT NULL DEFAULT '',
    ADD COLUMN carry_to_period VARCHAR(32) NOT NULL DEFAULT '';

-- Set once, when the SLA-breach event is emitted, so the sweeper never announces a breach twice.
-- guard_control_exception() only freezes the finding columns, so this stays updatable.
ALTER TABLE control_exceptions ADD COLUMN sla_breach_notified_at TIMESTAMPTZ;
CREATE INDEX idx_control_exceptions_sla ON control_exceptions (tenant_id, due_at)
    WHERE sla_breach_notified_at IS NULL
      AND state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY');
