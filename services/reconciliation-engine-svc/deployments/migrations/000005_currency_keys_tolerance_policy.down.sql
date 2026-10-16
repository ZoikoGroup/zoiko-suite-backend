-- Migration: 000005_currency_keys_tolerance_policy.down.sql
--
-- Restores the 000004 schema. Not every row written under 000005 has a
-- 000004 shape:
--   * BALANCE exceptions belong to no item, and 000004 requires one. They
--     cannot be deleted (exceptions are evidence), so the down migration
--     refuses to run while any exist.
--   * Definitions and runs written under 000005 have no single tolerance; the
--     strictest one (0) is backfilled.
--   * Snapshot and certification totals are backfilled as 000004 computed
--     them — adding currencies together — because that is the old schema.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_exceptions WHERE kind = 'BALANCE') THEN
        RAISE EXCEPTION 'cannot downgrade below 000005: BALANCE exceptions exist and have no 000004 representation';
    END IF;
END $$;

ALTER TABLE certifications DROP CONSTRAINT IF EXISTS certifications_per_currency_check;
ALTER TABLE certifications DISABLE TRIGGER trigger_certifications_immutable;
UPDATE certifications c SET total_asserted_minor_units = COALESCE((
    SELECT SUM((t->>'total_minor_units')::bigint)
    FROM population_snapshots ps, jsonb_array_elements(ps.control_totals) t
    WHERE ps.run_id = c.run_id), 0)
WHERE total_asserted_minor_units IS NULL;
ALTER TABLE certifications ENABLE TRIGGER trigger_certifications_immutable;
ALTER TABLE certifications ALTER COLUMN total_asserted_minor_units SET NOT NULL;
ALTER TABLE certifications DROP COLUMN IF EXISTS control_totals;

DROP INDEX IF EXISTS uq_reconciliation_exceptions_balance;
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_shape_check;
ALTER TABLE reconciliation_exceptions
    ALTER COLUMN item_id SET NOT NULL,
    ALTER COLUMN side SET NOT NULL,
    ALTER COLUMN ref_id SET NOT NULL,
    DROP COLUMN IF EXISTS detail,
    DROP COLUMN IF EXISTS dimension_value,
    DROP COLUMN IF EXISTS dimension,
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS kind;

CREATE OR REPLACE FUNCTION enforce_exception_resolution()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation exceptions cannot be deleted';
    END IF;

    IF OLD.status = 'Resolved' THEN
        RAISE EXCEPTION 'reconciliation exception % is already Resolved and immutable', OLD.exception_id;
    END IF;

    IF NEW.status <> 'Resolved' THEN
        RAISE EXCEPTION 'reconciliation exceptions may only transition Open -> Resolved';
    END IF;
    IF NEW.resolved_by_match_id IS NULL THEN
        RAISE EXCEPTION 'resolving an exception requires resolved_by_match_id';
    END IF;
    IF NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.item_id IS DISTINCT FROM OLD.item_id
        OR NEW.side IS DISTINCT FROM OLD.side
        OR NEW.ref_id IS DISTINCT FROM OLD.ref_id
        OR NEW.reason IS DISTINCT FROM OLD.reason
        OR NEW.amount_minor_units IS DISTINCT FROM OLD.amount_minor_units
        OR NEW.raised_at IS DISTINCT FROM OLD.raised_at
        OR NEW.raised_by IS DISTINCT FROM OLD.raised_by
    THEN
        RAISE EXCEPTION 'reconciliation exception % may only have status/resolved_at/resolved_by_match_id change', OLD.exception_id;
    END IF;
    NEW.resolved_at := COALESCE(NEW.resolved_at, NOW());
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE population_snapshots DROP CONSTRAINT IF EXISTS population_snapshots_no_cross_currency_total;
ALTER TABLE population_snapshots DISABLE TRIGGER trigger_population_snapshots_immutable;
UPDATE population_snapshots ps SET total_amount_minor_units = COALESCE((
    SELECT SUM((t->>'total_minor_units')::bigint) FROM jsonb_array_elements(ps.control_totals) t), 0)
WHERE total_amount_minor_units IS NULL;
ALTER TABLE population_snapshots ENABLE TRIGGER trigger_population_snapshots_immutable;
ALTER TABLE population_snapshots ALTER COLUMN total_amount_minor_units SET DEFAULT 0;
ALTER TABLE population_snapshots ALTER COLUMN total_amount_minor_units SET NOT NULL;

-- Runs: backfill with the lifecycle trigger off (the backfill touches pinned
-- columns), then restore 000004's function.
ALTER TABLE reconciliation_runs DROP CONSTRAINT IF EXISTS reconciliation_runs_rules_check;
ALTER TABLE reconciliation_runs DISABLE TRIGGER trigger_run_lifecycle;
UPDATE reconciliation_runs SET tolerance_amount_minor_units = 0 WHERE tolerance_amount_minor_units IS NULL;
ALTER TABLE reconciliation_runs ENABLE TRIGGER trigger_run_lifecycle;
ALTER TABLE reconciliation_runs
    ALTER COLUMN tolerance_amount_minor_units SET NOT NULL,
    DROP COLUMN IF EXISTS balance_dimensions,
    DROP COLUMN IF EXISTS match_key_fields,
    DROP COLUMN IF EXISTS tolerance_policy;

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
    THEN
        RAISE EXCEPTION 'reconciliation run % cannot change its legal entity, period, scope or amount scale', OLD.run_id;
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

ALTER TABLE reconciliation_definitions DROP CONSTRAINT IF EXISTS reconciliation_definitions_tolerance_policy_check;
ALTER TABLE reconciliation_definitions DISABLE TRIGGER trigger_definition_versioning;
UPDATE reconciliation_definitions SET tolerance_amount_minor_units = 0 WHERE tolerance_amount_minor_units IS NULL;
ALTER TABLE reconciliation_definitions ENABLE TRIGGER trigger_definition_versioning;
ALTER TABLE reconciliation_definitions
    ALTER COLUMN tolerance_amount_minor_units SET NOT NULL,
    DROP COLUMN IF EXISTS balance_dimensions,
    DROP COLUMN IF EXISTS tolerance_policy;

CREATE OR REPLACE FUNCTION enforce_definition_versioning()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation definitions cannot be deleted';
    END IF;
    IF NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id THEN
        RAISE EXCEPTION 'reconciliation definition % cannot change legal_entity_id', OLD.definition_id;
    END IF;
    IF (NEW.match_key_fields, NEW.tolerance_amount_minor_units, NEW.materiality_amount_minor_units,
        NEW.amount_scale, NEW.populations)
       IS DISTINCT FROM
       (OLD.match_key_fields, OLD.tolerance_amount_minor_units, OLD.materiality_amount_minor_units,
        OLD.amount_scale, OLD.populations) THEN
        NEW.version := OLD.version + 1;
        NEW.updated_at := NOW();
    ELSE
        NEW.version := OLD.version;
        NEW.updated_at := OLD.updated_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
