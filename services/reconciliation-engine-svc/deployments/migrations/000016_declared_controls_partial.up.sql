-- ZS-SVC-Z-001 DQC-03 (docs/architecture/DQC_GAP_MATRIX.md, step I): source-declared hash
-- total, sequence total and roll-forward, and a PARTIAL result that can be reached.
--
-- 1. A snapshot freezes what the source DECLARED beyond its totals (declared_controls) and what
--    re-performing each control concluded (control_outcomes), as evidence. 'completeness' says
--    whether the source delivered the whole in-scope population or a declared subset.
--
-- Every snapshot that exists today came from a source that declared none of this, and the
-- contract has always said a source delivers the WHOLE in-scope population. So FULL, no
-- declared controls and no outcomes ('[]': nothing was checked, which is the truth) are
-- statements of fact for the existing rows, not guesses. The defaults stay: they are exactly
-- the state of a source that declares nothing, which is a normal, supported source.
ALTER TABLE population_snapshots
    ADD COLUMN completeness      VARCHAR(8) NOT NULL DEFAULT 'FULL'
        CHECK (completeness IN ('FULL', 'PARTIAL')),
    ADD COLUMN declared_controls JSONB      NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(declared_controls) = 'object'),
    ADD COLUMN control_outcomes  JSONB      NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(control_outcomes) = 'array');

-- 'completeness' is derived from the declaration, never set on its own: it is PARTIAL exactly
-- when the source's completeness scope is PARTIAL, and a PARTIAL declaration must say which
-- subset it is.
ALTER TABLE population_snapshots
    ADD CONSTRAINT population_snapshots_completeness_matches_declaration CHECK (
        (completeness = 'PARTIAL') = (declared_controls->'completeness'->>'scope' = 'PARTIAL')
        AND (completeness <> 'PARTIAL' OR length(btrim(COALESCE(declared_controls->'completeness'->>'description', ''))) > 0)
    );

-- Cross-period continuity looks up the previous period's declared closing balance.
CREATE INDEX idx_population_snapshots_rollforward
    ON population_snapshots (tenant_id, spec_ref, side, ((declared_controls->'roll_forward'->>'period_end')))
    WHERE declared_controls ? 'roll_forward';

-- 2. PARTIAL (INV-26, NP-26). 000006 refused every run entering PARTIAL because nothing defined
--    a "defined subset". A subset is now defined as what a SOURCE declares; the database holds
--    both halves of that: a run can be PARTIAL only while a current population is a declared
--    subset, and can never be MATCH while one is. A PARTIAL run is never ELIGIBLE or CERTIFIED:
--    the existing CHECK and ValidateLifecycleTransition require MATCH for those.
CREATE OR REPLACE FUNCTION run_has_partial_population(p_run_id TEXT)
RETURNS BOOLEAN AS $$
    SELECT EXISTS (
        SELECT 1 FROM population_snapshots s
        WHERE s.run_id = p_run_id AND s.completeness = 'PARTIAL'
          AND s.generation = (SELECT MAX(m.generation) FROM population_snapshots m WHERE m.run_id = p_run_id)
    );
$$ LANGUAGE sql STABLE;

-- The lifecycle function as 000011 defined it, with the PARTIAL rule above in place of the
-- blanket refusal.
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

    -- INV-26 / DQC-03: a run whose current population is a source-declared subset can never be
    -- a full-population MATCH, however it got there.
    IF NEW.result_state = 'MATCH' AND run_has_partial_population(NEW.run_id) THEN
        RAISE EXCEPTION 'reconciliation run % cannot be MATCH: a current population is a declared subset (PARTIAL)', OLD.run_id;
    END IF;

    IF NEW.result_state <> OLD.result_state THEN
        IF NEW.result_state = 'NOT_RUN' THEN
            RAISE EXCEPTION 'reconciliation run % result cannot be reset to NOT_RUN', OLD.run_id;
        END IF;
        IF NEW.result_state = 'PARTIAL' AND NOT run_has_partial_population(NEW.run_id) THEN
            RAISE EXCEPTION 'reconciliation run % cannot enter PARTIAL: none of its current populations is a declared subset', OLD.run_id;
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
