-- Migration: 000011_certification_revocation.down.sql
--
-- Manual rollback only, and refused while any revocation is recorded or any
-- run's certification is REVOKED: dropping the table would lose who revoked
-- which certification, why, on what evidence and by what authority, with no
-- way to reconstruct it (the revoker's authorization decision is stored
-- nowhere else), and 000007's rule (Certified <=> certification CERTIFIED)
-- cannot hold for a revoked run; silently re-certifying it would undo the
-- revocation. The certifications themselves are untouched either way.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_certification_revocations)
        OR EXISTS (SELECT 1 FROM reconciliation_runs WHERE certification_state = 'REVOKED')
    THEN
        RAISE EXCEPTION 'cannot roll back 000011: recorded certification revocations exist';
    END IF;
END $$;

DROP TABLE IF EXISTS reconciliation_certification_revocations;
DROP FUNCTION IF EXISTS enforce_revocation_revokes();
DROP FUNCTION IF EXISTS reconciliation_refs_nonblank(TEXT[]);

ALTER TABLE reconciliation_runs DROP CONSTRAINT reconciliation_runs_state_dimensions_check;
ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_state_dimensions_check CHECK (
        ((status = 'Certified') = (certification_state = 'CERTIFIED'))
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

ALTER TABLE reconciliation_runs DROP CONSTRAINT reconciliation_runs_certification_state_check;
ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_certification_state_check
        CHECK (certification_state IN ('UNCERTIFIED', 'ELIGIBLE', 'CERTIFIED', 'SUPERSEDED'));

-- 000006's lifecycle trigger, as it was.
CREATE OR REPLACE FUNCTION enforce_run_lifecycle()
RETURNS trigger AS $$
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
            OR (OLD.certification_state = 'CERTIFIED' AND NEW.certification_state = 'SUPERSEDED')
        ) THEN
            RAISE EXCEPTION 'invalid reconciliation run certification transition from % to %',
                OLD.certification_state, NEW.certification_state;
        END IF;
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
