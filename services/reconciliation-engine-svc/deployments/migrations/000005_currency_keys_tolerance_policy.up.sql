-- Migration: 000005_currency_keys_tolerance_policy.up.sql
--
-- A2  Tolerance is per currency, amounts in different currencies are never
--     compared or added together: the cross-currency snapshot and
--     certification totals stop being written (per-currency control totals
--     replace them) and exceptions carry a currency.
-- A3  The definition's match key fields and balance dimensions are pinned on
--     every run; dimensional balance raises BALANCE exceptions, which belong
--     to no single item.
-- A5  tolerance_policy (per-currency absolute or percentage tolerance with an
--     explicit base side, optional date tolerance) replaces the single,
--     currency-less tolerance_amount_minor_units.
--
-- As in 000004, rows written earlier keep their values; the new rules are NOT
-- VALID constraints, enforced for every new or updated row.

-- ── Definitions ─────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_definitions
    ADD COLUMN tolerance_policy   JSONB,
    ADD COLUMN balance_dimensions TEXT[] NOT NULL DEFAULT '{}',
    ALTER COLUMN tolerance_amount_minor_units DROP NOT NULL;

ALTER TABLE reconciliation_definitions
    ADD CONSTRAINT reconciliation_definitions_tolerance_policy_check CHECK (
        tolerance_policy IS NOT NULL AND jsonb_typeof(tolerance_policy) = 'object'
        AND tolerance_amount_minor_units IS NULL
    ) NOT VALID;

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
        NEW.amount_scale, NEW.populations, NEW.tolerance_policy, NEW.balance_dimensions)
       IS DISTINCT FROM
       (OLD.match_key_fields, OLD.tolerance_amount_minor_units, OLD.materiality_amount_minor_units,
        OLD.amount_scale, OLD.populations, OLD.tolerance_policy, OLD.balance_dimensions) THEN
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
    ADD COLUMN tolerance_policy   JSONB,
    ADD COLUMN match_key_fields   TEXT[],
    ADD COLUMN balance_dimensions TEXT[],
    ALTER COLUMN tolerance_amount_minor_units DROP NOT NULL;

ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_rules_check CHECK (
        tolerance_policy IS NOT NULL AND match_key_fields IS NOT NULL AND balance_dimensions IS NOT NULL
        AND tolerance_amount_minor_units IS NULL
    ) NOT VALID;

-- 000004's lifecycle rules, with the pinned rules (tolerance policy, match
-- keys, balance dimensions) fixed from creation in every state: "tolerance
-- cannot be widened during a run" now holds before certification too.
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

-- ── Snapshots ───────────────────────────────────────────────────────────────

-- The single total added every currency together (INV-10, NP-22). New
-- snapshots carry only the per-currency control_totals from 000004.
ALTER TABLE population_snapshots ALTER COLUMN total_amount_minor_units DROP NOT NULL;
ALTER TABLE population_snapshots ALTER COLUMN total_amount_minor_units DROP DEFAULT;
ALTER TABLE population_snapshots
    ADD CONSTRAINT population_snapshots_no_cross_currency_total CHECK (total_amount_minor_units IS NULL) NOT VALID;

-- ── Exceptions ──────────────────────────────────────────────────────────────

ALTER TABLE reconciliation_exceptions
    ADD COLUMN kind            VARCHAR(16) NOT NULL DEFAULT 'ITEM' CHECK (kind IN ('ITEM', 'BALANCE')),
    ADD COLUMN currency        CHAR(3) CHECK (currency ~ '^[A-Z]{3}$'),
    ADD COLUMN dimension       VARCHAR(64),
    ADD COLUMN dimension_value VARCHAR(256),
    ADD COLUMN detail          JSONB,
    ALTER COLUMN item_id DROP NOT NULL,
    ALTER COLUMN side DROP NOT NULL,
    ALTER COLUMN ref_id DROP NOT NULL;

ALTER TABLE reconciliation_exceptions
    ADD CONSTRAINT reconciliation_exceptions_shape_check CHECK (
        currency IS NOT NULL AND detail IS NOT NULL AND (
            (kind = 'ITEM' AND item_id IS NOT NULL AND side IS NOT NULL AND ref_id IS NOT NULL
                AND dimension IS NULL AND dimension_value IS NULL)
         OR (kind = 'BALANCE' AND item_id IS NULL AND side IS NULL AND ref_id IS NULL
                AND dimension IS NOT NULL AND dimension_value IS NOT NULL)
        )
    ) NOT VALID;

-- A balance variance is raised once per run, currency and bucket, however many
-- times matching is re-run. dimension '' is the whole population.
CREATE UNIQUE INDEX uq_reconciliation_exceptions_balance
    ON reconciliation_exceptions (run_id, currency, dimension, dimension_value) WHERE kind = 'BALANCE';

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
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.dimension IS DISTINCT FROM OLD.dimension
        OR NEW.dimension_value IS DISTINCT FROM OLD.dimension_value
        OR NEW.detail IS DISTINCT FROM OLD.detail
    THEN
        RAISE EXCEPTION 'reconciliation exception % may only have status/resolved_at/resolved_by_match_id change', OLD.exception_id;
    END IF;
    NEW.resolved_at := COALESCE(NEW.resolved_at, NOW());
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── Certifications ──────────────────────────────────────────────────────────

ALTER TABLE certifications
    ADD COLUMN control_totals JSONB,
    ALTER COLUMN total_asserted_minor_units DROP NOT NULL;

ALTER TABLE certifications
    ADD CONSTRAINT certifications_per_currency_check CHECK (
        control_totals IS NOT NULL AND total_asserted_minor_units IS NULL
    ) NOT VALID;
