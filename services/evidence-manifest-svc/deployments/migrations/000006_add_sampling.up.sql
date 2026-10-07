-- AUD-04 Sampling operates over an AUD-03 frozen population's own ordered
-- row set (population_rows.ordinal). See internal/domain/sampling.go's
-- own package doc for the deterministic selection algorithm and the
-- deliberate choice NOT to derive sample_size from a statistical formula.

-- Versioned, never mutated in place — a parameter change inserts a new
-- row; SelectSample pins the version it used, so a later version bump is
-- exactly how AUD-NEG-011 ("parameters changed after selection") is
-- detected downstream.
CREATE TABLE sampling_parameter_sets (
    param_set_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    approach TEXT NOT NULL CHECK (approach IN ('RANDOM','SYSTEMATIC_MUS','JUDGMENTAL')),
    tolerable_misstatement NUMERIC(20,2),
    expected_misstatement NUMERIC(20,2),
    confidence_level NUMERIC(5,2),
    key_item_threshold NUMERIC(20,2),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sample_designs (
    design_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    population_id UUID NOT NULL REFERENCES audit_populations(population_id),
    objective TEXT NOT NULL,
    param_set_id UUID NOT NULL REFERENCES sampling_parameter_sets(param_set_id),
    sample_size INTEGER NOT NULL CHECK (sample_size > 0),
    status VARCHAR(20) NOT NULL DEFAULT 'DRAFT_DESIGN'
        CHECK (status IN ('DRAFT_DESIGN','APPROVED_DESIGN','SELECTED','TESTING','EVALUATED','LOCKED','INVALIDATED')),
    approved_by_principal_id TEXT,
    created_by_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX sample_design_create_idempotency_unique ON sample_designs (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';

CREATE TABLE sample_selections (
    selection_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    design_id UUID NOT NULL REFERENCES sample_designs(design_id),
    tenant_id UUID NOT NULL,
    method TEXT NOT NULL,
    rng_seed TEXT NOT NULL,
    interval_size NUMERIC(20,4),
    start_point NUMERIC(20,4),
    param_set_id UUID NOT NULL REFERENCES sampling_parameter_sets(param_set_id),
    param_set_version INTEGER NOT NULL,
    population_digest_sha256 TEXT NOT NULL,
    selected_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sample_items (
    item_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    selection_id UUID NOT NULL REFERENCES sample_selections(selection_id),
    tenant_id UUID NOT NULL,
    population_row_id UUID NOT NULL REFERENCES population_rows(row_id),
    is_key_item BOOLEAN NOT NULL DEFAULT false,
    status VARCHAR(25) NOT NULL DEFAULT 'SELECTED'
        CHECK (status IN ('SELECTED','TESTED','NONRESPONSE','ALTERNATIVE_PROCEDURE')),
    UNIQUE (selection_id, population_row_id)
);

CREATE TABLE sample_executions (
    execution_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id UUID NOT NULL REFERENCES sample_items(item_id),
    tenant_id UUID NOT NULL,
    action VARCHAR(25) NOT NULL CHECK (action IN ('RESULT','NONRESPONSE','ALTERNATIVE_PROCEDURE')),
    objective_tested TEXT NOT NULL,
    result TEXT,
    exception_amount NUMERIC(20,2),
    actor_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX sample_execution_idempotency_unique ON sample_executions (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';

CREATE TABLE sample_evaluations (
    evaluation_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    design_id UUID NOT NULL REFERENCES sample_designs(design_id),
    tenant_id UUID NOT NULL,
    projected_misstatement NUMERIC(20,2),
    known_exception_count INTEGER NOT NULL,
    completeness_ok BOOLEAN NOT NULL,
    correlation_id TEXT,
    evaluated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX sample_evaluation_idempotency_unique ON sample_evaluations (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';

ALTER TABLE sampling_parameter_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE sampling_parameter_sets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON sampling_parameter_sets
    FOR ALL USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE sample_designs ENABLE ROW LEVEL SECURITY;
ALTER TABLE sample_designs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON sample_designs
    FOR ALL USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE sample_selections ENABLE ROW LEVEL SECURITY;
ALTER TABLE sample_selections FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON sample_selections
    FOR ALL USING (design_id IN (SELECT design_id FROM sample_designs))
    WITH CHECK (design_id IN (SELECT design_id FROM sample_designs));

ALTER TABLE sample_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE sample_items FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON sample_items
    FOR ALL USING (selection_id IN (SELECT selection_id FROM sample_selections))
    WITH CHECK (selection_id IN (SELECT selection_id FROM sample_selections));

ALTER TABLE sample_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sample_executions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON sample_executions
    FOR ALL USING (item_id IN (SELECT item_id FROM sample_items))
    WITH CHECK (item_id IN (SELECT item_id FROM sample_items));

ALTER TABLE sample_evaluations ENABLE ROW LEVEL SECURITY;
ALTER TABLE sample_evaluations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON sample_evaluations
    FOR ALL USING (design_id IN (SELECT design_id FROM sample_designs))
    WITH CHECK (design_id IN (SELECT design_id FROM sample_designs));

-- Append-only doctrine: selections, executions and evaluations are all
-- immutable facts once recorded — reuses the same append-only-trigger
-- idiom as population_rows/population_control_totals in migration 000005.
CREATE OR REPLACE FUNCTION reject_sampling_fact_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER sample_selections_immutable
    BEFORE UPDATE OR DELETE ON sample_selections
    FOR EACH ROW EXECUTE FUNCTION reject_sampling_fact_mutation();
CREATE TRIGGER sample_executions_immutable
    BEFORE UPDATE OR DELETE ON sample_executions
    FOR EACH ROW EXECUTE FUNCTION reject_sampling_fact_mutation();
CREATE TRIGGER sample_evaluations_immutable
    BEFORE UPDATE OR DELETE ON sample_evaluations
    FOR EACH ROW EXECUTE FUNCTION reject_sampling_fact_mutation();
CREATE TRIGGER sampling_parameter_sets_immutable
    BEFORE UPDATE OR DELETE ON sampling_parameter_sets
    FOR EACH ROW EXECUTE FUNCTION reject_sampling_fact_mutation();

-- "No silent item replacement": population_row_id, once set at selection
-- time, can never change — the only permitted UPDATE path for sample_items
-- is a status transition (SELECTED -> TESTED/NONRESPONSE/
-- ALTERNATIVE_PROCEDURE), never a swap to a different row.
CREATE OR REPLACE FUNCTION reject_sample_item_replacement() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.population_row_id IS DISTINCT FROM OLD.population_row_id
        OR NEW.selection_id IS DISTINCT FROM OLD.selection_id THEN
        RAISE EXCEPTION 'sample item replacement is not permitted — use RecordNonresponse/AddAlternativeProcedure instead';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_sample_item_replacement
    BEFORE UPDATE ON sample_items
    FOR EACH ROW EXECUTE FUNCTION reject_sample_item_replacement();
CREATE OR REPLACE FUNCTION reject_sample_item_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'sample items are never deleted';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_sample_item_delete
    BEFORE DELETE ON sample_items
    FOR EACH ROW EXECUTE FUNCTION reject_sample_item_delete();
