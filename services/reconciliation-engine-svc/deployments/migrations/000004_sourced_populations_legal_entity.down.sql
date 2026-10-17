-- Migration: 000004_sourced_populations_legal_entity.down.sql
--
-- Restores the 000003 schema. Columns added by 000004 are dropped with their
-- data; the non-negative amount checks come back NOT VALID because signed
-- amounts written after 000004 may exist.

ALTER TABLE reconciliation_exceptions
    ADD CONSTRAINT reconciliation_exceptions_amount_minor_units_check CHECK (amount_minor_units >= 0) NOT VALID;
ALTER TABLE population_items
    ADD CONSTRAINT population_items_amount_minor_units_check CHECK (amount_minor_units >= 0) NOT VALID;

DROP INDEX IF EXISTS uq_population_items_source_record;
ALTER TABLE population_items DROP CONSTRAINT IF EXISTS population_items_sourced_check;
ALTER TABLE population_items
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS source_record_id;

ALTER TABLE population_snapshots DROP CONSTRAINT IF EXISTS population_snapshots_sourced_check;
ALTER TABLE population_snapshots
    DROP COLUMN IF EXISTS declared_totals,
    DROP COLUMN IF EXISTS declared_row_count,
    DROP COLUMN IF EXISTS control_totals,
    DROP COLUMN IF EXISTS amount_scale,
    DROP COLUMN IF EXISTS source_watermark,
    DROP COLUMN IF EXISTS spec_ref;

-- 000001's lifecycle function.
CREATE OR REPLACE FUNCTION enforce_run_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation runs cannot be deleted';
    END IF;

    IF OLD.status = 'Superseded' THEN
        RAISE EXCEPTION 'reconciliation run % is Superseded and immutable', OLD.run_id;
    END IF;

    IF OLD.status IN ('Certified', 'Failed') THEN
        IF NEW.status <> 'Superseded' THEN
            RAISE EXCEPTION 'reconciliation run % is % and immutable except for supersession', OLD.run_id, OLD.status;
        END IF;
        IF NEW.definition_id IS DISTINCT FROM OLD.definition_id
            OR NEW.definition_version IS DISTINCT FROM OLD.definition_version
            OR NEW.tolerance_amount_minor_units IS DISTINCT FROM OLD.tolerance_amount_minor_units
            OR NEW.materiality_amount_minor_units IS DISTINCT FROM OLD.materiality_amount_minor_units
            OR NEW.certified_at IS DISTINCT FROM OLD.certified_at
            OR NEW.certified_by IS DISTINCT FROM OLD.certified_by
            OR NEW.started_at IS DISTINCT FROM OLD.started_at
            OR NEW.started_by IS DISTINCT FROM OLD.started_by
        THEN
            RAISE EXCEPTION 'reconciliation run % is % and only status/superseded_by_run_id/superseded_reason may change', OLD.run_id, OLD.status;
        END IF;
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

DROP INDEX IF EXISTS idx_reconciliation_runs_definition;
DROP INDEX IF EXISTS uq_reconciliation_runs_replacement;
ALTER TABLE reconciliation_runs DROP CONSTRAINT IF EXISTS reconciliation_runs_superseded_actor_check;
ALTER TABLE reconciliation_runs DROP CONSTRAINT IF EXISTS reconciliation_runs_sourced_check;
ALTER TABLE reconciliation_runs
    DROP COLUMN IF EXISTS superseded_at,
    DROP COLUMN IF EXISTS superseded_by,
    DROP COLUMN IF EXISTS amount_scale,
    DROP COLUMN IF EXISTS scope,
    DROP COLUMN IF EXISTS period_id,
    DROP COLUMN IF EXISTS legal_entity_id;

-- 000003's versioning function.
CREATE OR REPLACE FUNCTION enforce_definition_versioning()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation definitions cannot be deleted';
    END IF;
    IF (NEW.match_key_fields, NEW.tolerance_amount_minor_units, NEW.materiality_amount_minor_units)
       IS DISTINCT FROM
       (OLD.match_key_fields, OLD.tolerance_amount_minor_units, OLD.materiality_amount_minor_units) THEN
        NEW.version := OLD.version + 1;
        NEW.updated_at := NOW();
    ELSE
        NEW.version := OLD.version;
        NEW.updated_at := OLD.updated_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Restoring UNIQUE (tenant_id, name) fails if two entities now share a name;
-- that is the correct outcome for a down migration that would merge them.
DROP INDEX IF EXISTS uq_reconciliation_definitions_entity_name;
ALTER TABLE reconciliation_definitions ADD CONSTRAINT reconciliation_definitions_tenant_id_name_key UNIQUE (tenant_id, name);
ALTER TABLE reconciliation_definitions DROP CONSTRAINT IF EXISTS reconciliation_definitions_sourced_check;
ALTER TABLE reconciliation_definitions
    DROP COLUMN IF EXISTS populations,
    DROP COLUMN IF EXISTS amount_scale,
    DROP COLUMN IF EXISTS legal_entity_id;
