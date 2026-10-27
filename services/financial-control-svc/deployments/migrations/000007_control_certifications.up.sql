-- Migration: 000007_control_certifications.up.sql
--
-- SPEC_DEVIATIONS A2. Invariant 9: "A certified control result always
-- identifies the certifier, certification authority, time, run version,
-- unresolved exceptions and impact assessment." §6's canonical
-- ControlCertification: "run_id, certifier, authority snapshot, result,
-- unresolved exceptions, assessed exposure, timestamp, evidence package."
--
-- Until now a certification was only a state change plus a transition row
-- (actor, reason, time); the authority, the exceptions it accepted and their
-- assessed exposure were not recorded at all. One row per certification,
-- written in CertifyRun's own transaction under the run's row lock, never
-- updated or deleted (reject_control_mutation).
--
-- No backfill: a run certified before this migration had no authority
-- recorded, and one is not invented. Such a run has no row here; its
-- certification is visible only through control_run_transitions.

CREATE TABLE control_certifications (
    certification_id            UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                   UUID          NOT NULL,
    run_id                      UUID          NOT NULL UNIQUE REFERENCES control_runs(run_id),
    -- Invariant 9 "run version": the run's version as certified.
    run_version                 INT           NOT NULL CHECK (run_version > 0),
    -- The CERTIFICATION -> CERTIFIED history row this certification records.
    certification_transition_id BIGINT        NOT NULL UNIQUE REFERENCES control_run_transitions(transition_id),
    certifier                   VARCHAR(255)  NOT NULL CHECK (certifier <> ''),
    -- authorization-svc's live decision for this certifier, action and entity.
    authority_snapshot          JSONB         NOT NULL CHECK (
        jsonb_typeof(authority_snapshot) = 'object'
        AND coalesce(authority_snapshot->>'access_decision_id', '') <> ''
        AND authority_snapshot->>'decision_outcome' = 'GRANTED'),
    result                      VARCHAR(32)   NOT NULL CHECK (result IN ('PASS','PASS_WITH_APPROVED_EXCEPTIONS')),
    unresolved_exceptions       JSONB         NOT NULL CHECK (jsonb_typeof(unresolved_exceptions) = 'array'),
    assessed_exposure           JSONB         NOT NULL CHECK (jsonb_typeof(assessed_exposure) = 'object'),
    certified_at                TIMESTAMPTZ   NOT NULL DEFAULT now(),
    -- The evidence package sealed at execution that this certification relies on.
    evidence_package_id         UUID          NOT NULL REFERENCES evidence_packages(package_id),
    evidence_digest             VARCHAR(71)   NOT NULL CHECK (evidence_digest ~ '^sha256:[0-9a-f]{64}$'),
    reason                      TEXT          NOT NULL DEFAULT '',
    correlation_id              VARCHAR(255)  NOT NULL DEFAULT ''
);

CREATE TRIGGER trg_immutable_certifications BEFORE UPDATE OR DELETE ON control_certifications
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();

ALTER TABLE control_certifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE control_certifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON control_certifications FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
