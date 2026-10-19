-- Migration: 000006_certification_revocation.up.sql
--
-- SPEC_DEVIATIONS A1. ZS-CONTROL-001 §7 names Revoked as a CERTIFICATION
-- state, orthogonal to the lifecycle; §27 "Reopen/supersede certified run —
-- restricted command, reason, evidence, prior certification retained";
-- ZS-SVC-Z-001 RB-05 "revoke affected certification prospectively, preserve
-- prior evidence"; NP-47 a revocation with no reason/evidence must fail safe.
--
-- A revoked run keeps lifecycle CERTIFIED (the run was certified; that fact
-- and its history do not change) and moves only its certification
-- dimension to REVOKED. 000001's CHECK forbade lifecycle CERTIFIED with any
-- certification other than CERTIFIED; it is replaced by one that also
-- admits REVOKED, plus a CHECK that REVOKED only ever follows a
-- certification (lifecycle CERTIFIED, or SUPERSEDED once a later run
-- replaces the revoked one).
--
-- control_certification_revocations records each revocation: who, why, on
-- what evidence, and which certification transition it revokes. It is
-- append-only (reject_control_mutation, as for every other history table)
-- and one per run: REVOKED is terminal.

DO $$
DECLARE c TEXT;
DECLARE n INT;
BEGIN
    SELECT count(*), min(conname) INTO n, c FROM pg_constraint
    WHERE conrelid = 'control_runs'::regclass AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%lifecycle_state)::text <> ''CERTIFIED''%certification_state)::text = ''CERTIFIED''%';
    IF n <> 1 THEN
        RAISE EXCEPTION '000006: expected exactly one lifecycle/certification CHECK on control_runs, found %', n;
    END IF;
    EXECUTE format('ALTER TABLE control_runs DROP CONSTRAINT %I', c);
END $$;

ALTER TABLE control_runs
    ADD CONSTRAINT chk_certified_lifecycle_certification
        CHECK (lifecycle_state <> 'CERTIFIED' OR certification_state IN ('CERTIFIED', 'REVOKED')),
    ADD CONSTRAINT chk_revoked_follows_certification
        CHECK (certification_state <> 'REVOKED' OR lifecycle_state IN ('CERTIFIED', 'SUPERSEDED'));

CREATE TABLE control_certification_revocations (
    revocation_id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                   UUID         NOT NULL,
    run_id                      UUID         NOT NULL UNIQUE REFERENCES control_runs(run_id),
    -- The certification being revoked, which itself is never touched: the
    -- CERTIFICATION -> CERTIFIED row of control_run_transitions.
    certification_transition_id BIGINT       NOT NULL REFERENCES control_run_transitions(transition_id),
    certified_by                VARCHAR(255) NOT NULL,
    certified_at                TIMESTAMPTZ  NOT NULL,
    reason                      TEXT         NOT NULL CHECK (length(btrim(reason)) > 0),
    evidence_refs               TEXT[]       NOT NULL CHECK (cardinality(evidence_refs) > 0),
    revoked_by                  VARCHAR(255) NOT NULL,
    -- Prospective (RB-05): the revocation takes effect from this instant; it
    -- does not reach back into decisions taken on the certification before.
    revoked_at                  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    correlation_id              VARCHAR(255) NOT NULL DEFAULT '',
    -- Independence: the certifier cannot revoke their own certification.
    CHECK (revoked_by <> certified_by)
);
CREATE INDEX idx_control_certification_revocations_tenant ON control_certification_revocations (tenant_id, run_id);

CREATE TRIGGER trg_immutable_certification_revocations BEFORE UPDATE OR DELETE ON control_certification_revocations
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();

ALTER TABLE control_certification_revocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE control_certification_revocations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON control_certification_revocations FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
