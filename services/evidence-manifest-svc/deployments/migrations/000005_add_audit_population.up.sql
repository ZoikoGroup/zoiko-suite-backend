-- AUD-03 Audit Population — a genuinely separate schema from
-- evidence_manifests/manifest_records (see internal/domain/population.go's
-- own package doc for why). No engagement/audit_engagements table exists in
-- THIS service (that lives in workflow-svc) — engagement_id here is an
-- opaque cross-service reference, not a foreign key.

CREATE TABLE audit_populations (
    population_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    engagement_id UUID NOT NULL,
    tenant_id UUID NOT NULL,
    legal_entity_id UUID NOT NULL,
    object_class TEXT NOT NULL,
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    source_system TEXT NOT NULL,
    source_query TEXT NOT NULL,
    source_watermark TEXT NOT NULL,
    assertion TEXT NOT NULL,
    expected_completeness_check TEXT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'DEFINED'
        CHECK (status IN ('DEFINED','BUILDING','VALIDATING','FROZEN','IN_USE','SUPERSEDED','QUARANTINED')),
    row_count BIGINT,
    digest_sha256 VARCHAR(64),
    prior_population_id UUID REFERENCES audit_populations(population_id),
    superseded_by_population_id UUID REFERENCES audit_populations(population_id),
    quarantine_reason TEXT,
    created_by_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    frozen_at TIMESTAMPTZ,
    CONSTRAINT audit_population_period_valid CHECK (period_end >= period_start),
    -- "Counts/control totals/hashes required": a FROZEN or IN_USE
    -- population must carry both — the DB-level half of AUD-CTRL-007,
    -- backed by FreezePopulation's own CAS predicate in the store layer.
    CONSTRAINT audit_population_frozen_requires_digest CHECK (
        status NOT IN ('FROZEN','IN_USE') OR (digest_sha256 IS NOT NULL AND row_count IS NOT NULL)
    )
);
CREATE INDEX idx_audit_populations_engagement ON audit_populations (tenant_id, engagement_id);
CREATE UNIQUE INDEX audit_population_create_idempotency_unique ON audit_populations (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';

CREATE TABLE population_control_totals (
    control_total_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    population_id UUID NOT NULL REFERENCES audit_populations(population_id),
    measure_name TEXT NOT NULL,
    source_value NUMERIC(20,2) NOT NULL,
    computed_value NUMERIC(20,2) NOT NULL,
    reconciled BOOLEAN NOT NULL GENERATED ALWAYS AS (source_value = computed_value) STORED,
    UNIQUE (population_id, measure_name)
);

CREATE TABLE population_rows (
    row_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    population_id UUID NOT NULL REFERENCES audit_populations(population_id),
    ordinal BIGINT NOT NULL,
    source_record_id TEXT NOT NULL,
    amount NUMERIC(20,2),
    row_snapshot JSONB NOT NULL,
    UNIQUE (population_id, ordinal)
);
CREATE INDEX idx_population_rows_population_ordinal ON population_rows (population_id, ordinal);

CREATE TABLE population_deltas (
    delta_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    population_id UUID NOT NULL REFERENCES audit_populations(population_id),
    reason TEXT NOT NULL,
    new_population_id UUID NOT NULL REFERENCES audit_populations(population_id),
    created_by_principal_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE audit_populations ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_populations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON audit_populations
    FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE population_control_totals ENABLE ROW LEVEL SECURITY;
ALTER TABLE population_control_totals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON population_control_totals
    FOR ALL
    USING (population_id IN (SELECT population_id FROM audit_populations))
    WITH CHECK (population_id IN (SELECT population_id FROM audit_populations));

ALTER TABLE population_rows ENABLE ROW LEVEL SECURITY;
ALTER TABLE population_rows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON population_rows
    FOR ALL
    USING (population_id IN (SELECT population_id FROM audit_populations))
    WITH CHECK (population_id IN (SELECT population_id FROM audit_populations));

ALTER TABLE population_deltas ENABLE ROW LEVEL SECURITY;
ALTER TABLE population_deltas FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON population_deltas
    FOR ALL
    USING (population_id IN (SELECT population_id FROM audit_populations))
    WITH CHECK (population_id IN (SELECT population_id FROM audit_populations));

-- Frozen immutability: "frozen population immutable" means the population's
-- own CONTENT (spec fields, digest, row_count) can never change again once
-- FROZEN — not that status itself may never move. Status legitimately
-- progresses FROZEN -> IN_USE -> SUPERSEDED (or -> QUARANTINED), each via
-- this store's own controlled methods, but every other column is locked
-- the moment status first reaches FROZEN or IN_USE. SUPERSEDED/QUARANTINED
-- are fully terminal: no further UPDATE of any kind reaches those rows.
CREATE OR REPLACE FUNCTION reject_frozen_population_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('SUPERSEDED','QUARANTINED') THEN
        RAISE EXCEPTION 'population % is terminal: mutation rejected', OLD.population_id;
    END IF;
    IF OLD.status IN ('FROZEN','IN_USE') THEN
        IF NEW.object_class IS DISTINCT FROM OLD.object_class
            OR NEW.source_system IS DISTINCT FROM OLD.source_system
            OR NEW.source_query IS DISTINCT FROM OLD.source_query
            OR NEW.source_watermark IS DISTINCT FROM OLD.source_watermark
            OR NEW.digest_sha256 IS DISTINCT FROM OLD.digest_sha256
            OR NEW.row_count IS DISTINCT FROM OLD.row_count
            OR NEW.period_start IS DISTINCT FROM OLD.period_start
            OR NEW.period_end IS DISTINCT FROM OLD.period_end THEN
            RAISE EXCEPTION 'population % content is immutable once frozen', OLD.population_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_frozen_population_mutation
    BEFORE UPDATE ON audit_populations
    FOR EACH ROW EXECUTE FUNCTION reject_frozen_population_mutation();

-- population_rows/population_control_totals get their own append-only
-- trigger — migration 000002's reject_mutation() hardcodes
-- OLD.manifest_record_id in its error message, so it cannot be reused
-- verbatim against a table with a different primary key column. Same
-- append-only doctrine, same enforcement idiom, a separate function.
CREATE OR REPLACE FUNCTION reject_population_child_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER population_rows_immutable
    BEFORE UPDATE OR DELETE ON population_rows
    FOR EACH ROW EXECUTE FUNCTION reject_population_child_mutation();
CREATE TRIGGER population_control_totals_immutable
    BEFORE UPDATE OR DELETE ON population_control_totals
    FOR EACH ROW EXECUTE FUNCTION reject_population_child_mutation();
