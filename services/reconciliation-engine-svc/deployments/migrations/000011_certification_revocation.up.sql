-- Migration: 000011_certification_revocation.up.sql
--
-- SPEC_DEVIATIONS A12 (revocation part). ZS-SVC-Z-001 §5 lists REVOKED among
-- the certification states and DQC-05 "New material evidence may revoke
-- certification prospectively; prior certification history remains"; RB-05
-- "Revoke affected certification prospectively, preserve prior evidence";
-- NP-47 a revocation with no reason/evidence must fail safe; ZS-CONTROL-001
-- §27 "Reopen/supersede certified run — restricted command, reason, evidence,
-- prior certification retained". The shape mirrors financial-control-svc's
-- 000006_certification_revocation.
--
-- A revoked run keeps status Certified (the run was certified; that fact and
-- its history do not change) and moves only its certification dimension,
-- CERTIFIED -> REVOKED. Its result is untouched and its certification row
-- (000010's manifest evidence: populations, manual matches, authority) is
-- never touched: certifications is immutable (reject_immutable_row).
--
-- reconciliation_certification_revocations records each revocation: who, why,
-- on what evidence and authority, and exactly which sealed certification it
-- revokes (its id, its manifest hash and its CERTIFIED transition). It is
-- append-only and one per run: REVOKED is terminal, except that a revoked run
-- can still be superseded, and stays REVOKED when it is.

-- ── certification_state vocabulary ──────────────────────────────────────────

DO $$
DECLARE c TEXT;
DECLARE n INT;
BEGIN
    SELECT count(*), min(conname) INTO n, c FROM pg_constraint
    WHERE conrelid = 'reconciliation_runs'::regclass AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%certification_state%'
      AND pg_get_constraintdef(oid) LIKE '%''UNCERTIFIED''%'
      AND pg_get_constraintdef(oid) NOT LIKE '%status%';
    IF n <> 1 THEN
        RAISE EXCEPTION '000011: expected exactly one certification_state vocabulary CHECK on reconciliation_runs, found %', n;
    END IF;
    EXECUTE format('ALTER TABLE reconciliation_runs DROP CONSTRAINT %I', c);
END $$;

ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_certification_state_check
        CHECK (certification_state IN ('UNCERTIFIED', 'ELIGIBLE', 'CERTIFIED', 'REVOKED', 'SUPERSEDED'));

-- ── Cross-dimension invariants ──────────────────────────────────────────────
-- 000007's, with its first clause (Certified <=> certification CERTIFIED)
-- split in three: a Certified run's certification is CERTIFIED or REVOKED;
-- CERTIFIED only on a Certified run; REVOKED only on a Certified run, or a
-- Superseded one (a revoked run replaced later keeps its revocation).

ALTER TABLE reconciliation_runs DROP CONSTRAINT reconciliation_runs_state_dimensions_check;
ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_state_dimensions_check CHECK (
        (status <> 'Certified' OR certification_state IN ('CERTIFIED', 'REVOKED'))
        AND (certification_state <> 'CERTIFIED' OR status = 'Certified')
        AND (certification_state <> 'REVOKED' OR status IN ('Certified', 'Superseded'))
        AND (certification_state NOT IN ('ELIGIBLE', 'CERTIFIED') OR result_state = 'MATCH'
             OR (result_state = 'VARIANCE' AND COALESCE((certification_profile ->> 'permit_accepted_risk')::boolean, FALSE)))
        AND ((status = 'Superseded') = (result_state = 'SUPERSEDED'))
        AND (certification_state <> 'SUPERSEDED' OR status = 'Superseded')
        AND (status <> 'Planned' OR (result_state = 'NOT_RUN' AND certification_state = 'UNCERTIFIED'))
        AND (status <> 'ExceptionsOpen' OR result_state = 'VARIANCE')
        AND (status <> 'Failed' OR (result_state = 'INDETERMINATE' AND failure_code IS NOT NULL))
        AND ((failure_code IS NULL) = (failure_reason IS NULL))
        AND (failure_code IS NULL OR status IN ('Failed', 'Superseded'))
    );

-- ── The revocation record ───────────────────────────────────────────────────

-- Every evidence reference names something: no NULL and no blank element.
CREATE OR REPLACE FUNCTION reconciliation_refs_nonblank(refs TEXT[])
RETURNS BOOLEAN AS $$
    SELECT COALESCE(bool_and(e IS NOT NULL AND length(btrim(e)) > 0), FALSE) FROM unnest(refs) AS e;
$$ LANGUAGE sql IMMUTABLE;

CREATE TABLE reconciliation_certification_revocations (
    revocation_id               TEXT         PRIMARY KEY
        CHECK (revocation_id ~ '^rcv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    run_id                      TEXT         NOT NULL UNIQUE REFERENCES reconciliation_runs(run_id),
    -- The certification being revoked, which itself is never touched: its
    -- sealed record, the manifest hash it was sealed under, and the run's
    -- CERTIFICATION -> CERTIFIED transition.
    certification_id            TEXT         NOT NULL UNIQUE REFERENCES certifications(certification_id),
    revoked_manifest_sha256     CHAR(64)     NOT NULL CHECK (revoked_manifest_sha256 ~ '^[0-9a-f]{64}$'),
    certification_transition_id BIGINT       NOT NULL REFERENCES reconciliation_run_transitions(transition_id),
    certified_by                VARCHAR(128) NOT NULL,
    certified_at                TIMESTAMPTZ  NOT NULL,
    reason                      TEXT         NOT NULL CHECK (length(btrim(reason)) > 0),
    evidence_refs               TEXT[]       NOT NULL
        CHECK (cardinality(evidence_refs) > 0 AND reconciliation_refs_nonblank(evidence_refs)),
    -- The revoker's authority: authorization-svc's live decision, as a
    -- certification records its certifier's (000010).
    authority_snapshot          JSONB        NOT NULL,
    revoked_by                  VARCHAR(128) NOT NULL,
    -- Prospective (RB-05): the revocation takes effect from this instant; it
    -- does not reach back into decisions taken on the certification before.
    revoked_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT reconciliation_certification_revocations_authority_check CHECK (
        jsonb_typeof(authority_snapshot) = 'object'
        AND coalesce(authority_snapshot->>'access_decision_id', '') <> ''
        AND authority_snapshot->>'decision_outcome' = 'GRANTED'
        AND authority_snapshot->>'principal_id' = revoked_by
    ),
    -- Independence: the certifier cannot revoke their own certification.
    CONSTRAINT reconciliation_certification_revocations_independence_check CHECK (revoked_by <> certified_by),
    CONSTRAINT reconciliation_certification_revocations_prospective_check CHECK (revoked_at >= certified_at)
);

CREATE TRIGGER trigger_certification_revocations_immutable
    BEFORE UPDATE OR DELETE ON reconciliation_certification_revocations
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE reconciliation_certification_revocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_certification_revocations FORCE ROW LEVEL SECURITY;
CREATE POLICY reconciliation_certification_revocations_tenant_isolation ON reconciliation_certification_revocations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- A revocation record exists only for a run whose certification is REVOKED by
-- the time its transaction commits (the run's lifecycle trigger below gives
-- the converse: REVOKED only with the record).
CREATE OR REPLACE FUNCTION enforce_revocation_revokes()
RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM reconciliation_runs r
                   WHERE r.run_id = NEW.run_id AND r.certification_state = 'REVOKED') THEN
        RAISE EXCEPTION 'revocation % records a revocation of run %, whose certification is not REVOKED',
            NEW.revocation_id, NEW.run_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER trigger_revocation_revokes
    AFTER INSERT ON reconciliation_certification_revocations
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_revocation_revokes();

-- ── Lifecycle trigger ───────────────────────────────────────────────────────

-- 000006's rules, plus revocation: the certification edge CERTIFIED ->
-- REVOKED, and the one move of a Certified run other than supersession. It
-- changes the certification state and nothing else, and only once the run's
-- revocation record exists in the same transaction. A revoked run can still
-- be superseded (its certification stays REVOKED).
CREATE OR REPLACE FUNCTION enforce_run_lifecycle()
RETURNS trigger AS $$
DECLARE
    unrevoked reconciliation_runs%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation runs cannot be deleted';
    END IF;

    IF OLD.status = 'Superseded' THEN
        RAISE EXCEPTION 'reconciliation run % is Superseded and immutable', OLD.run_id;
    END IF;

    IF NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.period_id IS DISTINCT FROM OLD.period_id
        OR NEW.scope IS DISTINCT FROM OLD.scope
        OR NEW.amount_scale IS DISTINCT FROM OLD.amount_scale
        OR NEW.tolerance_policy IS DISTINCT FROM OLD.tolerance_policy
        OR NEW.match_key_fields IS DISTINCT FROM OLD.match_key_fields
        OR NEW.balance_dimensions IS DISTINCT FROM OLD.balance_dimensions
        OR NEW.tolerance_amount_minor_units IS DISTINCT FROM OLD.tolerance_amount_minor_units
        OR NEW.materiality_amount_minor_units IS DISTINCT FROM OLD.materiality_amount_minor_units
        OR NEW.definition_id IS DISTINCT FROM OLD.definition_id
        OR NEW.definition_version IS DISTINCT FROM OLD.definition_version
    THEN
        RAISE EXCEPTION 'reconciliation run % cannot change its pinned scope or rules', OLD.run_id;
    END IF;

    IF (NEW.failure_code, NEW.failure_reason) IS DISTINCT FROM (OLD.failure_code, OLD.failure_reason)
        AND NOT (OLD.status <> 'Failed' AND NEW.status = 'Failed' AND OLD.failure_code IS NULL)
    THEN
        RAISE EXCEPTION 'reconciliation run % failure details are written once, on entering Failed', OLD.run_id;
    END IF;

    IF NEW.result_state <> OLD.result_state THEN
        IF NEW.result_state = 'NOT_RUN' THEN
            RAISE EXCEPTION 'reconciliation run % result cannot be reset to NOT_RUN', OLD.run_id;
        END IF;
        IF NEW.result_state = 'PARTIAL' THEN
            RAISE EXCEPTION 'reconciliation run % cannot enter PARTIAL: no defined subset semantics (A16)', OLD.run_id;
        END IF;
        IF OLD.result_state = 'INDETERMINATE' AND NEW.result_state <> 'SUPERSEDED' THEN
            RAISE EXCEPTION 'reconciliation run % is INDETERMINATE and cannot become %', OLD.run_id, NEW.result_state;
        END IF;
    END IF;

    IF NEW.certification_state <> OLD.certification_state THEN
        IF NOT (
            (OLD.certification_state = 'UNCERTIFIED' AND NEW.certification_state = 'ELIGIBLE')
            OR (OLD.certification_state = 'ELIGIBLE' AND NEW.certification_state IN ('UNCERTIFIED', 'CERTIFIED'))
            OR (OLD.certification_state = 'CERTIFIED' AND NEW.certification_state IN ('REVOKED', 'SUPERSEDED'))
        ) THEN
            RAISE EXCEPTION 'invalid reconciliation run certification transition from % to %',
                OLD.certification_state, NEW.certification_state;
        END IF;
    END IF;

    IF OLD.status = 'Certified' AND NEW.status = 'Certified' THEN
        IF NOT (OLD.certification_state = 'CERTIFIED' AND NEW.certification_state = 'REVOKED') THEN
            RAISE EXCEPTION 'reconciliation run % is Certified and immutable except for revocation or supersession', OLD.run_id;
        END IF;
        unrevoked := NEW;
        unrevoked.certification_state := OLD.certification_state;
        IF unrevoked IS DISTINCT FROM OLD THEN
            RAISE EXCEPTION 'revoking reconciliation run % changes its certification state and nothing else', OLD.run_id;
        END IF;
        IF NOT EXISTS (SELECT 1 FROM reconciliation_certification_revocations v WHERE v.run_id = NEW.run_id) THEN
            RAISE EXCEPTION 'reconciliation run % cannot be revoked without its revocation record', OLD.run_id;
        END IF;
        RETURN NEW;
    END IF;

    IF OLD.status IN ('Certified', 'Failed') THEN
        IF NEW.status <> 'Superseded' THEN
            RAISE EXCEPTION 'reconciliation run % is % and immutable except for supersession', OLD.run_id, OLD.status;
        END IF;
        IF NEW.certified_at IS DISTINCT FROM OLD.certified_at
            OR NEW.certified_by IS DISTINCT FROM OLD.certified_by
            OR NEW.started_at IS DISTINCT FROM OLD.started_at
            OR NEW.started_by IS DISTINCT FROM OLD.started_by
            OR NEW.supersedes_run_id IS DISTINCT FROM OLD.supersedes_run_id
        THEN
            RAISE EXCEPTION 'reconciliation run % is % and only its supersession fields may change', OLD.run_id, OLD.status;
        END IF;
        NEW.superseded_at := COALESCE(NEW.superseded_at, NOW());
        RETURN NEW;
    END IF;

    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Planned' THEN
                IF NEW.status NOT IN ('Running', 'Failed') THEN
                    RAISE EXCEPTION 'invalid reconciliation run transition from Planned to %', NEW.status;
                END IF;
            WHEN 'Running' THEN
                IF NEW.status NOT IN ('ExceptionsOpen', 'Certified', 'Failed', 'Reperformed') THEN
                    RAISE EXCEPTION 'invalid reconciliation run transition from Running to %', NEW.status;
                END IF;
            WHEN 'ExceptionsOpen' THEN
                IF NEW.status NOT IN ('Running', 'Reperformed', 'Failed') THEN
                    RAISE EXCEPTION 'invalid reconciliation run transition from ExceptionsOpen to %', NEW.status;
                END IF;
            WHEN 'Reperformed' THEN
                IF NEW.status NOT IN ('Running', 'ExceptionsOpen', 'Certified', 'Failed') THEN
                    RAISE EXCEPTION 'invalid reconciliation run transition from Reperformed to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown reconciliation run status %', OLD.status;
        END CASE;
    END IF;

    IF NEW.status = 'Certified' THEN
        NEW.certified_at := COALESCE(NEW.certified_at, NOW());
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
