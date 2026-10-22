-- Migration: 000009_late_data_checks.up.sql
--
-- SPEC_DEVIATIONS A7. ZS-CONTROL-001 Invariant 14: "Late-arriving data after
-- population freeze creates an explicit late-data exception or a
-- new/superseding run"; scenario 05: "certified population is not silently
-- mutated"; anti-pattern: "Ignoring late-arriving data after certification".
--
-- The late-data sweep re-reads, for the latest CERTIFIED run of each control,
-- entity and period, each side's source watermark and compares it with the
-- one frozen in population_snapshots. A moved watermark opens a LATE_DATA
-- exception (reason LATE_DATA_AFTER_CERTIFICATION) in control_exceptions,
-- the table every exception already lives in. The close gate reads it.
--
-- control_watermark_checks is the sweep's own state, one row per run and
-- side, overwritten by each check. It holds no history and no finding: the
-- exception, its transitions and its event are the record. It holds:
--   * when the side was last checked and what was seen (CURRENT, MOVED, or
--     UNVERIFIED with the error, when the source could not answer), which
--     orders the sweep (least recently checked first) and lets an operator
--     see a source that keeps failing;
--   * reported_watermark / late_exception_id: the watermark the latest
--     late-data exception was opened for. Once that exception is resolved
--     under authority, the same watermark is not reported again; a further
--     move is.
-- Nothing here references or changes the frozen population beyond a foreign
-- key to it.

CREATE TABLE control_watermark_checks (
    tenant_id          UUID         NOT NULL,
    run_id             UUID         NOT NULL REFERENCES control_runs(run_id),
    side               CHAR(1)      NOT NULL CHECK (side IN ('A','B')),
    population_id      UUID         NOT NULL REFERENCES population_snapshots(population_id),
    checked_at         TIMESTAMPTZ  NOT NULL,
    outcome            VARCHAR(16)  NOT NULL CHECK (outcome IN ('CURRENT','MOVED','UNVERIFIED')),
    observed_watermark VARCHAR(255),
    error              TEXT         NOT NULL DEFAULT '',
    reported_watermark VARCHAR(255),
    late_exception_id  UUID         REFERENCES control_exceptions(exception_id),
    PRIMARY KEY (run_id, side),
    CHECK ((outcome = 'UNVERIFIED') = (observed_watermark IS NULL)),
    CHECK ((reported_watermark IS NULL) = (late_exception_id IS NULL))
);
CREATE INDEX idx_control_watermark_checks_tenant ON control_watermark_checks (tenant_id, run_id);

ALTER TABLE control_watermark_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE control_watermark_checks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON control_watermark_checks FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
