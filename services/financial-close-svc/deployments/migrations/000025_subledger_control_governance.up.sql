-- Migration: 000025_subledger_control_governance.up.sql
--
-- ACC-06 (Subledger Control): SPEC_DEVIATIONS A1 — "RunSubledgerControl is a
-- single synchronous comparison... There is no Planned/Running/Reperformed/
-- Certified/Failed state, no exception entity, and none of the commands
-- AssignException, RecordResolution, CertifyControl or RejectControl."
-- "A break is 'resolved' by running the control again until it matches. No
-- resolution, owner or approval is recorded."
--
-- This closes that gap without touching subledger_control_runs' own
-- append-only finding (status MATCHED/EXCEPTION is never rewritten — the
-- finding at the moment it ran is permanent history): certification is a
-- separate fact layered on top of a run, and resolution of an EXCEPTION is
-- its own tracked object with a real owner and a real approval.

-- certification_status is deliberately NOT a rename/overwrite of status:
-- status is the run's own finding (did the subledger agree with the GL at
-- that instant); certification_status is "has a human signed off that this
-- period's control is reliable." A run can be MATCHED and never certified
-- (nobody has looked at it yet) or EXCEPTION and later certified (once its
-- exception is RESOLVED).
ALTER TABLE subledger_control_runs
    ADD COLUMN certification_status VARCHAR(16),
    ADD COLUMN certified_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN certified_by_principal_id VARCHAR(255),
    ADD COLUMN certification_note TEXT;

ALTER TABLE subledger_control_runs
    ADD CONSTRAINT subledger_control_runs_certification_status_check
    CHECK (certification_status IS NULL OR certification_status IN ('CERTIFIED', 'FAILED'));

-- Migration 000004 made this table append-only with a trigger that refuses
-- EVERY update. Certification is deliberately a second fact layered on the
-- run, so it must be the one exception: replace the function with a guard
-- that still forbids changing any finding field (and still forbids deletes),
-- but permits the certification columns. Without this, CertifyControlRun's
-- UPDATE is refused by the database even though every application-level
-- check passed.
CREATE OR REPLACE FUNCTION reject_control_run_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'subledger_control_runs is append-only: % is not permitted', TG_OP;
    END IF;
    IF (NEW.control_run_id, NEW.tenant_id, NEW.legal_entity_id, NEW.fiscal_period,
        NEW.subledger, NEW.control_account_code, NEW.subledger_total_amount,
        NEW.gl_control_balance_amount, NEW.difference_amount, NEW.status,
        NEW.run_at, NEW.run_by_principal_id, NEW.book_id)
       IS DISTINCT FROM
       (OLD.control_run_id, OLD.tenant_id, OLD.legal_entity_id, OLD.fiscal_period,
        OLD.subledger, OLD.control_account_code, OLD.subledger_total_amount,
        OLD.gl_control_balance_amount, OLD.difference_amount, OLD.status,
        OLD.run_at, OLD.run_by_principal_id, OLD.book_id) THEN
        RAISE EXCEPTION 'subledger_control_runs finding is append-only; only certification may change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- A control run's certifier must differ from who ran it (maker-checker,
-- spec: "Preparer cannot certify own material control"). Enforced in the
-- application layer (the row lock + check happens in one transaction with
-- the UPDATE), same pattern as every other maker-checker guard in this
-- codebase — a CHECK constraint can't compare across two different
-- principal_id columns conditionally like this without being either always
-- true or needing a trigger, and a trigger duplicates the same logic one
-- more time for no real extra safety.

CREATE TABLE control_exceptions (
    control_exception_id       UUID PRIMARY KEY,
    tenant_id                  UUID NOT NULL,
    control_run_id             UUID NOT NULL REFERENCES subledger_control_runs(control_run_id),
    legal_entity_id            UUID NOT NULL,
    fiscal_period              VARCHAR(16) NOT NULL,
    subledger                  VARCHAR(32) NOT NULL,
    book_id                    VARCHAR(255),
    status                     VARCHAR(16) NOT NULL DEFAULT 'OPEN',
    assigned_to_principal_id   VARCHAR(255),
    assigned_at                TIMESTAMP WITH TIME ZONE,
    resolution_note            TEXT,
    resolved_at                TIMESTAMP WITH TIME ZONE,
    resolved_by_principal_id   VARCHAR(255),
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

ALTER TABLE control_exceptions
    ADD CONSTRAINT control_exceptions_status_check
    CHECK (status IN ('OPEN', 'ASSIGNED', 'RESOLVED'));

-- One exception per run: a run's own finding is immutable, so its
-- resolution workflow is a single, one-to-one companion row, not a
-- collection.
CREATE UNIQUE INDEX idx_control_exceptions_run ON control_exceptions (control_run_id);

ALTER TABLE control_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE control_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON control_exceptions
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

-- ListOpenExceptions / GetCloseBlockingControls both ask "what's
-- outstanding for this entity, right now" — OPEN/ASSIGNED exceptions
-- across a tenant's legal entities.
CREATE INDEX idx_control_exceptions_tenant_entity_status
    ON control_exceptions (tenant_id, legal_entity_id, status);
