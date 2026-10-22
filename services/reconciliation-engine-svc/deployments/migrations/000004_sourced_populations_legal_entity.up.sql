-- Migration: 000004_sourced_populations_legal_entity.up.sql
--
-- A1  Populations are read from their owning services through the control
--     population contract, not pushed in the StartRun body. A definition names
--     the two populations (side, system, population, params) and a fixed-point
--     amount_scale; every snapshot pins the spec it was read with, the source
--     watermark, the source's declared totals and its own per-currency control
--     totals; every item keeps its source record id and currency.
-- A10 Definitions and runs carry legal_entity_id; authorization is evaluated
--     against it instead of the tenant id.
-- A13 Supersession records who superseded and when; a run can be the
--     replacement of at most one run.
--
-- Rows written before this migration have none of these values and none can be
-- invented for them. The new NOT NULL rules are therefore NOT VALID CHECK
-- constraints: enforced for every new or updated row, not retro-applied to old
-- ones. The service refuses to authorize or run such legacy rows
-- (domain.ErrLegacyRecord).

-- ── Definitions ─────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_definitions
    ADD COLUMN legal_entity_id UUID,
    ADD COLUMN amount_scale    SMALLINT CHECK (amount_scale BETWEEN 0 AND 12),
    ADD COLUMN populations     JSONB;

ALTER TABLE reconciliation_definitions
    ADD CONSTRAINT reconciliation_definitions_sourced_check CHECK (
        legal_entity_id IS NOT NULL
        AND amount_scale IS NOT NULL
        AND populations IS NOT NULL
        AND jsonb_typeof(populations) = 'array'
        AND jsonb_array_length(populations) = 2
    ) NOT VALID;

-- A definition name is unique within its legal entity, not across the tenant:
-- the upsert must never be able to move a definition to another entity.
ALTER TABLE reconciliation_definitions DROP CONSTRAINT IF EXISTS reconciliation_definitions_tenant_id_name_key;
CREATE UNIQUE INDEX uq_reconciliation_definitions_entity_name
    ON reconciliation_definitions (tenant_id, legal_entity_id, name);

-- The trigger stays the single owner of version/updated_at (B4). The rule
-- columns now include the populations and the amount scale; legal_entity_id
-- can never change.
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

-- ── Runs ────────────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_runs
    ADD COLUMN legal_entity_id UUID,
    ADD COLUMN period_id       VARCHAR(7) CHECK (period_id ~ '^\d{4}-(0[1-9]|1[0-2])$'),
    ADD COLUMN scope           JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN amount_scale    SMALLINT CHECK (amount_scale BETWEEN 0 AND 12),
    ADD COLUMN superseded_by   VARCHAR(128),
    ADD COLUMN superseded_at   TIMESTAMPTZ;

ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_sourced_check CHECK (
        legal_entity_id IS NOT NULL AND amount_scale IS NOT NULL
    ) NOT VALID;

ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_superseded_actor_check CHECK (
        status <> 'Superseded' OR (superseded_by IS NOT NULL AND superseded_by_run_id IS NOT NULL)
    ) NOT VALID;

-- One replacement supersedes at most one run. Scoped to rows superseded under
-- this migration's rules (superseded_by set), so legacy data cannot block it.
CREATE UNIQUE INDEX uq_reconciliation_runs_replacement
    ON reconciliation_runs (superseded_by_run_id) WHERE superseded_by IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_reconciliation_runs_definition ON reconciliation_runs (definition_id);

-- 000001's lifecycle rules, with the new pinned columns added to the
-- immutable set of a Certified/Failed run, and legal_entity_id, period_id,
-- scope and amount_scale fixed from creation in every state.
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

-- ── Snapshots and items ─────────────────────────────────────────────────────

ALTER TABLE population_snapshots
    ADD COLUMN spec_ref           TEXT,
    ADD COLUMN source_watermark   TEXT,
    ADD COLUMN amount_scale       SMALLINT CHECK (amount_scale BETWEEN 0 AND 12),
    ADD COLUMN control_totals     JSONB,
    ADD COLUMN declared_row_count BIGINT CHECK (declared_row_count >= 0),
    ADD COLUMN declared_totals    JSONB;

-- A new snapshot cannot be frozen without the evidence that it is complete and
-- reproducible, and its row count must equal what the source declared.
ALTER TABLE population_snapshots
    ADD CONSTRAINT population_snapshots_sourced_check CHECK (
        spec_ref IS NOT NULL AND btrim(spec_ref) <> ''
        AND source_watermark IS NOT NULL AND btrim(source_watermark) <> ''
        AND amount_scale IS NOT NULL
        AND control_totals IS NOT NULL
        AND declared_totals IS NOT NULL
        AND declared_row_count = item_count
    ) NOT VALID;

ALTER TABLE population_items
    ADD COLUMN source_record_id VARCHAR(256),
    ADD COLUMN currency         CHAR(3) CHECK (currency ~ '^[A-Z]{3}$');

ALTER TABLE population_items
    ADD CONSTRAINT population_items_sourced_check CHECK (
        source_record_id IS NOT NULL AND currency IS NOT NULL
    ) NOT VALID;

-- record_id is the source's stable identity: a population holds it once.
CREATE UNIQUE INDEX uq_population_items_source_record
    ON population_items (snapshot_id, source_record_id) WHERE source_record_id IS NOT NULL;

-- Source amounts are signed (the contract's GL and bank populations carry
-- negative values), so the non-negative checks of 000001 go.
ALTER TABLE population_items DROP CONSTRAINT IF EXISTS population_items_amount_minor_units_check;
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT IF EXISTS reconciliation_exceptions_amount_minor_units_check;
