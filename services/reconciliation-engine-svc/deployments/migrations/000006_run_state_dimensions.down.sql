-- Migration: 000006_run_state_dimensions.down.sql
--
-- Restores the 000005 schema. The transition history is evidence of how each
-- run's result and certification moved; dropping it would destroy that, so the
-- down migration refuses once any live (non-backfill) transition exists.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_run_transitions WHERE actor <> 'migration:000006') THEN
        RAISE EXCEPTION 'cannot downgrade below 000006: run state transitions exist and have no 000005 representation';
    END IF;
END $$;

DROP TRIGGER IF EXISTS trigger_run_creation ON reconciliation_runs;
DROP FUNCTION IF EXISTS enforce_run_creation();

ALTER TABLE reconciliation_runs DROP CONSTRAINT IF EXISTS reconciliation_runs_state_dimensions_check;

DROP TABLE IF EXISTS reconciliation_run_transitions;

ALTER TABLE reconciliation_runs
    DROP COLUMN IF EXISTS failure_reason,
    DROP COLUMN IF EXISTS failure_code,
    DROP COLUMN IF EXISTS certification_state,
    DROP COLUMN IF EXISTS result_state;

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
                IF NEW.status <> 'Running' THEN
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
