-- Migration: 000006_run_state_dimensions.up.sql
--
-- A6  A run carries three orthogonal state dimensions (ZS-CONTROL-001 §7):
--     status (DATA-07 lifecycle), result_state (ZS-SVC-Z-001 DQC-03) and
--     certification_state (ZS-SVC-Z-001 §5). Every change to any of them is
--     recorded in the append-only reconciliation_run_transitions. Failed is
--     reachable: a run whose populations could not be proven complete is
--     recorded as Failed / INDETERMINATE with a failure code.
--
-- Existing rows are backfilled from what they already prove (status and open
-- exceptions); the backfill is itself recorded as transitions by the actor
-- 'migration:000006', so a reader can tell it from a live evaluation.

ALTER TABLE reconciliation_runs
    ADD COLUMN result_state VARCHAR(16) NOT NULL DEFAULT 'NOT_RUN'
        CHECK (result_state IN ('NOT_RUN', 'MATCH', 'VARIANCE', 'INDETERMINATE', 'PARTIAL', 'SUPERSEDED')),
    ADD COLUMN certification_state VARCHAR(16) NOT NULL DEFAULT 'UNCERTIFIED'
        CHECK (certification_state IN ('UNCERTIFIED', 'ELIGIBLE', 'CERTIFIED', 'SUPERSEDED')),
    ADD COLUMN failure_code VARCHAR(32)
        CHECK (failure_code IN ('SOURCE_UNAVAILABLE', 'SOURCE_UNREGISTERED', 'SOURCE_INCONSISTENT',
                                'POPULATION_TOO_LARGE', 'POPULATION_INVALID')),
    ADD COLUMN failure_reason TEXT;

CREATE TABLE IF NOT EXISTS reconciliation_run_transitions (
    transition_id BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id     VARCHAR(64)  NOT NULL,
    run_id        TEXT         NOT NULL REFERENCES reconciliation_runs(run_id),
    dimension     VARCHAR(16)  NOT NULL CHECK (dimension IN ('LIFECYCLE', 'RESULT', 'CERTIFICATION')),
    from_state    VARCHAR(32)  NOT NULL,
    to_state      VARCHAR(32)  NOT NULL,
    reason        TEXT         NOT NULL,
    actor         VARCHAR(255) NOT NULL,
    occurred_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (from_state <> to_state)
);
CREATE INDEX IF NOT EXISTS idx_reconciliation_run_transitions_run
    ON reconciliation_run_transitions (tenant_id, run_id, transition_id);

ALTER TABLE reconciliation_run_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_run_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY reconciliation_run_transitions_tenant_isolation ON reconciliation_run_transitions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- History is never rewritten.
CREATE TRIGGER trigger_run_transitions_immutable
    BEFORE UPDATE OR DELETE ON reconciliation_run_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

-- ── Backfill ────────────────────────────────────────────────────────────────

-- No code path could set Failed before this migration, so a Failed row was
-- written by hand and cannot be given a truthful failure code. Refuse rather
-- than invent one; an operator must investigate it first.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_runs WHERE status = 'Failed') THEN
        RAISE EXCEPTION 'cannot apply 000006: Failed runs exist that no code path could have produced; investigate them first';
    END IF;
END $$;

ALTER TABLE reconciliation_runs DISABLE TRIGGER trigger_run_lifecycle;

UPDATE reconciliation_runs r SET
    result_state = CASE
        WHEN r.status = 'Planned' THEN 'NOT_RUN'
        WHEN r.status = 'Certified' THEN 'MATCH'
        WHEN r.status = 'Superseded' THEN 'SUPERSEDED'
        WHEN r.status = 'ExceptionsOpen' THEN 'VARIANCE'
        WHEN EXISTS (SELECT 1 FROM reconciliation_exceptions e WHERE e.run_id = r.run_id AND e.status = 'Open')
            THEN 'VARIANCE'
        ELSE 'MATCH'
    END,
    certification_state = CASE
        WHEN r.status = 'Certified' THEN 'CERTIFIED'
        WHEN r.status = 'Superseded' AND r.certified_at IS NOT NULL THEN 'SUPERSEDED'
        WHEN r.status IN ('Running', 'Reperformed')
             AND NOT EXISTS (SELECT 1 FROM reconciliation_exceptions e WHERE e.run_id = r.run_id AND e.status = 'Open')
            THEN 'ELIGIBLE'
        ELSE 'UNCERTIFIED'
    END;

ALTER TABLE reconciliation_runs ENABLE TRIGGER trigger_run_lifecycle;

INSERT INTO reconciliation_run_transitions (tenant_id, run_id, dimension, from_state, to_state, reason, actor)
SELECT tenant_id, run_id, 'RESULT', 'NOT_RUN', result_state,
       'backfilled by migration 000006 from status ' || status || ' and open exceptions', 'migration:000006'
FROM reconciliation_runs WHERE result_state <> 'NOT_RUN';
INSERT INTO reconciliation_run_transitions (tenant_id, run_id, dimension, from_state, to_state, reason, actor)
SELECT tenant_id, run_id, 'CERTIFICATION', 'UNCERTIFIED', certification_state,
       'backfilled by migration 000006 from status ' || status || ' and open exceptions', 'migration:000006'
FROM reconciliation_runs WHERE certification_state <> 'UNCERTIFIED';

-- ── Cross-dimension invariants ──────────────────────────────────────────────
-- Valid for every row: the backfill above satisfies them.

ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_state_dimensions_check CHECK (
        -- Certified <=> certification CERTIFIED; ELIGIBLE/CERTIFIED only over a MATCH (INV-26).
        ((status = 'Certified') = (certification_state = 'CERTIFIED'))
        AND (certification_state NOT IN ('ELIGIBLE', 'CERTIFIED') OR result_state = 'MATCH')
        -- Superseded <=> result SUPERSEDED; certification SUPERSEDED only on a superseded run.
        AND ((status = 'Superseded') = (result_state = 'SUPERSEDED'))
        AND (certification_state <> 'SUPERSEDED' OR status = 'Superseded')
        -- No result before execution.
        AND (status <> 'Planned' OR (result_state = 'NOT_RUN' AND certification_state = 'UNCERTIFIED'))
        AND (status <> 'ExceptionsOpen' OR result_state = 'VARIANCE')
        -- A failed run never carries a MATCH (INV-05, scenario 09) and says why it failed.
        AND (status <> 'Failed' OR (result_state = 'INDETERMINATE' AND failure_code IS NOT NULL))
        AND ((failure_code IS NULL) = (failure_reason IS NULL))
        AND (failure_code IS NULL OR status IN ('Failed', 'Superseded'))
    );

-- ── Lifecycle trigger ───────────────────────────────────────────────────────

-- A run is always created Planned, with no result, uncertified and unfailed;
-- every later state is reached through an UPDATE this trigger governs.
CREATE OR REPLACE FUNCTION enforce_run_creation()
RETURNS trigger AS $$
BEGIN
    IF NEW.status <> 'Planned' OR NEW.result_state <> 'NOT_RUN' OR NEW.certification_state <> 'UNCERTIFIED'
        OR NEW.failure_code IS NOT NULL OR NEW.failure_reason IS NOT NULL THEN
        RAISE EXCEPTION 'reconciliation runs are created Planned / NOT_RUN / UNCERTIFIED';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_run_creation
    BEFORE INSERT ON reconciliation_runs
    FOR EACH ROW EXECUTE FUNCTION enforce_run_creation();

-- 000005's rules, plus: Planned may fail; the result and certification follow
-- their own edges; failure details are written once, on entering Failed; a
-- terminal run's only move is supersession, which marks its result (and a
-- certification) SUPERSEDED without touching any other column.
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
