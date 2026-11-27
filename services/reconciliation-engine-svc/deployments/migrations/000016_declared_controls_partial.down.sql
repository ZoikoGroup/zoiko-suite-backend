-- Reverting is possible only while nothing relies on what 000016 added: no snapshot may be a
-- declared subset, no snapshot may carry declared controls (that evidence would be destroyed),
-- and no run may be PARTIAL (the restored lifecycle rule refuses PARTIAL, and a run's result
-- cannot be rewritten). Outcomes of sources that declared nothing are all NOT_DECLARED and are
-- derivable, so they do not block.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM population_snapshots WHERE completeness = 'PARTIAL' OR declared_controls <> '{}'::jsonb) THEN
        RAISE EXCEPTION 'cannot revert 000016: population snapshots carry declared controls or a declared subset';
    END IF;
    IF EXISTS (SELECT 1 FROM reconciliation_runs WHERE result_state = 'PARTIAL') THEN
        RAISE EXCEPTION 'cannot revert 000016: a reconciliation run is PARTIAL';
    END IF;
END $$;

-- The lifecycle function as 000011 defined it.
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

DROP FUNCTION IF EXISTS run_has_partial_population(TEXT);
DROP INDEX IF EXISTS idx_population_snapshots_rollforward;
ALTER TABLE population_snapshots DROP CONSTRAINT IF EXISTS population_snapshots_completeness_matches_declaration;
ALTER TABLE population_snapshots
    DROP COLUMN IF EXISTS control_outcomes,
    DROP COLUMN IF EXISTS declared_controls,
    DROP COLUMN IF EXISTS completeness;
