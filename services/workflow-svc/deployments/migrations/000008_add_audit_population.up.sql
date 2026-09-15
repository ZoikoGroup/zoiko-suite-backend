-- AUD-03 Audit Population lives beside AUD-01/AUD-02 in the same service for
-- the same reason as AUD-07: a later AUD-04 Sampling module needs to check
-- "is this population FROZEN" synchronously in the same database, not via a
-- cross-service call that would turn a correctness gate into a race.
--
-- This build does not own a source-system extraction engine. BuildPopulation
-- accepts a caller-computed item_count/control_total (the extract itself is
-- produced outside this service) and this table exists to make that extract
-- a frozen, reproducible, hash-verifiable fact — not to re-implement ETL.
CREATE TABLE audit_populations (
    population_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    engagement_id UUID NOT NULL REFERENCES audit_engagements(engagement_id),
    tenant_id UUID NOT NULL,
    source_system TEXT NOT NULL,
    source_object TEXT NOT NULL,
    filter_spec TEXT NOT NULL,
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    watermark TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'DEFINED',
    item_count BIGINT,
    control_total_amount NUMERIC(18,2),
    digest TEXT,
    -- Set only by AddControlledDelta on the newly created successor row —
    -- "late journal appears after frozen population: create controlled
    -- delta/new version; do not mutate original" (AUD-NEG-010).
    prior_population_id UUID REFERENCES audit_populations(population_id),
    -- Set only on the OLD row, by AddControlledDelta or SupersedePopulation.
    superseded_by_population_id UUID REFERENCES audit_populations(population_id),
    supersede_reason TEXT,
    quarantine_reason TEXT,
    created_by_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    frozen_at TIMESTAMPTZ,
    CONSTRAINT audit_population_status_valid CHECK (status IN ('DEFINED','BUILDING','VALIDATING','FROZEN','IN_USE','SUPERSEDED','QUARANTINED')),
    CONSTRAINT audit_population_creator_present CHECK (created_by_principal_id <> ''),
    CONSTRAINT audit_population_period_valid CHECK (period_end >= period_start),
    -- AUD-CTRL-007/009: a frozen population (or anything derived from one)
    -- always carries the counts/hash that make it reproducible.
    CONSTRAINT audit_population_frozen_has_manifest CHECK (
        status NOT IN ('FROZEN','IN_USE','SUPERSEDED') OR (item_count IS NOT NULL AND control_total_amount IS NOT NULL AND digest IS NOT NULL AND frozen_at IS NOT NULL)
    ),
    CONSTRAINT audit_population_quarantine_has_reason CHECK (status <> 'QUARANTINED' OR quarantine_reason IS NOT NULL),
    CONSTRAINT audit_population_supersede_has_reason CHECK (status <> 'SUPERSEDED' OR supersede_reason IS NOT NULL)
);
CREATE UNIQUE INDEX audit_population_create_idempotency_unique ON audit_populations (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX audit_populations_engagement ON audit_populations (engagement_id);

CREATE TABLE audit_population_transitions (
    transition_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    population_id UUID NOT NULL REFERENCES audit_populations(population_id),
    tenant_id UUID NOT NULL,
    from_status TEXT NOT NULL,
    to_status TEXT NOT NULL,
    actor_principal_id TEXT NOT NULL,
    reason TEXT,
    correlation_id TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT audit_population_transition_actor_present CHECK (actor_principal_id <> '')
);
CREATE UNIQUE INDEX audit_population_transition_idempotency_unique ON audit_population_transitions (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX audit_population_transitions_population ON audit_population_transitions (population_id, occurred_at);

ALTER TABLE audit_populations ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_populations FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_populations_tenant_isolation ON audit_populations FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE audit_population_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_population_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_population_transitions_tenant_isolation ON audit_population_transitions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE OR REPLACE FUNCTION reject_audit_population_transition_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit population transitions are append-only';
END;
$$;
CREATE TRIGGER trg_reject_audit_population_transition_mutation
    BEFORE UPDATE OR DELETE ON audit_population_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_audit_population_transition_mutation();

-- AUD-CTRL-008 "Frozen population cannot mutate after testing starts", at
-- the DB layer, not just application discipline: once a population has ever
-- been frozen, its manifest-defining fields become immutable. Only status
-- (FROZEN -> IN_USE -> SUPERSEDED / QUARANTINED) and the supersession
-- linkage/reason may still change.
CREATE OR REPLACE FUNCTION reject_frozen_population_manifest_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'audit populations are never deleted';
    END IF;
    IF OLD.frozen_at IS NOT NULL AND (
        OLD.source_system IS DISTINCT FROM NEW.source_system
        OR OLD.source_object IS DISTINCT FROM NEW.source_object
        OR OLD.filter_spec IS DISTINCT FROM NEW.filter_spec
        OR OLD.watermark IS DISTINCT FROM NEW.watermark
        OR OLD.item_count IS DISTINCT FROM NEW.item_count
        OR OLD.control_total_amount IS DISTINCT FROM NEW.control_total_amount
        OR OLD.digest IS DISTINCT FROM NEW.digest
        OR OLD.frozen_at IS DISTINCT FROM NEW.frozen_at
    ) THEN
        RAISE EXCEPTION 'frozen population manifest is immutable — use AddControlledDelta or SupersedePopulation instead';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_reject_frozen_population_manifest_mutation
    BEFORE UPDATE OR DELETE ON audit_populations
    FOR EACH ROW EXECUTE FUNCTION reject_frozen_population_manifest_mutation();
